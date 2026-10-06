// Package web 提供本地 Web 修改器界面：SWF 数值补丁工作台与 .sol 存档编辑器。
package web

import (
	"bytes"
	"compress/gzip"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"swfkit/internal/abc"
	"swfkit/internal/patch"
	"swfkit/internal/sol"
	"swfkit/internal/swf"
)

//go:embed static
var staticFS embed.FS

// session 为一次上传的会话状态。
type session struct {
	kind     string // "swf" | "sol"
	name     string
	raw      []byte   // 原始文件
	patched  []byte   // 补丁后文件（swf）
	history  [][]byte // 每次补丁前的状态（undo 栈）
	rev      int      // 补丁版本号（Ruffle 重载时作缓存击穿参数）
	doc      *sol.Doc
	hits     []*patch.Hit
	lastUsed time.Time
}

type store struct {
	mu  sync.Mutex
	seq int
	m   map[string]*session
}

func newStore() *store {
	s := &store{m: map[string]*session{}}
	go s.gcLoop()
	return s
}

func (s *store) gcLoop() {
	for range time.Tick(10 * time.Minute) {
		s.mu.Lock()
		for id, sess := range s.m {
			if time.Since(sess.lastUsed) > 30*time.Minute {
				delete(s.m, id)
			}
		}
		s.mu.Unlock()
	}
}

func (s *store) put(sess *session) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	id := fmt.Sprintf("s%d", s.seq)
	s.m[id] = sess
	return id
}

func (s *store) get(id string) (*session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.m[id]
	if ok {
		sess.lastUsed = time.Now()
	}
	return sess, ok
}

// Serve 启动 Web 服务。
func Serve(addr string) error {
	st := newStore()
	mux := http.NewServeMux()

	mux.HandleFunc("POST /api/swf", func(w http.ResponseWriter, r *http.Request) { uploadSWF(st, w, r) })
	mux.HandleFunc("POST /api/swf/{id}/scan", func(w http.ResponseWriter, r *http.Request) { scanSWF(st, w, r) })
	mux.HandleFunc("POST /api/swf/{id}/patch", func(w http.ResponseWriter, r *http.Request) { patchSWF(st, w, r) })
	mux.HandleFunc("POST /api/swf/{id}/undo", func(w http.ResponseWriter, r *http.Request) { undoSWF(st, w, r) })
	mux.HandleFunc("GET /api/swf/{id}/raw", func(w http.ResponseWriter, r *http.Request) { rawSWF(st, w, r) })
	mux.HandleFunc("GET /api/swf/{id}/download", func(w http.ResponseWriter, r *http.Request) { downloadSWF(st, w, r) })

	mux.HandleFunc("POST /api/sol", func(w http.ResponseWriter, r *http.Request) { uploadSOL(st, w, r) })
	mux.HandleFunc("GET /api/sol/{id}", func(w http.ResponseWriter, r *http.Request) { getSOL(st, w, r) })
	mux.HandleFunc("POST /api/sol/{id}/set", func(w http.ResponseWriter, r *http.Request) { setSOL(st, w, r) })
	mux.HandleFunc("POST /api/sol/{id}/get", func(w http.ResponseWriter, r *http.Request) { getSOLValue(st, w, r) })
	mux.HandleFunc("GET /api/sol/{id}/download", func(w http.ResponseWriter, r *http.Request) { downloadSOL(st, w, r) })

	// 运行时模式：从本地路径载入游戏（服务端把游戏目录挂为静态根，
	// 游戏的相对资源加载如 game/xxx.swf 可正常解析）
	rtDirs := newRTDirs()
	lib := newSWFLibrary()
	go lib.rescanDirs() // 启动时后台重建已知目录的索引
	mux.HandleFunc("POST /api/rt/open", func(w http.ResponseWriter, r *http.Request) { rtOpen(rtDirs, lib, w, r) })
	mux.HandleFunc("POST /api/rt/byname", func(w http.ResponseWriter, r *http.Request) { rtByName(rtDirs, lib, w, r) })
	mux.HandleFunc("POST /api/library/scan", func(w http.ResponseWriter, r *http.Request) { libraryScan(lib, w, r) })

	// 修改表：按游戏内容哈希持久化/恢复清单
	mux.HandleFunc("POST /api/table/save", tableSave)
	mux.HandleFunc("GET /api/table/load/{key}", tableLoad)
	mux.HandleFunc("DELETE /api/table/{key}", tableDelete)
	mux.HandleFunc("POST /api/abc/strings", abcStrings)
	mux.HandleFunc("GET /api/rt/base/{id}/", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		dir, ok := rtDirs.get(id)
		if !ok {
			http.NotFound(w, r)
			return
		}
		// http.Dir 内部拒绝 .. 穿越等非法路径；包一层统计供进度条使用
		prefix := "/api/rt/base/" + id + "/"
		st := rtDirs.statsFor(id)
		sw := &sizeWriter{ResponseWriter: w}
		http.StripPrefix(prefix, http.FileServer(http.Dir(dir))).ServeHTTP(sw, r)
		st.Requests.Add(1)
		st.Bytes.Add(int64(sw.written))
	})
	mux.HandleFunc("GET /api/rt/progress/{id}", func(w http.ResponseWriter, r *http.Request) {
		bytes, reqs, ok := rtDirs.progress(r.PathValue("id"))
		if !ok {
			writeErr(w, http.StatusNotFound, "会话不存在")
			return
		}
		writeOK(w, map[string]any{"bytes": bytes, "requests": reqs})
	})

	// 内嵌 Ruffle（gzip 预压缩，本地分发，离线可用、秒级加载）
	mux.HandleFunc("GET /vendor/ruffle/{file}", serveRuffle)

	// 静态资源去掉 static/ 前缀，使 / 直接指向首页；
	// no-cache 强制按 ETag 协商缓存，避免升级后浏览器拿旧版页面/脚本
	staticSub, _ := fs.Sub(staticFS, "static")
	mux.Handle("GET /", noCache(http.FileServer(http.FS(staticSub))))

	log.Printf("swfkit Web 修改器已启动: http://%s", addr)
	return http.ListenAndServe(addr, mux)
}

// noCache 为静态资源加协商缓存头。
func noCache(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		next.ServeHTTP(w, r)
	})
}

// ---- 内嵌 Ruffle 分发 ----

// ruffleFiles 为允许分发的文件名白名单（gzip 预压缩形态存于 embed）。
var ruffleFiles = map[string]string{
	"ruffle.js":                           "text/javascript; charset=utf-8",
	"core.ruffle.c80159b526e567babaf5.js": "text/javascript; charset=utf-8",
	"core.ruffle.f000070ea72f8ae4fe3a.js": "text/javascript; charset=utf-8",
	"72a20ef1c0b8ceb37720.wasm":           "application/wasm",
	"826bb0938097485a2c9d.wasm":           "application/wasm",
}

// serveRuffle 分发内嵌的 Ruffle 自托管文件（static/vendor/ruffle/*.gz）。
// 客户端支持 gzip 时直接透传预压缩字节（零 CPU 开销）；
// 否则现场解压兜底。no-cache 协商缓存，版本随二进制升级即失效。
func serveRuffle(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	ctype, ok := ruffleFiles[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	raw, err := fs.ReadFile(staticFS, "static/vendor/ruffle/"+name+".gz")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Cache-Control", "no-cache")
	if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Length", strconv.Itoa(len(raw)))
		_, _ = w.Write(raw)
		return
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "解压失败")
		return
	}
	defer zr.Close()
	_, _ = io.Copy(w, zr)
}

// ---- 运行时：本地路径载入 ----

// rtDirs 管理"会话 id → 游戏目录"映射与传输统计。
type rtDirs struct {
	mu    sync.Mutex
	seq   int
	dirs  map[string]string
	stats map[string]*rtStats
}

// rtStats 为单个会话的资源传输统计（进度条数据源）。
type rtStats struct {
	Requests atomic.Int64
	Bytes    atomic.Int64
}

func newRTDirs() *rtDirs {
	return &rtDirs{dirs: map[string]string{}, stats: map[string]*rtStats{}}
}

func (d *rtDirs) add(dir string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.seq++
	id := fmt.Sprintf("g%d", d.seq)
	d.dirs[id] = dir
	d.stats[id] = &rtStats{}
	return id
}

func (d *rtDirs) get(id string) (string, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	dir, ok := d.dirs[id]
	return dir, ok
}

// statsFor 返回会话的传输统计（无则创建）。
func (d *rtDirs) statsFor(id string) *rtStats {
	d.mu.Lock()
	defer d.mu.Unlock()
	st, ok := d.stats[id]
	if !ok {
		st = &rtStats{}
		d.stats[id] = st
	}
	return st
}

// progress 返回会话的已传输字节数与请求次数。
func (d *rtDirs) progress(id string) (int64, int64, bool) {
	d.mu.Lock()
	_, ok := d.dirs[id]
	d.mu.Unlock()
	if !ok {
		return 0, 0, false
	}
	return d.statsFor(id).Bytes.Load(), d.statsFor(id).Requests.Load(), true
}

// sizeWriter 捕获响应字节数。
type sizeWriter struct {
	http.ResponseWriter
	written int64
}

func (w *sizeWriter) Write(b []byte) (int, error) {
	n, err := w.ResponseWriter.Write(b)
	w.written += int64(n)
	return n, err
}

// rtOpen 打开本地 SWF 或游戏目录：登记其所在目录为静态根，返回可交给 Ruffle 的基础 URL。
// 目录模式下自动挑选入口 SWF（常见入口名优先）。同时把入口文件与同目录的其他 SWF
// 登记进持久化索引（供文件选择/拖拽时智能定位）。
func rtOpen(d *rtDirs, lib *swfLibrary, w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Path == "" {
		writeErr(w, http.StatusBadRequest, "请求体非法")
		return
	}
	// 容错：去掉复制粘贴带上的引号，展开 ~
	p := strings.Trim(strings.TrimSpace(req.Path), `"'`)
	p = expandHome(p)
	abs, err := filepath.Abs(p)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "路径非法")
		return
	}
	info, err := os.Stat(abs)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "路径不存在: "+req.Path)
		return
	}
	fromDir := false
	if info.IsDir() {
		fromDir = true
		main, err := pickMainSWF(abs)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "目录中没有 .swf 文件: "+req.Path)
			return
		}
		abs = main
		if info, err = os.Stat(abs); err != nil {
			writeErr(w, http.StatusBadRequest, "入口文件不可读: "+abs)
			return
		}
	}
	if !strings.EqualFold(filepath.Ext(abs), ".swf") {
		writeErr(w, http.StatusBadRequest, "仅支持 .swf 文件或游戏目录")
		return
	}
	id := d.add(filepath.Dir(abs))
	lib.add(abs, info.Size())
	lib.addDir(filepath.Dir(abs)) // 同目录 SWF 一并入索引（非递归，开销小）
	name := filepath.Base(abs)
	resp := map[string]any{
		"id":      id,
		"name":    name,
		"path":    abs,
		"size":    info.Size(),
		"playUrl": "/api/rt/base/" + id + "/" + name,
	}
	if fromDir {
		resp["dir"] = true
	}
	writeOK(w, resp)
}

// pickMainSWF 在游戏目录里挑选入口 SWF：常见入口文件名优先（root/main/loader…），
// 其次目录下唯一的 .swf，最后兜底取体积最大者（可能是资源模块，前端会提示可改粘贴具体文件）。
func pickMainSWF(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	byStem := map[string]string{}
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.EqualFold(filepath.Ext(e.Name()), ".swf") {
			continue
		}
		stem := strings.ToLower(strings.TrimSuffix(e.Name(), filepath.Ext(e.Name())))
		byStem[stem] = e.Name()
		names = append(names, e.Name())
	}
	if len(names) == 0 {
		return "", fs.ErrNotExist
	}
	for _, stem := range []string{"main", "root", "loader", "loading", "preload", "preloader", "boot", "start", "index", "game", "shell"} {
		if name, ok := byStem[stem]; ok {
			return filepath.Join(dir, name), nil
		}
	}
	if len(names) == 1 {
		return filepath.Join(dir, names[0]), nil
	}
	best, bestSize := "", int64(-1)
	for _, name := range names {
		if fi, err := os.Stat(filepath.Join(dir, name)); err == nil && fi.Size() > bestSize {
			best, bestSize = name, fi.Size()
		}
	}
	return filepath.Join(dir, best), nil
}

// expandHome 展开路径开头的 ~ 为用户主目录（GUI 粘贴不走 shell）。
func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

// rtByName 按文件名（+大小校验）从持久化索引定位本地 SWF：
// 文件选择/拖拽无法获得本地路径，借此实现"选了就能完整运行"的智能降级。
func rtByName(d *rtDirs, lib *swfLibrary, w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
		Size int64  `json:"size"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
		writeErr(w, http.StatusBadRequest, "请求体非法")
		return
	}
	path, ok := lib.find(req.Name, req.Size)
	if !ok {
		writeOK(w, map[string]any{"found": false})
		return
	}
	id := d.add(filepath.Dir(path))
	writeOK(w, map[string]any{
		"found":   true,
		"path":    path,
		"id":      id,
		"name":    filepath.Base(path),
		"size":    req.Size,
		"playUrl": "/api/rt/base/" + id + "/" + filepath.Base(path),
	})
}

// libraryScan 递归扫描游戏库根目录，把全部 SWF 登记进索引。
func libraryScan(lib *swfLibrary, w http.ResponseWriter, r *http.Request) {
	var req struct {
		Root string `json:"root"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Root == "" {
		writeErr(w, http.StatusBadRequest, "请求体非法")
		return
	}
	n, err := lib.scanRoot(req.Root)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "扫描失败: "+err.Error())
		return
	}
	writeOK(w, map[string]any{"indexed": n})
}

// ---- 通用辅助 ----

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func writeOK(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}

// ---- SWF 工作台 ----

func uploadSWF(st *store, w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 200<<20))
	if err != nil || len(raw) == 0 {
		writeErr(w, http.StatusBadRequest, "读取上传内容失败")
		return
	}
	f, err := swf.Parse(raw)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	sess := &session{kind: "swf", name: fileBase(r), raw: raw, lastUsed: time.Now()}
	info := swfInfo(f)
	sess.patched = nil
	id := st.put(sess)
	writeOK(w, map[string]any{"id": id, "name": sess.name, "info": info})
}

func fileBase(r *http.Request) string {
	name := r.Header.Get("X-Filename")
	if name == "" {
		name = "未命名.swf"
	}
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		name = name[i+1:]
	}
	return name
}

func swfInfo(f *swf.File) map[string]any {
	mods := []map[string]any{}
	for i, t := range f.Tags {
		if !t.IsDoABC() {
			continue
		}
		ta, err := abc.ParseTag(t, i)
		if err != nil {
			mods = append(mods, map[string]any{"index": i, "error": err.Error()})
			continue
		}
		mods = append(mods, map[string]any{
			"index": i, "name": ta.Name,
			"methods": len(ta.File.Methods), "classes": len(ta.File.Instances),
			"scripts": len(ta.File.Scripts), "bodies": len(ta.File.Bodies),
		})
	}
	return map[string]any{
		"summary": f.String(),
		"modules": mods,
	}
}

func scanSWF(st *store, w http.ResponseWriter, r *http.Request) {
	sess, ok := st.get(r.PathValue("id"))
	if !ok || sess.kind != "swf" {
		writeErr(w, http.StatusNotFound, "会话不存在")
		return
	}
	var req struct {
		Mode   string `json:"mode"`
		Value  any    `json:"value"`
		Refine bool   `json:"refine"` // 在既有命中范围内二次扫描（类 CE "next scan"）
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体非法")
		return
	}
	var opts patch.ScanOpts
	switch req.Mode {
	case "int":
		opts.Mode = patch.ScanInt
	case "uint":
		opts.Mode = patch.ScanUInt
	case "double":
		opts.Mode = patch.ScanDouble
	default:
		opts.Mode = patch.ScanAny
	}
	iv, fv := parseScanValue(req.Value)
	opts.IntValue, opts.DoubleValue = iv, fv
	f, err := swf.Parse(sess.current())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	hits, err := patch.Scan(f, opts)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Refine {
		old := map[string]bool{}
		for _, h := range sess.hits {
			old[hitKey(h)] = true
		}
		var narrowed []*patch.Hit
		for _, h := range hits {
			if old[hitKey(h)] {
				narrowed = append(narrowed, h)
			}
		}
		hits = narrowed
	}
	sess.hits = hits
	type hitView struct {
		Index  int      `json:"index"`
		Source string   `json:"source"`
		Value  string   `json:"value"`
		Where  string   `json:"where"`
		Hints  []string `json:"hints"`
	}
	var views []hitView
	for i, h := range hits {
		val := fmt.Sprintf("%d", h.IntVal)
		if h.IsFloat {
			val = strconv.FormatFloat(h.FloatVal, 'g', -1, 64)
		}
		views = append(views, hitView{Index: i + 1, Source: h.Source, Value: val, Where: h.Where, Hints: h.Hints})
	}
	writeOK(w, map[string]any{"hits": views, "total": len(views), "refined": req.Refine})
}

// hitKey 为命中的稳定坐标（补丁只重编码指令、不增删指令数，坐标跨补丁稳定）。
func hitKey(h *patch.Hit) string {
	return fmt.Sprintf("%d:%d:%d", h.ABCIdx, h.BodyIdx, h.InsnIdx)
}

func patchSWF(st *store, w http.ResponseWriter, r *http.Request) {
	sess, ok := st.get(r.PathValue("id"))
	if !ok || sess.kind != "swf" {
		writeErr(w, http.StatusNotFound, "会话不存在")
		return
	}
	var req struct {
		Indices []int `json:"indices"` // 1 起始
		Value   any   `json:"value"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Indices) == 0 {
		writeErr(w, http.StatusBadRequest, "请求体非法或未选择命中")
		return
	}
	newInt, newDouble := parseScanValue(req.Value)
	var ops []*patch.Op
	for _, idx := range req.Indices {
		if idx < 1 || idx > len(sess.hits) {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("序号 %d 超出范围", idx))
			return
		}
		ops = append(ops, &patch.Op{Hit: sess.hits[idx-1], NewInt: newInt, NewDouble: newDouble})
	}
	f, err := swf.Parse(sess.current())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := patch.Apply(f, ops); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	data, err := f.Save()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	sess.history = append(sess.history, sess.current())
	sess.patched = data
	sess.rev++
	writeOK(w, map[string]any{
		"patched":  len(ops),
		"rev":      sess.rev,
		"play":     fmt.Sprintf("/api/swf/%s/raw?rev=%d", r.PathValue("id"), sess.rev),
		"download": "/api/swf/" + r.PathValue("id") + "/download",
	})
}

// undoSWF 回滚到上一次补丁前的状态。
func undoSWF(st *store, w http.ResponseWriter, r *http.Request) {
	sess, ok := st.get(r.PathValue("id"))
	if !ok || sess.kind != "swf" {
		writeErr(w, http.StatusNotFound, "会话不存在")
		return
	}
	if len(sess.history) == 0 {
		writeErr(w, http.StatusBadRequest, "没有可回滚的补丁")
		return
	}
	prev := sess.history[len(sess.history)-1]
	sess.history = sess.history[:len(sess.history)-1]
	if bytes.Equal(prev, sess.raw) {
		sess.patched = nil
	} else {
		sess.patched = prev
	}
	sess.hits = nil
	sess.rev++
	writeOK(w, map[string]any{"rev": sess.rev, "play": fmt.Sprintf("/api/swf/%s/raw?rev=%d", r.PathValue("id"), sess.rev)})
}

// rawSWF 以内联方式返回当前文件（供内嵌 Ruffle 播放器加载）。
func rawSWF(st *store, w http.ResponseWriter, r *http.Request) {
	sess, ok := st.get(r.PathValue("id"))
	if !ok || sess.kind != "swf" {
		writeErr(w, http.StatusNotFound, "会话不存在")
		return
	}
	w.Header().Set("Content-Type", "application/x-shockwave-flash")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(sess.current())
}

func downloadSWF(st *store, w http.ResponseWriter, r *http.Request) {
	sess, ok := st.get(r.PathValue("id"))
	if !ok || sess.kind != "swf" {
		writeErr(w, http.StatusNotFound, "会话不存在")
		return
	}
	data := sess.current()
	name := strings.TrimSuffix(sess.name, ".swf") + ".patched.swf"
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	_, _ = w.Write(data)
}

// ---- SOL 编辑器 ----

func uploadSOL(st *store, w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 50<<20))
	if err != nil || len(raw) == 0 {
		writeErr(w, http.StatusBadRequest, "读取上传内容失败")
		return
	}
	doc, err := sol.Parse(raw)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	sess := &session{kind: "sol", name: fileBase(r), raw: raw, doc: doc, lastUsed: time.Now()}
	id := st.put(sess)
	writeOK(w, map[string]any{"id": id, "name": doc.Name})
}

func getSOL(st *store, w http.ResponseWriter, r *http.Request) {
	sess, ok := st.get(r.PathValue("id"))
	if !ok || sess.kind != "sol" {
		writeErr(w, http.StatusNotFound, "会话不存在")
		return
	}
	j, err := sess.doc.MarshalJSON()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeOK(w, json.RawMessage(j))
}

func setSOL(st *store, w http.ResponseWriter, r *http.Request) {
	sess, ok := st.get(r.PathValue("id"))
	if !ok || sess.kind != "sol" {
		writeErr(w, http.StatusNotFound, "会话不存在")
		return
	}
	var req struct {
		Path  string `json:"path"`
		Value any    `json:"value"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Path == "" {
		writeErr(w, http.StatusBadRequest, "请求体非法")
		return
	}
	if err := sess.doc.Set(req.Path, normalizeSOLValue(req.Value)); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	j, err := sess.doc.MarshalJSON()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeOK(w, json.RawMessage(j))
}

// normalizeSOLValue 把前端 JSON 值收敛为 AMF 标量（数字统一 float64）。
func normalizeSOLValue(v any) any {
	switch x := v.(type) {
	case float64:
		return x
	case string:
		if sv, err := sol.ParseScalar(x); err == nil {
			return sv
		}
		return x
	case bool, nil:
		return v
	default:
		return v
	}
}

// getSOLValue 按点分路径读取单个值（控制台 get 命令用）。
func getSOLValue(st *store, w http.ResponseWriter, r *http.Request) {
	sess, ok := st.get(r.PathValue("id"))
	if !ok || sess.kind != "sol" {
		writeErr(w, http.StatusNotFound, "会话不存在")
		return
	}
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Path == "" {
		writeErr(w, http.StatusBadRequest, "请求体非法")
		return
	}
	v, found := sess.doc.Get(req.Path)
	if !found {
		writeOK(w, map[string]any{"found": false})
		return
	}
	j, err := sol.MarshalValue(v)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeOK(w, map[string]any{"found": true, "path": req.Path, "value": json.RawMessage(j)})
}

func downloadSOL(st *store, w http.ResponseWriter, r *http.Request) {
	sess, ok := st.get(r.PathValue("id"))
	if !ok || sess.kind != "sol" {
		writeErr(w, http.StatusNotFound, "会话不存在")
		return
	}
	data, err := sess.doc.Serialize()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	name := strings.TrimSuffix(sess.name, ".sol") + ".patched.sol"
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	_, _ = w.Write(data)
}

// ---- 内部辅助 ----

func parseScanValue(v any) (int64, float64) {
	switch x := v.(type) {
	case float64:
		return int64(x), x
	case string:
		s := strings.TrimSpace(x)
		if iv, err := strconv.ParseInt(s, 0, 64); err == nil {
			return iv, float64(iv)
		}
		if fv, err := strconv.ParseFloat(s, 64); err == nil {
			return int64(fv), fv
		}
	}
	return 0, 0
}

// current 返回最新文件内容（有补丁用补丁版）。
func (s *session) current() []byte {
	if s.patched != nil {
		return s.patched
	}
	return s.raw
}

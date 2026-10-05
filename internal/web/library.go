package web

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// swfLibrary 为"文件名 → 本地路径"持久化索引：
// 让文件选择/拖拽的 SWF 能自动定位到磁盘真实位置（同名 + 同大小校验），
// 从而以路径模式载入、完整支持游戏外挂资源。
type swfLibrary struct {
	mu    sync.Mutex
	Files map[string][]libEntry `json:"files"`
	Dirs  []string              `json:"dirs"` // 曾打开过的游戏目录（用于启动时重建索引）
	file  string                // 持久化路径
}

type libEntry struct {
	Path string    `json:"path"`
	Size int64     `json:"size"`
	At   time.Time `json:"at"`
}

func newSWFLibrary() *swfLibrary {
	l := &swfLibrary{Files: map[string][]libEntry{}}
	l.file = libraryPath()
	l.load()
	return l
}

func libraryPath() string {
	base, err := os.UserConfigDir()
	if err != nil || base == "" {
		base = os.TempDir()
	}
	return filepath.Join(base, "swfkit", "library.json")
}

func (l *swfLibrary) load() {
	l.mu.Lock()
	defer l.mu.Unlock()
	data, err := os.ReadFile(l.file)
	if err != nil {
		return
	}
	_ = json.Unmarshal(data, l)
}

func (l *swfLibrary) saveLocked() {
	data, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(l.file), 0o755)
	_ = os.WriteFile(l.file, data, 0o644)
}

// add 登记一个 SWF（去重、同路径置顶）。
func (l *swfLibrary) add(path string, size int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	name := strings.ToLower(filepath.Base(path))
	entries := l.Files[name]
	// 已存在同名同路径则置顶即可
	for i, e := range entries {
		if strings.EqualFold(e.Path, path) {
			l.Files[name] = append(entries[:i], entries[i+1:]...)
			l.Files[name] = append([]libEntry{{Path: path, Size: size, At: time.Now()}}, l.Files[name]...)
			l.saveLocked()
			return
		}
	}
	l.Files[name] = append([]libEntry{{Path: path, Size: size, At: time.Now()}}, entries...)
	l.saveLocked()
}

// find 按文件名（+ 可选大小校验）定位；优先大小一致者。
func (l *swfLibrary) find(name string, size int64) (string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	entries, ok := l.Files[strings.ToLower(filepath.Base(name))]
	if !ok || len(entries) == 0 {
		return "", false
	}
	if size > 0 {
		for _, e := range entries {
			if e.Size == size {
				return e.Path, true
			}
		}
		return "", false // 有同名但大小不符：宁可不认领（内容可能不同）
	}
	return entries[0].Path, true
}

// addDir 登记目录并对其中的 .swf 建立索引（非递归）。
func (l *swfLibrary) addDir(dir string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	dir = filepath.Clean(dir)
	// 目录去重
	for _, d := range l.Dirs {
		if strings.EqualFold(d, dir) {
			return 0
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if e.IsDir() || !strings.EqualFold(filepath.Ext(e.Name()), ".swf") {
			continue
		}
		full := filepath.Join(dir, e.Name())
		if info, err := e.Info(); err == nil {
			name := strings.ToLower(e.Name())
			l.Files[name] = append([]libEntry{{Path: full, Size: info.Size(), At: time.Now()}}, l.Files[name]...)
			n++
		}
	}
	l.Dirs = append(l.Dirs, dir)
	l.saveLocked()
	return n
}

// scanRoot 递归扫描库根目录（限深 6 层、上限 20000 个）。
func (l *swfLibrary) scanRoot(root string) (int, error) {
	info, err := os.Stat(root)
	if err != nil {
		return 0, err
	}
	if !info.IsDir() {
		return 0, fs.ErrInvalid
	}
	l.mu.Lock()
	root = filepath.Clean(root)
	l.mu.Unlock()

	n := 0
	var walkErr error
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // 跳过不可读项
		}
		if d.IsDir() {
			rel, rerr := filepath.Rel(root, path)
			if rerr == nil && strings.Count(rel, string(filepath.Separator)) >= 6 {
				return fs.SkipDir // 限深
			}
			return nil
		}
		if n >= 20000 {
			walkErr = fs.ErrInvalid
			return fs.SkipAll
		}
		if !strings.EqualFold(filepath.Ext(d.Name()), ".swf") {
			return nil
		}
		if fi, ierr := d.Info(); ierr == nil {
			l.mu.Lock()
			name := strings.ToLower(d.Name())
			l.Files[name] = append([]libEntry{{Path: path, Size: fi.Size(), At: time.Now()}}, l.Files[name]...)
			l.mu.Unlock()
			n++
		}
		return nil
	})
	l.mu.Lock()
	l.Dirs = append(l.Dirs, filepath.Clean(root))
	l.saveLocked()
	l.mu.Unlock()
	return n, walkErr
}

// rescanDirs 启动时后台重建已登记目录的索引。
func (l *swfLibrary) rescanDirs() {
	l.mu.Lock()
	dirs := append([]string(nil), l.Dirs...)
	l.mu.Unlock()
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.EqualFold(filepath.Ext(e.Name()), ".swf") {
				continue
			}
			full := filepath.Join(dir, e.Name())
			if fi, err := e.Info(); err == nil {
				l.add(full, fi.Size())
			}
		}
	}
}

package web

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

// 修改表（cheat table）：以游戏文件内容哈希为钥匙持久化清单，
// 同一游戏再次载入时自动恢复，无需重新搜索。

var tableKeyRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,80}$`)

func tablesDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil || base == "" {
		base = os.TempDir()
	}
	dir := filepath.Join(base, "swfkit", "tables")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

func tablePath(key string) (string, error) {
	if !tableKeyRe.MatchString(key) {
		return "", os.ErrInvalid
	}
	dir, err := tablesDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, key+".json"), nil
}

// tableSave 持久化修改表（内容原样存储，schema 由前端演化）。
func tableSave(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key     string          `json:"key"`
		Name    string          `json:"name"`
		Entries json.RawMessage `json:"entries"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Key == "" {
		writeErr(w, http.StatusBadRequest, "请求体非法")
		return
	}
	path, err := tablePath(req.Key)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "非法的表钥匙")
		return
	}
	blob, _ := json.Marshal(map[string]any{
		"name":    req.Name,
		"entries": json.RawMessage(req.Entries),
		"savedAt": time.Now().Format(time.RFC3339),
	})
	if err := os.WriteFile(path, blob, 0o644); err != nil {
		writeErr(w, http.StatusInternalServerError, "写入失败: "+err.Error())
		return
	}
	writeOK(w, map[string]any{"saved": true})
}

// tableLoad 读取修改表。
func tableLoad(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	path, err := tablePath(key)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "非法的表钥匙")
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		writeOK(w, map[string]any{"found": false})
		return
	}
	var blob map[string]any
	if err := json.Unmarshal(data, &blob); err != nil {
		writeOK(w, map[string]any{"found": false})
		return
	}
	blob["found"] = true
	writeOK(w, blob)
}

// tableDelete 删除修改表。
func tableDelete(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	path, err := tablePath(key)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "非法的表钥匙")
		return
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeOK(w, map[string]any{"deleted": true})
}

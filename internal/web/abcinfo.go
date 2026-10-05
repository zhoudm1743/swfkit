package web

import (
	"bytes"
	"compress/zlib"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"unicode"

	"swfkit/internal/abc"
	"swfkit/internal/swf"
)

// abcInfo 解析 SWF 中的 DoABC 模块，提取属性名字典供搜索参考。
func abcStrings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Path == "" {
		writeErr(w, http.StatusBadRequest, "请求体非法")
		return
	}
	raw, err := os.ReadFile(req.Path)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "文件读取失败: "+err.Error())
		return
	}
	f, err := swf.Parse(raw)
	if err != nil {
		// 解析失败：退化为原始二进制字符串提取
		strs := rawStrings(raw)
		writeOK(w, map[string]any{"strings": strs, "classes": nil, "mode": "raw"})
		return
	}

	// 收集全部字符串常量（过滤噪声）
	strSet := map[string]bool{}
	var classNames []string
	classTraits := map[string][]string{} // className → trait names

	isLikelyProp := func(s string) bool {
		if len(s) < 2 || len(s) > 40 {
			return false
		}
		for _, c := range s {
			if c < 0x20 || c > 0x7E && c < 0x4E00 {
				return false
			}
		}
		low := strings.ToLower(s)
		for _, bad := range []string{"http", "://", ".exe", ".dll", ".png", ".jpg", ".swf", ".mp3", "\x00"} {
			if strings.Contains(low, bad) {
				return false
			}
		}
		return true
	}

	for _, t := range f.Tags {
		if !t.IsDoABC() {
			continue
		}
		ta, err := abc.ParseTag(t, 0)
		if err != nil {
			continue
		}
		pool := ta.File.Pool
		for _, s := range pool.Strings {
			st := string(s)
			if isLikelyProp(st) && !strSet[st] {
				strSet[st] = true
			}
		}
		// 类 + trait 属性名
		for _, inst := range ta.File.Instances {
			cn := ta.File.Pool.MultinameString(inst.Name)
			var props []string
			for _, tr := range inst.Traits {
				tn := ta.File.Pool.MultinameString(tr.Name)
				if tn != "" && isLikelyProp(tn) {
					props = append(props, tn)
				}
			}
			if len(props) > 0 && cn != "" {
				classTraits[cn] = append(classTraits[cn], props...)
				classNames = append(classNames, cn)
			}
		}
	}

	// 排序输出
	var stringsOut []string
	for s := range strSet {
		stringsOut = append(stringsOut, s)
	}
	sort.Slice(stringsOut, func(i, j int) bool {
		return unicode.IsDigit(rune(stringsOut[i][0])) == unicode.IsDigit(rune(stringsOut[j][0])) &&
			stringsOut[i] < stringsOut[j]
	})
	sort.Strings(classNames)

	var classOut []map[string]any
	for _, cn := range classNames {
		traits := classTraits[cn]
		sort.Strings(traits)
		classOut = append(classOut, map[string]any{"name": cn, "traits": traits})
	}

	if len(stringsOut) == 0 {
		// AS2 等无 DoABC 的 SWF：从解压后的 body 提取原始字符串
		body := decompressBody(raw)
		stringsOut = rawStrings(body)
		writeOK(w, map[string]any{"strings": stringsOut, "classes": nil, "mode": "raw"})
		return
	}

	writeOK(w, map[string]any{
		"strings": stringsOut,
		"classes": classOut,
		"mode":    "abc",
	})
}

// rawStrings 从二进制数据提取可打印 ASCII/UTF-8 字符串（≥2 字符）。
func rawStrings(data []byte) []string {
	seen := map[string]bool{}
	var out []string
	i := 0
	for i < len(data) {
		start := i
		for i < len(data) && ((data[i] >= 'a' && data[i] <= 'z') || (data[i] >= 'A' && data[i] <= 'Z') ||
			(data[i] >= '0' && data[i] <= '9') || data[i] == '_' || data[i] >= 0x80) {
			i++
		}
		if i-start >= 2 && i-start <= 40 {
			s := string(data[start:i])
			if !seen[s] {
				seen[s] = true
				// 过滤纯数字
				hasAlpha := false
				for _, c := range s {
					if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c >= 0x80 {
						hasAlpha = true
						break
					}
				}
				if hasAlpha {
					out = append(out, s)
				}
			}
		}
		i++
	}
	return out
}

// decompressBody 解压 SWF body（CWS→zlib，FWS→原样）。
func decompressBody(raw []byte) []byte {
	if len(raw) < 8 {
		return raw
	}
	if raw[0] == 'C' {
		zr, err := zlib.NewReader(bytes.NewReader(raw[8:]))
		if err != nil {
			return raw
		}
		defer zr.Close()
		out, err := io.ReadAll(zr)
		if err != nil {
			return raw
		}
		return out
	}
	return raw[8:]
}

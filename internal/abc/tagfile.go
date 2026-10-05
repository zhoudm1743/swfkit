package abc

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"swfkit/internal/swf"
)

// TagABC 为一个 DoABC tag 的解析视图。
type TagABC struct {
	Index   int    // 在 swf.File.Tags 中的下标
	Name    string // tag 82 携带的模块名（tag 72 为空）
	Flags   uint32 // tag 82 的标志（bit0 = 惰性初始化）
	IsTag72 bool   // 旧式无名称 DoABC（tag 72）
	File    *File
}

// ParseTag 解析单个 DoABC tag。
func ParseTag(tag *swf.Tag, index int) (*TagABC, error) {
	t := &TagABC{Index: index}
	data := tag.Data
	if tag.Code == swf.TagDoABC {
		t.IsTag72 = true
	} else if tag.Code == 82 {
		if len(data) < 5 {
			return nil, fmt.Errorf("abc: DoABC tag 数据过短（%d 字节）", len(data))
		}
		t.Flags = binary.LittleEndian.Uint32(data[:4])
		data = data[4:]
		if i := bytes.IndexByte(data, 0); i >= 0 {
			t.Name = string(data[:i])
			data = data[i+1:]
		} else {
			t.Name = string(data)
			data = nil
		}
	} else {
		return nil, fmt.Errorf("abc: tag code %d 不是 DoABC", tag.Code)
	}
	f, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("abc: DoABC %q: %w", t.Name, err)
	}
	t.File = f
	return t, nil
}

// ParseAll 解析 SWF 中全部 DoABC tag。
func ParseAll(f *swf.File) ([]*TagABC, error) {
	var out []*TagABC
	for i, tag := range f.Tags {
		if tag.IsDoABC() {
			t, err := ParseTag(tag, i)
			if err != nil {
				return nil, err
			}
			out = append(out, t)
		}
	}
	return out, nil
}

// Serialize 重建 tag.Data（调用方需将其写回 swf.Tag 后再 Save SWF）。
func (t *TagABC) Serialize() ([]byte, error) {
	abcData, err := t.File.Serialize()
	if err != nil {
		return nil, err
	}
	if t.IsTag72 {
		return abcData, nil
	}
	buf := make([]byte, 0, 5+len(t.Name)+len(abcData))
	buf = binary.LittleEndian.AppendUint32(buf, t.Flags)
	buf = append(buf, t.Name...)
	buf = append(buf, 0)
	buf = append(buf, abcData...)
	return buf, nil
}

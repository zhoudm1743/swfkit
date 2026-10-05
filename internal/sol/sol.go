package sol

import (
	"encoding/binary"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Object 为有序键值对象（AMF0 object / typed object / ECMA array / AMF3 object/array）。
type Object struct {
	ClassName string // typed object 类名
	IsArray   bool   // ECMA array / AMF3 array
	Keys      []string
	Vals      map[string]any
}

func NewObject() *Object {
	return &Object{Vals: map[string]any{}}
}

// Set 写入键值（新键追加到末尾，已有键保持原位）。
func (o *Object) Set(k string, v any) {
	if o.Vals == nil {
		o.Vals = map[string]any{}
	}
	if _, exists := o.Vals[k]; !exists {
		o.Keys = append(o.Keys, k)
	}
	o.Vals[k] = v
}

func (o *Object) Get(k string) (any, bool) {
	v, ok := o.Vals[k]
	return v, ok
}

// Delete 删除键。
func (o *Object) Delete(k string) {
	if _, ok := o.Vals[k]; !ok {
		return
	}
	delete(o.Vals, k)
	for i, key := range o.Keys {
		if key == k {
			o.Keys = append(o.Keys[:i], o.Keys[i+1:]...)
			break
		}
	}
}

// StrictArray 为 AMF0 严格数组 / AMF3 密集数组。
type StrictArray struct {
	Items []any
}

// Doc 为一个解析后的 .sol 文档。
type Doc struct {
	Name  string
	Fixed []byte // TCSO 之后的 6 字节（保留原样写出）
	Root  *Object
}

// Parse 解析 .sol 字节流。
func Parse(data []byte) (*Doc, error) {
	if len(data) < 16 {
		return nil, fmt.Errorf("sol: 文件过短（%d 字节）", len(data))
	}
	if data[0] != 0x00 || data[1] != 0xBF {
		return nil, fmt.Errorf("sol: 非法魔数 % X，不是 .sol 文件", data[:2])
	}
	declared := int(binary.BigEndian.Uint32(data[2:6]))
	if declared != len(data)-6 {
		// 个别生成器长度字段不准，不作硬校验，仅继续解析
		_ = declared
	}
	body := data[6:]
	if string(body[:4]) != "TCSO" {
		return nil, fmt.Errorf("sol: 缺少 TCSO 标记")
	}
	doc := &Doc{Fixed: append([]byte(nil), body[4:10]...)}
	pos := 10
	// 名称：U16 BE；高位置位时为 U32 长变体
	nameLen := int(binary.BigEndian.Uint16(body[pos:]))
	longName := nameLen&0x8000 != 0
	if longName {
		nameLen = int(binary.BigEndian.Uint32(body[pos:]))
		pos += 4
	} else {
		pos += 2
	}
	if pos+nameLen > len(body) {
		return nil, fmt.Errorf("sol: 名称字段越界")
	}
	doc.Name = string(body[pos : pos+nameLen])
	pos += nameLen
	pos += 4 // 名称后 4 个零字节

	r := &amf0Reader{data: body, pos: pos}
	doc.Root = NewObject()
	for {
		if r.pos >= len(r.data) {
			break
		}
		klen, err := r.u16()
		if err != nil {
			return nil, fmt.Errorf("sol: 键长读取失败: %w", err)
		}
		if klen == 0 { // 顶层终止符（00 00 09）
			if m, err := r.u8(); err == nil && m != amf0ObjectEnd {
				return nil, fmt.Errorf("sol: 顶层终止符异常 0x%02X", m)
			}
			break
		}
		kb, err := r.need(klen)
		if err != nil {
			return nil, err
		}
		k := string(kb)
		v, err := r.readValue(k)
		if err != nil {
			return nil, fmt.Errorf("sol: key=%q: %w", k, err)
		}
		doc.Root.Set(k, v)
	}
	return doc, nil
}

// Serialize 写出 .sol 字节流（标准终止符 + 1 字节尾垫）。
func (d *Doc) Serialize() ([]byte, error) {
	var out []byte
	out = append(out, 0x00, 0xBF)
	out = binary.BigEndian.AppendUint32(out, 0) // 长度占位，末尾回填
	out = append(out, "TCSO"...)
	if len(d.Fixed) == 6 {
		out = append(out, d.Fixed...)
	} else {
		out = append(out, 0x00, 0x04, 0x00, 0x00, 0x00, 0x00)
	}
	out = binary.BigEndian.AppendUint16(out, uint16(len(d.Name)))
	out = append(out, d.Name...)
	out = append(out, 0, 0, 0, 0)

	w := &amf0Writer{}
	for _, k := range d.Root.Keys {
		w.str(k)
		if err := w.writeValue(d.Root.Vals[k]); err != nil {
			return nil, fmt.Errorf("sol: key=%q: %w", k, err)
		}
	}
	w.u16(0)
	w.u8(amf0ObjectEnd)
	w.u8(0x00) // 尾垫
	out = append(out, w.buf...)

	binary.BigEndian.PutUint32(out[2:6], uint32(len(out)-6))
	return out, nil
}

// Get 按点分路径取值（数组下标作为数字段，如 "items.0.label"）。
func (d *Doc) Get(path string) (any, bool) {
	var cur any = d.Root
	for _, seg := range strings.Split(path, ".") {
		obj, ok := cur.(*Object)
		if !ok {
			arr, ok := cur.(*StrictArray)
			if !ok {
				return nil, false
			}
			idx, err := strconv.Atoi(seg)
			if err != nil || idx < 0 || idx >= len(arr.Items) {
				return nil, false
			}
			cur = arr.Items[idx]
			continue
		}
		v, exists := obj.Get(seg)
		if !exists {
			return nil, false
		}
		cur = v
	}
	return cur, true
}

// Set 按点分路径写入标量值；中间层不存在时自动创建对象。
func (d *Doc) Set(path string, v any) error {
	segs := strings.Split(path, ".")
	if len(segs) == 0 || path == "" {
		return fmt.Errorf("sol: 路径为空")
	}
	var cur *Object = d.Root
	for i, seg := range segs {
		last := i == len(segs)-1
		if last {
			cur.Set(seg, v)
			return nil
		}
		next, exists := cur.Get(seg)
		if !exists {
			nn := NewObject()
			cur.Set(seg, nn)
			cur = nn
			continue
		}
		no, ok := next.(*Object)
		if !ok {
			return fmt.Errorf("sol: 路径 %q 的中间层 %q 不是对象（%T）", path, seg, next)
		}
		cur = no
	}
	return nil
}

// ParseScalar 将字符串解析为标量：数字 / true / false / null / 字符串。
func ParseScalar(s string) (any, error) {
	s = strings.TrimSpace(s)
	switch s {
	case "true":
		return true, nil
	case "false":
		return false, nil
	case "null", "":
		return nil, nil
	}
	if iv, err := strconv.ParseInt(s, 0, 64); err == nil {
		if iv >= math.MinInt32 && iv <= math.MaxInt32 {
			return float64(iv), nil // 存档数值统一按 Number 语义写回
		}
		return float64(iv), nil
	}
	if fv, err := strconv.ParseFloat(s, 64); err == nil {
		return fv, nil
	}
	return s, nil
}

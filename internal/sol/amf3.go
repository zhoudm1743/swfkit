package sol

import (
	"encoding/binary"
	"fmt"
	"math"
	"strconv"
)

// AMF3 标记。
const (
	amf3Undefined = 0x00
	amf3Null      = 0x01
	amf3False     = 0x02
	amf3True      = 0x03
	amf3Integer   = 0x04
	amf3Double    = 0x05
	amf3String    = 0x06
	amf3XMLDoc    = 0x07
	amf3Date      = 0x08
	amf3Array     = 0x09
	amf3Object    = 0x0A
	amf3XML       = 0x0B
	amf3ByteArray = 0x0C
)

// traits 为 AMF3 对象的类描述（密封成员表 + 动态标志）。
type traits struct {
	className string
	dynamic   bool
	members   []string
}

// amf3Reader 解码 AMF3 值流；引用表读侧完整解析，写侧一律内联。
type amf3Reader struct {
	data      []byte
	pos       int
	refs      []any     // date/array/object/bytearray/xml 引用表
	strRefs   []string  // 字符串引用表
	traitRefs []*traits // traits 引用表
}

func (r *amf3Reader) need(n int) ([]byte, error) {
	if r.pos+n > len(r.data) {
		return nil, errEOM
	}
	b := r.data[r.pos : r.pos+n : r.pos+n]
	r.pos += n
	return b, nil
}

// u29 读取 AMF3 变长整数（最多 4 字节、29 位）。
func (r *amf3Reader) u29() (int, error) {
	var v int
	for i := 0; i < 3; i++ {
		b, err := r.need(1)
		if err != nil {
			return 0, err
		}
		v = v<<7 | int(b[0]&0x7F)
		if b[0]&0x80 == 0 {
			return v, nil
		}
	}
	b, err := r.need(1)
	if err != nil {
		return 0, err
	}
	return v<<8 | int(b[0]), nil
}

func (r *amf3Reader) double() (float64, error) {
	b, err := r.need(8)
	if err != nil {
		return 0, err
	}
	return math.Float64frombits(binary.BigEndian.Uint64(b)), nil
}

func (r *amf3Reader) refValue(idx int) (any, error) {
	if idx >= len(r.refs) {
		return nil, fmt.Errorf("amf3: 引用索引 %d 越界", idx)
	}
	return r.refs[idx], nil
}

// readString 读取 AMF3 字符串（支持引用）。
func (r *amf3Reader) readString() (string, error) {
	v, err := r.u29()
	if err != nil {
		return "", err
	}
	if v&1 == 0 { // 引用
		idx := v >> 1
		if idx >= len(r.strRefs) {
			return "", fmt.Errorf("amf3: 字符串引用 %d 越界", idx)
		}
		return r.strRefs[idx], nil
	}
	n := v >> 1
	if n == 0 {
		return "", nil
	}
	b, err := r.need(n)
	if err != nil {
		return "", err
	}
	s := string(b)
	r.strRefs = append(r.strRefs, s)
	return s, nil
}

func (r *amf3Reader) readValue(key string) (any, error) {
	marker, err := r.need(1)
	if err != nil {
		return nil, err
	}
	switch marker[0] {
	case amf3Undefined:
		return Undefined{}, nil
	case amf3Null:
		return nil, nil
	case amf3False:
		return false, nil
	case amf3True:
		return true, nil
	case amf3Integer:
		v, err := r.u29()
		if err != nil {
			return nil, err
		}
		if v&0x10000000 != 0 { // 29 位符号扩展
			v -= 0x20000000
		}
		return int32(v), nil
	case amf3Double:
		return r.double()
	case amf3String:
		return r.readString()
	case amf3XMLDoc, amf3XML:
		s, err := r.readString()
		if err != nil {
			return nil, err
		}
		x := &XMLDoc{IsDoc: marker[0] == amf3XMLDoc, Text: s}
		r.refs = append(r.refs, x)
		return x, nil
	case amf3Date:
		v, err := r.u29()
		if err != nil {
			return nil, err
		}
		if v&1 == 0 {
			return r.refValue(v >> 1)
		}
		ms, err := r.double()
		if err != nil {
			return nil, err
		}
		d := Date{Millis: ms}
		r.refs = append(r.refs, d)
		return d, nil
	case amf3ByteArray:
		v, err := r.u29()
		if err != nil {
			return nil, err
		}
		if v&1 == 0 {
			return r.refValue(v >> 1)
		}
		b, err := r.need(v >> 1)
		if err != nil {
			return nil, err
		}
		cp := make([]byte, len(b))
		copy(cp, b)
		r.refs = append(r.refs, cp)
		return cp, nil
	case amf3Array:
		v, err := r.u29()
		if err != nil {
			return nil, err
		}
		if v&1 == 0 {
			return r.refValue(v >> 1)
		}
		dense := v >> 1
		obj := &Object{IsArray: true}
		r.refs = append(r.refs, obj)
		for { // 关联键值直到空键
			k, err := r.readString()
			if err != nil {
				return nil, err
			}
			if k == "" {
				break
			}
			val, err := r.readValue(k)
			if err != nil {
				return nil, err
			}
			obj.Set(k, val)
		}
		for i := 0; i < dense; i++ {
			val, err := r.readValue("")
			if err != nil {
				return nil, err
			}
			obj.Set(strconv.Itoa(len(obj.Keys)), val)
		}
		return obj, nil
	case amf3Object:
		v, err := r.u29()
		if err != nil {
			return nil, err
		}
		if v&1 == 0 {
			return r.refValue(v >> 1)
		}
		return r.readObject(v, key)
	default:
		return nil, fmt.Errorf("amf3: 未知标记 0x%02X（key=%q）", marker[0], key)
	}
}

func (r *amf3Reader) readObject(v int, key string) (any, error) {
	var tr *traits
	switch v & 0x03 {
	case 1: // traits 引用
		idx := v >> 2
		if idx >= len(r.traitRefs) {
			return nil, fmt.Errorf("amf3: traits 引用 %d 越界（key=%q）", idx, key)
		}
		tr = r.traitRefs[idx]
	case 3: // 内联 traits
		externalizable := v&0x04 != 0
		dynamic := v&0x08 != 0
		memberCount := v >> 4
		cn, err := r.readString()
		if err != nil {
			return nil, err
		}
		tr = &traits{className: cn, dynamic: dynamic}
		for i := 0; i < memberCount; i++ {
			name, err := r.readString()
			if err != nil {
				return nil, err
			}
			tr.members = append(tr.members, name)
		}
		r.traitRefs = append(r.traitRefs, tr)
		if externalizable {
			return nil, fmt.Errorf("amf3: 暂不支持 Externalizable 类 %q（key=%q）", cn, key)
		}
	default:
		return nil, fmt.Errorf("amf3: 对象头异常 0x%X（key=%q）", v, key)
	}
	obj := &Object{ClassName: tr.className}
	r.refs = append(r.refs, obj)
	for _, name := range tr.members {
		val, err := r.readValue(name)
		if err != nil {
			return nil, err
		}
		obj.Set(name, val)
	}
	if tr.dynamic {
		for {
			k, err := r.readString()
			if err != nil {
				return nil, err
			}
			if k == "" {
				break
			}
			val, err := r.readValue(k)
			if err != nil {
				return nil, err
			}
			obj.Set(k, val)
		}
	}
	return obj, nil
}

// amf3Writer 编码 AMF3 值流（引用一律内联展开）。
type amf3Writer struct {
	buf []byte
}

func (w *amf3Writer) raw(b []byte) { w.buf = append(w.buf, b...) }

// u29 写出变长整数（最小编码）。
func (w *amf3Writer) u29(v int) {
	switch {
	case v < 0x80:
		w.raw([]byte{byte(v)})
	case v < 0x4000:
		w.raw([]byte{byte(v>>7) | 0x80, byte(v & 0x7F)})
	case v < 0x200000:
		w.raw([]byte{byte(v>>14) | 0x80, byte((v>>7)&0x7F) | 0x80, byte(v & 0x7F)})
	default:
		w.raw([]byte{byte(v>>22) | 0x80, byte((v>>15)&0x7F) | 0x80, byte((v>>8)&0x7F) | 0x80, byte(v & 0xFF)})
	}
}

func (w *amf3Writer) str(s string) {
	if s == "" {
		w.u29(1) // len 0 内联
		return
	}
	w.u29(len(s)<<1 | 1)
	w.raw([]byte(s))
}

func (w *amf3Writer) double(v float64) {
	w.raw(binary.BigEndian.AppendUint64(nil, math.Float64bits(v)))
}

func (w *amf3Writer) writeValue(v any) error {
	switch x := v.(type) {
	case nil:
		w.raw([]byte{amf3Null})
	case Undefined:
		w.raw([]byte{amf3Undefined})
	case bool:
		if x {
			w.raw([]byte{amf3True})
		} else {
			w.raw([]byte{amf3False})
		}
	case int32:
		w.raw([]byte{amf3Integer})
		w.u29(int(x))
	case float64:
		w.raw([]byte{amf3Double})
		w.double(x)
	case string:
		w.raw([]byte{amf3String})
		w.str(x)
	case *XMLDoc:
		if x.IsDoc {
			w.raw([]byte{amf3XMLDoc})
		} else {
			w.raw([]byte{amf3XML})
		}
		w.str(x.Text)
	case Date:
		w.raw([]byte{amf3Date})
		w.u29(1) // 内联
		w.double(x.Millis)
	case []byte:
		w.raw([]byte{amf3ByteArray})
		w.u29(len(x)<<1 | 1)
		w.raw(x)
	case *Object:
		if x.IsArray {
			w.raw([]byte{amf3Array})
			return w.writeArrayBody(x)
		}
		w.raw([]byte{amf3Object})
		// 内联密封 traits（无动态尾巴）：bit0=1 内联、bit1=1 非 traits 引用、
		// bit2=0 非 Externalizable、bit3=0 非动态、bit4+ 为成员数
		w.u29(len(x.Keys)<<4 | 0x03)
		w.str(x.ClassName)
		for _, k := range x.Keys {
			w.str(k)
		}
		for _, k := range x.Keys {
			if err := w.writeValue(x.Vals[k]); err != nil {
				return err
			}
		}
		return nil
	case *StrictArray:
		w.raw([]byte{amf3Array})
		w.u29(len(x.Items)<<1 | 1)
		for _, it := range x.Items {
			if err := w.writeValue(it); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("amf3: 不支持的值类型 %T", v)
	}
	return nil
}

// writeArrayBody 写 AMF3 数组：数字连续键视为密集项，其余为关联键。
func (w *amf3Writer) writeArrayBody(obj *Object) error {
	var assoc, dense []string
	for _, k := range obj.Keys {
		if isNumericKey(k) && k == strconv.Itoa(len(dense)) {
			dense = append(dense, k)
		} else {
			assoc = append(assoc, k)
		}
	}
	w.u29(len(dense)<<1 | 1)
	for _, k := range assoc {
		w.str(k)
		if err := w.writeValue(obj.Vals[k]); err != nil {
			return err
		}
	}
	w.str("") // 关联区终止
	for _, k := range dense {
		if err := w.writeValue(obj.Vals[k]); err != nil {
			return err
		}
	}
	return nil
}

func isNumericKey(k string) bool {
	if k == "" {
		return false
	}
	for i := 0; i < len(k); i++ {
		if k[i] < '0' || k[i] > '9' {
			return false
		}
	}
	return true
}

// Package sol 解析与写回 Flash 本地共享对象（.sol）存档：
// SOL 容器头 + AMF0/AMF3 值编码（AVM+ 切换 0x11 支持 AMF3 内嵌）。
package sol

import (
	"encoding/binary"
	"fmt"
	"math"
)

// AMF0 标记。
const (
	amf0Number      = 0x00
	amf0Boolean     = 0x01
	amf0String      = 0x02
	amf0Object      = 0x03
	amf0Null        = 0x05
	amf0Undefined   = 0x06
	amf0Reference   = 0x07
	amf0ECMAArray   = 0x08
	amf0ObjectEnd   = 0x09
	amf0StrictArray = 0x0A
	amf0Date        = 0x0B
	amf0LongString  = 0x0C
	amf0Unsupported = 0x0D
	amf0XMLDoc      = 0x0F
	amf0TypedObject = 0x10
	amf0AVMPlus     = 0x11
)

// Undefined 表示 AMF undefined 值（与 null 区分）。
type Undefined struct{}

// Date 为 AMF 日期值。
type Date struct {
	Millis float64
	TZ     int16 // 仅 AMF0 携带时区
}

// XMLDoc 为 XML/XMLDoc 值（保留原始文本）。
type XMLDoc struct {
	IsDoc bool // true = AMF0 XMLDoc / AMF3 XMLDoc，false = AMF3 XML
	Text  string
}

// amf0Reader 解码 AMF0 值流（大端）。
type amf0Reader struct {
	data []byte
	pos  int
	refs []any       // 对象引用表（object/ecma/strict/typed）
	amf3 *amf3Reader // AVM+ 桥（惰性创建，跨切换共享引用表）
}

var errEOM = fmt.Errorf("amf0: 输入结束")

func (r *amf0Reader) need(n int) ([]byte, error) {
	if r.pos+n > len(r.data) {
		return nil, errEOM
	}
	b := r.data[r.pos : r.pos+n : r.pos+n]
	r.pos += n
	return b, nil
}

func (r *amf0Reader) u8() (byte, error) {
	b, err := r.need(1)
	if err != nil {
		return 0, err
	}
	return b[0], nil
}

func (r *amf0Reader) u16() (int, error) {
	b, err := r.need(2)
	if err != nil {
		return 0, err
	}
	return int(binary.BigEndian.Uint16(b)), nil
}

func (r *amf0Reader) u32() (int, error) {
	b, err := r.need(4)
	if err != nil {
		return 0, err
	}
	return int(binary.BigEndian.Uint32(b)), nil
}

func (r *amf0Reader) double() (float64, error) {
	b, err := r.need(8)
	if err != nil {
		return 0, err
	}
	return math.Float64frombits(binary.BigEndian.Uint64(b)), nil
}

// readValueUntil 读取 AMF0 值；keyOf 用于报告上下文。
func (r *amf0Reader) readValue(key string) (any, error) {
	marker, err := r.u8()
	if err != nil {
		return nil, err
	}
	switch marker {
	case amf0Number:
		return r.double()
	case amf0Boolean:
		b, err := r.u8()
		return b != 0, err
	case amf0String:
		return r.readString()
	case amf0Null:
		return nil, nil
	case amf0Undefined:
		return Undefined{}, nil
	case amf0Reference:
		idx, err := r.u16()
		if err != nil {
			return nil, err
		}
		if idx >= len(r.refs) {
			return nil, fmt.Errorf("amf0: 引用索引 %d 越界（key=%q）", idx, key)
		}
		return r.refs[idx], nil
	case amf0ECMAArray:
		if _, err := r.u32(); err != nil { // 关联计数（读取方忽略）
			return nil, err
		}
		obj := &Object{IsArray: true}
		r.refs = append(r.refs, obj)
		if err := r.readEntriesInto(obj, key); err != nil {
			return nil, err
		}
		return obj, nil
	case amf0Object:
		obj := &Object{}
		r.refs = append(r.refs, obj)
		if err := r.readEntriesInto(obj, key); err != nil {
			return nil, err
		}
		return obj, nil
	case amf0StrictArray:
		n, err := r.u32()
		if err != nil {
			return nil, err
		}
		arr := &StrictArray{Items: make([]any, 0, min(n, 1<<20))}
		r.refs = append(r.refs, arr)
		for i := 0; i < n; i++ {
			v, err := r.readValue(key)
			if err != nil {
				return nil, err
			}
			arr.Items = append(arr.Items, v)
		}
		return arr, nil
	case amf0Date:
		ms, err := r.double()
		if err != nil {
			return nil, err
		}
		tzb, err := r.need(2)
		if err != nil {
			return nil, err
		}
		tz := int16(binary.BigEndian.Uint16(tzb))
		return Date{Millis: ms, TZ: tz}, nil
	case amf0LongString:
		return r.readLongString()
	case amf0Unsupported:
		return Undefined{}, nil
	case amf0XMLDoc:
		s, err := r.readString()
		return XMLDoc{IsDoc: true, Text: s}, err
	case amf0TypedObject:
		cn, err := r.readString()
		if err != nil {
			return nil, err
		}
		obj := &Object{ClassName: cn}
		r.refs = append(r.refs, obj)
		if err := r.readEntriesInto(obj, key); err != nil {
			return nil, err
		}
		return obj, nil
	case amf0AVMPlus:
		return r.readAMF3(key)
	default:
		return nil, fmt.Errorf("amf0: 未知标记 0x%02X（key=%q）", marker, key)
	}
}

// readAMF3 从 AVM+ 切换点读取 AMF3 值；引用表在整个文档生命周期内共享。
func (r *amf0Reader) readAMF3(key string) (any, error) {
	if r.amf3 == nil {
		r.amf3 = &amf3Reader{data: r.data, pos: r.pos}
	}
	r.amf3.data = r.data
	r.amf3.pos = r.pos
	v, err := r.amf3.readValue(key)
	r.pos = r.amf3.pos
	return v, err
}

func (r *amf0Reader) readString() (string, error) {
	n, err := r.u16()
	if err != nil {
		return "", err
	}
	b, err := r.need(n)
	return string(b), err
}

func (r *amf0Reader) readLongString() (string, error) {
	n, err := r.u32()
	if err != nil {
		return "", err
	}
	b, err := r.need(n)
	return string(b), err
}

// readEntriesInto 读取 (key, value)* 直到 00 00 09 终止符。
func (r *amf0Reader) readEntriesInto(obj *Object, ctx string) error {
	for {
		klen, err := r.u16()
		if err != nil {
			return err
		}
		if klen == 0 {
			m, err := r.u8()
			if err != nil {
				return err
			}
			if m != amf0ObjectEnd {
				return fmt.Errorf("amf0: 空键后期望对象终止符，得到 0x%02X", m)
			}
			return nil
		}
		kb, err := r.need(klen)
		if err != nil {
			return err
		}
		k := string(kb)
		v, err := r.readValue(k)
		if err != nil {
			return fmt.Errorf("amf0: key=%q: %w", k, err)
		}
		obj.Set(k, v)
		_ = ctx
	}
}

// amf0Writer 编码 AMF0 值流。
type amf0Writer struct {
	buf []byte
}

func (w *amf0Writer) u8(v byte)    { w.buf = append(w.buf, v) }
func (w *amf0Writer) u16(v int)    { w.buf = binary.BigEndian.AppendUint16(w.buf, uint16(v)) }
func (w *amf0Writer) u32(v int)    { w.buf = binary.BigEndian.AppendUint32(w.buf, uint32(v)) }
func (w *amf0Writer) raw(b []byte) { w.buf = append(w.buf, b...) }
func (w *amf0Writer) str(s string) {
	w.u16(len(s))
	w.raw([]byte(s))
}
func (w *amf0Writer) double(v float64) {
	w.raw(binary.BigEndian.AppendUint64(nil, math.Float64bits(v)))
}

// writeAMF3 写出 AMF3 载荷（不含 AVM+ 切换标记，调用方已写）。
func (w *amf0Writer) writeAMF3(v any) error {
	w3 := &amf3Writer{}
	if err := w3.writeValue(v); err != nil {
		return err
	}
	w.raw(w3.buf)
	return nil
}

// writeValue 序列化 AMF0 值；AMF3 专属类型经 AVM+ 切换内嵌。
func (w *amf0Writer) writeValue(v any) error {
	switch x := v.(type) {
	case nil:
		w.u8(amf0Null)
	case bool:
		w.u8(amf0Boolean)
		if x {
			w.u8(1)
		} else {
			w.u8(0)
		}
	case float64:
		w.u8(amf0Number)
		w.double(x)
	case int32: // AMF3 整数经 AVM+ 表达
		w.u8(amf0AVMPlus)
		w.writeAMF3(x)
	case string:
		if len(x) > 0xFFFF {
			w.u8(amf0LongString)
			w.u32(len(x))
			w.raw([]byte(x))
		} else {
			w.u8(amf0String)
			w.str(x)
		}
	case Undefined:
		w.u8(amf0Undefined)
	case Date:
		w.u8(amf0Date)
		w.double(x.Millis)
		w.u16(int(uint16(x.TZ)))
	case *XMLDoc:
		if x.IsDoc {
			w.u8(amf0XMLDoc)
			w.str(x.Text)
		} else {
			w.u8(amf0AVMPlus)
			w.writeAMF3(x)
		}
	case []byte: // ByteArray 仅存在于 AMF3
		w.u8(amf0AVMPlus)
		w.writeAMF3(x)
	case *Object:
		if x.IsArray {
			w.u8(amf0ECMAArray)
			w.u32(len(x.Keys))
		} else if x.ClassName != "" {
			w.u8(amf0TypedObject)
			w.str(x.ClassName)
		} else {
			w.u8(amf0Object)
		}
		for _, k := range x.Keys {
			w.str(k)
			if err := w.writeValue(x.Vals[k]); err != nil {
				return err
			}
		}
		w.u16(0)
		w.u8(amf0ObjectEnd)
	case *StrictArray:
		w.u8(amf0StrictArray)
		w.u32(len(x.Items))
		for _, it := range x.Items {
			if err := w.writeValue(it); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("amf0: 不支持的值类型 %T", v)
	}
	return nil
}

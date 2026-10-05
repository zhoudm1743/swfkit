// Package abc 解析与写回 AVM2 (ActionScript 3) 字节码，
// 格式依据 Adobe《AVM2 Overview》(avm2overview.pdf) 公开规范。
package abc

import (
	"encoding/binary"
	"fmt"
	"math"
)

// reader 为 ABC 二进制顺序读取器（小端多字节 + 变长 u30/u32）。
type reader struct {
	data []byte
	pos  int
	err  error
}

func (r *reader) fail(err error) {
	if r.err == nil {
		r.err = err
	}
}

func (r *reader) u8() byte {
	if r.err != nil {
		return 0
	}
	if r.pos >= len(r.data) {
		r.fail(fmt.Errorf("abc: 数据截断于偏移 %d", r.pos))
		return 0
	}
	v := r.data[r.pos]
	r.pos++
	return v
}

func (r *reader) bytes(n int) []byte {
	if r.err != nil {
		return nil
	}
	if r.pos+n > len(r.data) {
		r.fail(fmt.Errorf("abc: 数据截断于偏移 %d，需要 %d 字节", r.pos, n))
		return nil
	}
	v := r.data[r.pos : r.pos+n : r.pos+n]
	r.pos += n
	return v
}

// varLen 读取变长整数（u30/u32 同一编码：每字节低 7 位，最高位为续传标志；
// 最多 5 字节，第 5 字节贡献全部 8 位）。u30 语义上限 30 位，读取时统一按 u32 处理。
func (r *reader) varLen() uint32 {
	var v uint32
	for i := 0; i < 5; i++ {
		b := r.u8()
		if r.err != nil {
			return 0
		}
		if i == 4 { // 第 5 字节：8 位全用
			v |= uint32(b) << 28
			return v
		}
		v |= uint32(b&0x7F) << (7 * i)
		if b&0x80 == 0 {
			return v
		}
	}
	r.fail(fmt.Errorf("abc: 变长整数超过 5 字节（偏移 %d）", r.pos))
	return 0
}

func (r *reader) u30() uint32 { return r.varLen() }
func (r *reader) s24() int32 {
	b := r.bytes(3)
	if r.err != nil {
		return 0
	}
	v := uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16
	if v&0x800000 != 0 { // 符号扩展
		v |= 0xFF000000
	}
	return int32(v)
}
func (r *reader) u16() uint16 {
	b := r.bytes(2)
	if r.err != nil {
		return 0
	}
	return binary.LittleEndian.Uint16(b)
}

// writer 为 ABC 二进制写出器。
type writer struct {
	buf []byte
}

func (w *writer) u8(v byte)    { w.buf = append(w.buf, v) }
func (w *writer) raw(b []byte) { w.buf = append(w.buf, b...) }
func (w *writer) u16(v uint16) { w.buf = binary.LittleEndian.AppendUint16(w.buf, v) }

// varLen 写出变长整数（最小编码）。
func (w *writer) varLen(v uint32) {
	for {
		if v < 0x80 {
			w.u8(byte(v))
			return
		}
		w.u8(byte(v&0x7F) | 0x80)
		v >>= 7
	}
}

func (w *writer) u30(v uint32) { w.varLen(v) }
func (w *writer) s24(v int32) {
	w.buf = append(w.buf, byte(v), byte(v>>8), byte(v>>16))
}

// ---- 常量池 ----

// 命名空间种类（AVM2 spec §4.4.3）。
const (
	NamespacePrivate    = 0x05
	NamespacePackage    = 0x16
	NamespaceInternal   = 0x17
	NamespaceProtected  = 0x18
	NamespaceExplicit   = 0x19
	NamespaceStaticProt = 0x1A
)

// 多名种类（AVM2 spec §4.4，经 RABCDAsm 实现核对）。
const (
	MultinameQName     = 0x07 // ns, name
	MultinameQNameA    = 0x0D
	MultinameRTQName   = 0x0F // name
	MultinameRTQNameA  = 0x10
	MultinameRTQNameL  = 0x11 // 无载荷
	MultinameRTQNameLA = 0x12
	MultinameName      = 0x09 // name, ns_set
	MultinameNameA     = 0x0E
	MultinameNameL     = 0x1B // ns_set
	MultinameNameLA    = 0x1C
	MultinameTypeName  = 0x1D // 泛型 Vector.<T>
)

// Namespace 为命名空间条目。
type Namespace struct {
	Kind uint8
	Name uint32 // 字符串索引，0 = 空名
}

// NamespaceSet 为命名空间集合。
type NamespaceSet struct {
	Namespaces []uint32
}

// MultinameKind 为多名条目的种类与载荷（载荷字段按 kind 取用）。
type Multiname struct {
	Kind       uint8
	NS         uint32   // QName: 命名空间索引
	Name       uint32   // QName/RTQName/Multiname: 名称索引；TypeName: 类型名
	NSSet      uint32   // Multiname/MultinameL: ns_set 索引
	ParamCount uint32   // TypeName
	Params     []uint32 // TypeName 泛型参数
}

// Pool 为 ABC 常量池。所有索引 1 起始（0 为"无/默认"），与规范一致；
// 下标 i-1 对应索引 i。
type Pool struct {
	Ints       []int32
	UInts      []uint32
	Doubles    []float64
	Strings    [][]byte // 保留原始字节，避免非法 UTF-8 丢失
	Namespaces []Namespace
	NSSets     []NamespaceSet
	Multinames []Multiname
}

func (p *Pool) String(i uint32) string {
	if i == 0 || int(i) > len(p.Strings) {
		return ""
	}
	return string(p.Strings[i-1])
}

// SetString 修改字符串常量（索引 1 起始）。
func (p *Pool) SetString(i uint32, s string) {
	if i >= 1 && int(i) <= len(p.Strings) {
		p.Strings[i-1] = []byte(s)
	}
}

// AddString 追加字符串常量，返回其索引。
func (p *Pool) AddString(s string) uint32 {
	p.Strings = append(p.Strings, []byte(s))
	return uint32(len(p.Strings))
}

// AddInt / AddUInt / AddDouble 追加数值常量，返回索引。
func (p *Pool) AddInt(v int32) uint32 {
	p.Ints = append(p.Ints, v)
	return uint32(len(p.Ints))
}

func (p *Pool) AddUInt(v uint32) uint32 {
	p.UInts = append(p.UInts, v)
	return uint32(len(p.UInts))
}

func (p *Pool) AddDouble(v float64) uint32 {
	p.Doubles = append(p.Doubles, v)
	return uint32(len(p.Doubles))
}

// parsePool 解析常量池。注意规范规定 count 字段 = 条目数 + 1。
func parsePool(r *reader) *Pool {
	p := &Pool{}
	if intCount := r.u30(); intCount > 0 {
		p.Ints = make([]int32, 0, intCount-1)
		for i := uint32(1); i < intCount && r.err == nil; i++ {
			p.Ints = append(p.Ints, int32(r.varLen()))
		}
	}
	if uintCount := r.u30(); uintCount > 0 {
		p.UInts = make([]uint32, 0, uintCount-1)
		for i := uint32(1); i < uintCount && r.err == nil; i++ {
			p.UInts = append(p.UInts, r.varLen())
		}
	}
	if dblCount := r.u30(); dblCount > 0 {
		p.Doubles = make([]float64, 0, dblCount-1)
		for i := uint32(1); i < dblCount && r.err == nil; i++ {
			b := r.bytes(8)
			if r.err != nil {
				break
			}
			p.Doubles = append(p.Doubles, math.Float64frombits(binary.LittleEndian.Uint64(b)))
		}
	}
	if strCount := r.u30(); strCount > 0 {
		p.Strings = make([][]byte, 0, strCount-1)
		for i := uint32(1); i < strCount && r.err == nil; i++ {
			n := int(r.u30())
			p.Strings = append(p.Strings, r.bytes(n))
		}
	}
	if nsCount := r.u30(); nsCount > 0 {
		p.Namespaces = make([]Namespace, 0, nsCount-1)
		for i := uint32(1); i < nsCount && r.err == nil; i++ {
			p.Namespaces = append(p.Namespaces, Namespace{Kind: r.u8(), Name: r.u30()})
		}
	}
	if setCount := r.u30(); setCount > 0 {
		p.NSSets = make([]NamespaceSet, 0, setCount-1)
		for i := uint32(1); i < setCount && r.err == nil; i++ {
			n := r.u30()
			ns := NamespaceSet{Namespaces: make([]uint32, 0, n)}
			for j := uint32(0); j < n && r.err == nil; j++ {
				ns.Namespaces = append(ns.Namespaces, r.u30())
			}
			p.NSSets = append(p.NSSets, ns)
		}
	}
	if mnCount := r.u30(); mnCount > 0 {
		p.Multinames = make([]Multiname, 0, mnCount-1)
		for i := uint32(1); i < mnCount && r.err == nil; i++ {
			mn := Multiname{Kind: r.u8()}
			switch mn.Kind {
			case MultinameQName, MultinameQNameA:
				mn.NS, mn.Name = r.u30(), r.u30()
			case MultinameName, MultinameNameA:
				mn.Name, mn.NSSet = r.u30(), r.u30()
			case MultinameNameL, MultinameNameLA:
				mn.NSSet = r.u30()
			case MultinameRTQName, MultinameRTQNameA:
				mn.Name = r.u30()
			case MultinameRTQNameL, MultinameRTQNameLA:
				// 无载荷
			case MultinameTypeName:
				mn.Name = r.u30()
				mn.ParamCount = r.u30()
				for j := uint32(0); j < mn.ParamCount && r.err == nil; j++ {
					mn.Params = append(mn.Params, r.u30())
				}
			default:
				r.fail(fmt.Errorf("abc: 未知多名种类 0x%02X（索引 %d）", mn.Kind, i+1))
			}
			p.Multinames = append(p.Multinames, mn)
		}
	}
	return p
}

// serialize 写出常量池（count = 条目数 + 1）。
func (p *Pool) serialize(w *writer) {
	w.varLen(uint32(len(p.Ints)) + 1)
	for _, v := range p.Ints {
		w.varLen(uint32(v))
	}
	w.varLen(uint32(len(p.UInts)) + 1)
	for _, v := range p.UInts {
		w.varLen(v)
	}
	w.varLen(uint32(len(p.Doubles)) + 1)
	for _, v := range p.Doubles {
		w.raw(binary.LittleEndian.AppendUint64(nil, math.Float64bits(v)))
	}
	w.varLen(uint32(len(p.Strings)) + 1)
	for _, s := range p.Strings {
		w.varLen(uint32(len(s)))
		w.raw(s)
	}
	w.varLen(uint32(len(p.Namespaces)) + 1)
	for _, ns := range p.Namespaces {
		w.u8(ns.Kind)
		w.u30(ns.Name)
	}
	w.varLen(uint32(len(p.NSSets)) + 1)
	for _, set := range p.NSSets {
		w.varLen(uint32(len(set.Namespaces)))
		for _, n := range set.Namespaces {
			w.u30(n)
		}
	}
	w.varLen(uint32(len(p.Multinames)) + 1)
	for _, mn := range p.Multinames {
		w.u8(mn.Kind)
		switch mn.Kind {
		case MultinameQName, MultinameQNameA:
			w.u30(mn.NS)
			w.u30(mn.Name)
		case MultinameName, MultinameNameA:
			w.u30(mn.Name)
			w.u30(mn.NSSet)
		case MultinameNameL, MultinameNameLA:
			w.u30(mn.NSSet)
		case MultinameRTQName, MultinameRTQNameA:
			w.u30(mn.Name)
		case MultinameRTQNameL, MultinameRTQNameLA:
		case MultinameTypeName:
			w.u30(mn.Name)
			w.u30(mn.ParamCount)
			for _, p := range mn.Params {
				w.u30(p)
			}
		}
	}
}

// MultinameString 渲染多名为可读形式（供归因与列表输出）。
func (p *Pool) MultinameString(idx uint32) string {
	if idx == 0 || int(idx) > len(p.Multinames) {
		return "*"
	}
	mn := p.Multinames[idx-1]
	name := p.String(mn.Name)
	switch mn.Kind {
	case MultinameQName, MultinameQNameA:
		ns := p.String(p.namespaceName(mn.NS))
		if ns != "" {
			return ns + "." + name
		}
		return name
	case MultinameName, MultinameNameA:
		return name + ".*" // 运行时命名空间
	case MultinameNameL, MultinameNameLA, MultinameRTQNameL, MultinameRTQNameLA:
		return "<late>"
	case MultinameRTQName, MultinameRTQNameA:
		return name
	case MultinameTypeName:
		base := p.MultinameString(mn.Name)
		s := base + ".<"
		for i, prm := range mn.Params {
			if i > 0 {
				s += ","
			}
			s += p.MultinameString(prm)
		}
		return s + ">"
	}
	return fmt.Sprintf("<mn:0x%02X:%s>", mn.Kind, name)
}

func (p *Pool) namespaceName(nsIdx uint32) uint32 {
	if nsIdx == 0 || int(nsIdx) > len(p.Namespaces) {
		return 0
	}
	return p.Namespaces[nsIdx-1].Name
}

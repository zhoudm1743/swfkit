package abc

import (
	"fmt"
	"os"
)

// method_info 标志位（AVM2 spec §4.5）。
const (
	MethodNeedArguments  = 0x01
	MethodNeedActivation = 0x02
	MethodNeedRest       = 0x04
	MethodHasOptional    = 0x08
	MethodSetDXNS        = 0x40
	MethodHasParamNames  = 0x80
)

// trait 种类与属性（kind 低 4 位为种类，高 4 位为属性）。
const (
	TraitSlot     = 0
	TraitMethod   = 1
	TraitGetter   = 2
	TraitSetter   = 3
	TraitClass    = 4
	TraitFunction = 5
	TraitConst    = 6

	TraitFinal    = 0x10
	TraitOverride = 0x20
	TraitMetadata = 0x40
)

// instance_info 标志位。
const (
	ClassSealed      = 0x01
	ClassFinal       = 0x02
	ClassInterface   = 0x04
	ClassProtectedNs = 0x08
)

// Option 为 method_info 可选参数默认值。
type Option struct {
	Val  uint32 // 常量池索引
	Kind uint8  // 常量种类（ASType）
}

// MethodInfo 为 method_info 结构。
type MethodInfo struct {
	ParamTypes []uint32 // 多名索引
	ReturnType uint32   // 多名索引
	Name       uint32   // 字符串索引，0 = 匿名
	Flags      uint8
	Options    []Option
	ParamNames []uint32
}

// Trait 为 trait_info：类的成员/方法/槽位定义。
type Trait struct {
	Name uint32 // 多名索引
	Kind uint8

	// Slot / Const：
	SlotID   uint32
	TypeName uint32
	VIndex   uint32
	VKind    uint8

	// Method / Getter / Setter / Function：
	DispID uint32
	Method uint32

	// Class：
	ClassID uint32

	Metadata []uint32
}

func (t *Trait) KindBase() uint8 { return t.Kind & 0x0F }
func (t *Trait) HasMeta() bool   { return t.Kind&TraitMetadata != 0 }

// InstanceInfo 为 instance_info：类实例定义。
type InstanceInfo struct {
	Name        uint32 // 多名索引（全限定类名）
	SuperName   uint32
	Flags       uint8
	ProtectedNs uint32
	Interfaces  []uint32
	IInit       uint32 // 实例构造函数 method 索引
	Traits      []Trait
}

// ClassInfo 为 class_info：静态侧定义。
type ClassInfo struct {
	CInit  uint32 // 类初始化器 method 索引
	Traits []Trait
}

// ScriptInfo 为 script_info：一个 ABC 脚本块（全局对象）。
type ScriptInfo struct {
	Init   uint32
	Traits []Trait
}

// Exception 为 method_body 的异常处理表项（code 内绝对偏移）。
type Exception struct {
	From    uint32
	To      uint32
	Target  uint32
	ExcType uint32 // 多名索引
	VarName uint32 // 多名索引
}

// MethodBody 为 method_body：方法实现（指令流）。
type MethodBody struct {
	Method         uint32 // 对应 method_info 索引
	MaxStack       uint32
	LocalCount     uint32
	InitScopeDepth uint32
	MaxScopeDepth  uint32
	Insns          []*Insn
	Exceptions     []Exception
	Traits         []Trait
}

// MetadataInfo 为 metadata_info。
type MetadataInfo struct {
	Name   uint32
	Keys   []uint32
	Values []uint32
}

// File 为一个完整 ABC 模块（DoABC tag 的载荷）。
type File struct {
	MinorVersion uint16 // 通常 16
	MajorVersion uint16 // 通常 46
	Pool         *Pool
	Methods      []*MethodInfo
	Metadata     []*MetadataInfo
	Instances    []*InstanceInfo
	Classes      []*ClassInfo
	Scripts      []*ScriptInfo
	Bodies       []*MethodBody
}

// Parse 解析 ABC 字节流。
func Parse(data []byte) (*File, error) {
	r := &reader{data: data}
	f := &File{}
	f.MinorVersion = r.u16()
	f.MajorVersion = r.u16()
	f.Pool = parsePool(r)
	abcDebug("pool 完成 @%d", r.pos)

	if n := r.u30(); n > 0 {
		abcDebug("methods=%d @%d", n, r.pos)
		f.Methods = make([]*MethodInfo, 0, n)
		for i := uint32(0); i < n && r.err == nil; i++ {
			f.Methods = append(f.Methods, parseMethodInfo(r))
		}
	}
	abcDebug("methods 完成 @%d", r.pos)
	if n := r.u30(); n > 0 {
		abcDebug("metadata=%d @%d", n, r.pos)
		f.Metadata = make([]*MetadataInfo, 0, n)
		for i := uint32(0); i < n && r.err == nil; i++ {
			f.Metadata = append(f.Metadata, parseMetadata(r))
		}
	}
	abcDebug("metadata 完成 @%d", r.pos)
	if n := r.u30(); n > 0 {
		abcDebug("instances=%d @%d", n, r.pos)
		f.Instances = make([]*InstanceInfo, 0, n)
		for i := uint32(0); i < n && r.err == nil; i++ {
			f.Instances = append(f.Instances, parseInstance(r))
		}
	}
	abcDebug("instances 完成 @%d", r.pos)
	// 注意：class_count 不存储于文件——规范规定其必须等于 instance_count，
	// 直接复用（RABCDAsm 同样如此实现）。
	if len(f.Instances) > 0 {
		f.Classes = make([]*ClassInfo, 0, len(f.Instances))
		for i := 0; i < len(f.Instances) && r.err == nil; i++ {
			f.Classes = append(f.Classes, parseClassInfo(r))
		}
	}
	abcDebug("classes 完成 @%d", r.pos)
	if n := r.u30(); n > 0 {
		abcDebug("scripts=%d @%d", n, r.pos)
		f.Scripts = make([]*ScriptInfo, 0, n)
		for i := uint32(0); i < n && r.err == nil; i++ {
			f.Scripts = append(f.Scripts, parseScript(r))
		}
	}
	abcDebug("scripts 完成 @%d", r.pos)
	if n := r.u30(); n > 0 {
		abcDebug("bodies=%d @%d", n, r.pos)
		f.Bodies = make([]*MethodBody, 0, n)
		for i := uint32(0); i < n && r.err == nil; i++ {
			body, err := parseMethodBody(r)
			if err != nil {
				r.fail(err)
				break
			}
			f.Bodies = append(f.Bodies, body)
		}
	}
	abcDebug("bodies 完成 @%d / %d", r.pos, len(r.data))
	if r.err != nil {
		return nil, fmt.Errorf("abc: 解析失败: %w", r.err)
	}
	if r.pos != len(r.data) {
		return nil, fmt.Errorf("abc: 解析后剩余 %d 字节未消费（格式可能不受支持）", len(r.data)-r.pos)
	}
	return f, nil
}

// Serialize 序列化整个 ABC 模块。
func (f *File) Serialize() ([]byte, error) {
	w := &writer{}
	w.u16(f.MinorVersion)
	w.u16(f.MajorVersion)
	f.Pool.serialize(w)

	w.varLen(uint32(len(f.Methods)))
	for _, m := range f.Methods {
		serializeMethodInfo(w, m)
	}
	w.varLen(uint32(len(f.Metadata)))
	for _, md := range f.Metadata {
		serializeMetadata(w, md)
	}
	w.varLen(uint32(len(f.Instances)))
	for _, inst := range f.Instances {
		serializeInstance(w, inst)
	}
	// class_count 不写出（文件格式规定其等于 instance_count）
	for _, cls := range f.Classes {
		serializeClassInfo(w, cls)
	}
	w.varLen(uint32(len(f.Scripts)))
	for _, s := range f.Scripts {
		serializeScript(w, s)
	}
	w.varLen(uint32(len(f.Bodies)))
	for _, b := range f.Bodies {
		if err := serializeMethodBody(w, b); err != nil {
			return nil, err
		}
	}
	return w.buf, nil
}

func parseMethodInfo(r *reader) *MethodInfo {
	m := &MethodInfo{}
	paramCount := r.u30()
	m.ReturnType = r.u30()
	m.ParamTypes = make([]uint32, paramCount)
	for i := uint32(0); i < paramCount && r.err == nil; i++ {
		m.ParamTypes[i] = r.u30()
	}
	m.Name = r.u30()
	m.Flags = r.u8()
	if m.Flags&MethodHasOptional != 0 {
		n := r.u30()
		m.Options = make([]Option, n)
		for i := uint32(0); i < n && r.err == nil; i++ {
			m.Options[i] = Option{Val: r.u30(), Kind: r.u8()}
		}
	}
	if m.Flags&MethodHasParamNames != 0 {
		m.ParamNames = make([]uint32, paramCount)
		for i := uint32(0); i < paramCount && r.err == nil; i++ {
			m.ParamNames[i] = r.u30()
		}
	}
	return m
}

func serializeMethodInfo(w *writer, m *MethodInfo) {
	w.varLen(uint32(len(m.ParamTypes)))
	w.varLen(m.ReturnType)
	for _, p := range m.ParamTypes {
		w.varLen(p)
	}
	w.varLen(m.Name)
	w.u8(m.Flags)
	if m.Flags&MethodHasOptional != 0 {
		w.varLen(uint32(len(m.Options)))
		for _, o := range m.Options {
			w.varLen(o.Val)
			w.u8(o.Kind)
		}
	}
	if m.Flags&MethodHasParamNames != 0 {
		for _, p := range m.ParamNames {
			w.varLen(p)
		}
	}
}

func parseMetadata(r *reader) *MetadataInfo {
	md := &MetadataInfo{Name: r.u30()}
	n := r.u30()
	md.Keys = make([]uint32, n)
	md.Values = make([]uint32, n)
	for i := uint32(0); i < n && r.err == nil; i++ { // 先全部 keys
		md.Keys[i] = r.u30()
	}
	for i := uint32(0); i < n && r.err == nil; i++ { // 再全部 values
		md.Values[i] = r.u30()
	}
	return md
}

func serializeMetadata(w *writer, md *MetadataInfo) {
	w.varLen(md.Name)
	w.varLen(uint32(len(md.Keys)))
	for i := range md.Keys { // 先全部 keys
		w.varLen(md.Keys[i])
	}
	for i := range md.Keys { // 再全部 values
		w.varLen(md.Values[i])
	}
}

func parseTraits(r *reader) []Trait {
	n := r.u30()
	traits := make([]Trait, 0, n)
	for i := uint32(0); i < n && r.err == nil; i++ {
		t := Trait{Name: r.u30(), Kind: r.u8()}
		switch t.KindBase() {
		case TraitSlot, TraitConst:
			t.SlotID = r.u30()
			t.TypeName = r.u30()
			t.VIndex = r.u30()
			if t.VIndex != 0 {
				t.VKind = r.u8()
			}
		case TraitMethod, TraitGetter, TraitSetter, TraitFunction:
			t.DispID = r.u30()
			t.Method = r.u30()
		case TraitClass:
			t.SlotID = r.u30()
			t.ClassID = r.u30()
		default:
			lo := r.pos - 24
			if lo < 0 {
				lo = 0
			}
			hi := r.pos + 8
			if hi > len(r.data) {
				hi = len(r.data)
			}
			r.fail(fmt.Errorf("abc: 未知 trait 种类 0x%02X @%d，现场: % X", t.Kind, r.pos-1, r.data[lo:hi]))
			return traits
		}
		if t.HasMeta() {
			n := r.u30()
			t.Metadata = make([]uint32, n)
			for j := uint32(0); j < n && r.err == nil; j++ {
				t.Metadata[j] = r.u30()
			}
		}
		traits = append(traits, t)
	}
	return traits
}

func serializeTraits(w *writer, traits []Trait) {
	w.varLen(uint32(len(traits)))
	for i := range traits {
		t := &traits[i]
		w.varLen(t.Name)
		w.u8(t.Kind)
		switch t.KindBase() {
		case TraitSlot, TraitConst:
			w.varLen(t.SlotID)
			w.varLen(t.TypeName)
			w.varLen(t.VIndex)
			if t.VIndex != 0 {
				w.u8(t.VKind)
			}
		case TraitMethod, TraitGetter, TraitSetter, TraitFunction:
			w.varLen(t.DispID)
			w.varLen(t.Method)
		case TraitClass:
			w.varLen(t.SlotID)
			w.varLen(t.ClassID)
		}
		if t.HasMeta() {
			w.varLen(uint32(len(t.Metadata)))
			for _, m := range t.Metadata {
				w.varLen(m)
			}
		}
	}
}

func parseInstance(r *reader) *InstanceInfo {
	inst := &InstanceInfo{Name: r.u30(), SuperName: r.u30(), Flags: r.u8()}
	if inst.Flags&ClassProtectedNs != 0 {
		inst.ProtectedNs = r.u30()
	}
	n := r.u30()
	inst.Interfaces = make([]uint32, n)
	for i := uint32(0); i < n && r.err == nil; i++ {
		inst.Interfaces[i] = r.u30()
	}
	inst.IInit = r.u30()
	inst.Traits = parseTraits(r)
	return inst
}

func serializeInstance(w *writer, inst *InstanceInfo) {
	w.varLen(inst.Name)
	w.varLen(inst.SuperName)
	w.u8(inst.Flags)
	if inst.Flags&ClassProtectedNs != 0 {
		w.varLen(inst.ProtectedNs)
	}
	w.varLen(uint32(len(inst.Interfaces)))
	for _, i := range inst.Interfaces {
		w.varLen(i)
	}
	w.varLen(inst.IInit)
	serializeTraits(w, inst.Traits)
}

func parseClassInfo(r *reader) *ClassInfo {
	c := &ClassInfo{CInit: r.u30()}
	c.Traits = parseTraits(r)
	return c
}

func serializeClassInfo(w *writer, c *ClassInfo) {
	w.varLen(c.CInit)
	serializeTraits(w, c.Traits)
}

func parseScript(r *reader) *ScriptInfo {
	s := &ScriptInfo{Init: r.u30()}
	s.Traits = parseTraits(r)
	return s
}

func serializeScript(w *writer, s *ScriptInfo) {
	w.varLen(s.Init)
	serializeTraits(w, s.Traits)
}

func parseMethodBody(r *reader) (*MethodBody, error) {
	start := r.pos
	b := &MethodBody{
		Method:         r.u30(),
		MaxStack:       r.u30(),
		LocalCount:     r.u30(),
		InitScopeDepth: r.u30(),
		MaxScopeDepth:  r.u30(),
	}
	codeLen := int(r.u30())
	abcDebug("body @%d method=%d stack=%d local=%d init=%d max=%d codeLen=%d",
		start, b.Method, b.MaxStack, b.LocalCount, b.InitScopeDepth, b.MaxScopeDepth, codeLen)
	if r.err != nil {
		return nil, r.err
	}
	code := r.bytes(codeLen)
	if r.err != nil {
		return nil, r.err
	}
	insns, err := DecodeCode(code)
	if err != nil {
		return nil, fmt.Errorf("method #%d: %w", b.Method, err)
	}
	b.Insns = insns
	if n := r.u30(); n > 0 {
		b.Exceptions = make([]Exception, n)
		for i := uint32(0); i < n && r.err == nil; i++ {
			b.Exceptions[i] = Exception{
				From: r.u30(), To: r.u30(), Target: r.u30(),
				ExcType: r.u30(), VarName: r.u30(),
			}
		}
	}
	b.Traits = parseTraits(r)
	abcDebug("body method=%d 结束 @%d", b.Method, r.pos)
	return b, r.err
}

// abcDebug 为临时调试输出（SWFKIT_DEBUG=1 时启用）。
func abcDebug(format string, args ...any) {
	if os.Getenv("SWFKIT_DEBUG") == "1" {
		fmt.Printf("DBG "+format+"\n", args...)
	}
}

func serializeMethodBody(w *writer, b *MethodBody) error {
	code, om, err := EncodeCode(b.Insns)
	if err != nil {
		return err
	}
	w.varLen(b.Method)
	w.varLen(b.MaxStack)
	w.varLen(b.LocalCount)
	w.varLen(b.InitScopeDepth)
	w.varLen(b.MaxScopeDepth)
	w.varLen(uint32(len(code)))
	w.raw(code)
	w.varLen(uint32(len(b.Exceptions)))
	for _, e := range b.Exceptions {
		w.varLen(uint32(om.resolve(int(e.From))))
		w.varLen(uint32(om.resolve(int(e.To))))
		w.varLen(uint32(om.resolve(int(e.Target))))
		w.varLen(e.ExcType)
		w.varLen(e.VarName)
	}
	serializeTraits(w, b.Traits)
	return nil
}

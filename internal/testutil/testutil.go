// Package testutil 构建测试用的最小 SWF/ABC/SOL 样本。
// 仅使用各包导出 API：方法体指令流通过手汇字节码 + abc.DecodeCode 生成，
// 因此 ABC 序列化链路本身也被测试覆盖。
package testutil

import (
	"swfkit/internal/abc"
	"swfkit/internal/swf"
)

// varLen 编码变长 u30。
func varLen(v uint32) []byte {
	var out []byte
	for {
		if v < 0x80 {
			return append(out, byte(v))
		}
		out = append(out, byte(v&0x7F)|0x80)
		v >>= 7
	}
}

// insn 为测试汇编器的指令描述（label 用于分支目标）。
type insn struct {
	op    byte
	u30   []uint32
	u8    []byte
	label string // 分支目标标签（分支类指令）
}

// asm 两遍汇编：先定尺寸，再解析标签填 delta。
// 标签哨兵 {op:0xFF, u8:标签名} 在两遍中均为零长占位，不产出字节。
func asm(code []insn) []byte {
	// 第一遍：偏移与尺寸
	offs := make([]int, len(code))
	off := 0
	for i, in := range code {
		offs[i] = off
		if in.op == 0xFF { // 标签哨兵零长
			continue
		}
		size := 1
		if in.label != "" {
			size = 4
		} else {
			for _, v := range in.u30 {
				size += len(varLen(v))
			}
			size += len(in.u8)
		}
		off += size
	}
	// 第二遍：编码
	var out []byte
	for i, in := range code {
		if in.op == 0xFF {
			continue
		}
		out = append(out, in.op)
		if in.label != "" {
			t := labelOff(code, offs, in.label)
			d := int32(t - (offs[i] + 4)) // 分支偏移相对指令末尾
			out = append(out, byte(d), byte(d>>8), byte(d>>16))
			continue
		}
		for _, v := range in.u30 {
			out = append(out, varLen(v)...)
		}
		out = append(out, in.u8...)
	}
	return out
}

func labelOff(code []insn, offs []int, label string) int {
	for i, in := range code {
		if in.op == 0xFF && in.u8 != nil && string(in.u8) == label { // 0xFF 为标签哨兵
			return offs[i]
		}
	}
	panic("testutil: 未知标签 " + label)
}

// BuildGameABC 构建含 com.test.Player 类的 ABC 模块字节流：
// addMoney 方法包含 pushbyte/pushshort/pushint/pushdouble/pushstring、
// 分支、try/catch（异常表），覆盖补丁引擎的全部关键路径。
func BuildGameABC() []byte {
	f := &abc.File{MinorVersion: 16, MajorVersion: 46, Pool: &abc.Pool{}}
	nsCom := f.Pool.AddString("com.test") // 字符串 1
	strPlayer := f.Pool.AddString("Player")
	strAddMoney := f.Pool.AddString("addMoney")
	f.Pool.AddString("gold") // 字符串 4

	nsIdx := uint32(len(f.Pool.Namespaces) + 1)
	f.Pool.Namespaces = append(f.Pool.Namespaces, abc.Namespace{Kind: 0x16, Name: nsCom})

	qname := func(name uint32) uint32 {
		f.Pool.Multinames = append(f.Pool.Multinames, abc.Multiname{Kind: 0x07, NS: nsIdx, Name: name})
		return uint32(len(f.Pool.Multinames))
	}
	mnPlayer := qname(strPlayer)
	mnAddMoney := qname(strAddMoney)

	intIdx := f.Pool.AddInt(777)
	dblIdx := f.Pool.AddDouble(3.14)

	richInsns := []insn{
		{op: 0xD0},
		{op: 0x30},
		{op: 0x24, u8: []byte{100}},       // pushbyte 100
		{op: 0x25, u30: []uint32{30000}},  // pushshort 30000
		{op: 0x2D, u30: []uint32{intIdx}}, // pushint 777
		{op: 0x2F, u30: []uint32{dblIdx}}, // pushdouble 3.14
		{op: 0x2C, u30: []uint32{4}},      // pushstring "gold"
		{op: 0x11, label: "ok"},           // iftrue ok
		{op: 0x24, u8: []byte{1}},
		{op: 0x29},                   // pop
		{op: 0xFF, u8: []byte("ok")}, // 标签哨兵
		{op: 0x29},                   // pop
		{op: 0x24, u8: []byte{9}},    // pushbyte 9 —— try 体
		{op: 0x29},
		{op: 0x10, label: "end"}, // jump end（跳过 handler）
		{op: 0xFF, u8: []byte("handler")},
		{op: 0x24, u8: []byte{2}}, // handler: pushbyte 2
		{op: 0x29},
		{op: 0xFF, u8: []byte("end")},
		{op: 0x47}, // returnvoid
	}
	decode := func(code []insn) []*abc.Insn {
		ins, err := abc.DecodeCode(asm(code))
		if err != nil {
			panic(err)
		}
		return ins
	}

	method := func(name uint32) uint32 {
		f.Methods = append(f.Methods, &abc.MethodInfo{Name: name})
		return uint32(len(f.Methods) - 1)
	}

	traitMethod := func(name, m uint32) abc.Trait {
		return abc.Trait{Name: name, Kind: abc.TraitMethod, DispID: 0, Method: m}
	}

	mCinit := method(0)
	mIinit := method(0)
	mAdd := method(strAddMoney)
	mScript := method(0)

	f.Instances = append(f.Instances, &abc.InstanceInfo{
		Name: mnPlayer, SuperName: 0, Flags: abc.ClassSealed,
		IInit:  mIinit,
		Traits: []abc.Trait{traitMethod(mnAddMoney, mAdd)},
	})
	f.Classes = append(f.Classes, &abc.ClassInfo{CInit: mCinit})
	f.Scripts = append(f.Scripts, &abc.ScriptInfo{Init: mScript})

	// 方法体：与 method 索引对应
	for mi := range f.Methods {
		var ins []*abc.Insn
		switch uint32(mi) {
		case mCinit, mIinit, mScript:
			ins = decode([]insn{{op: 0xD0}, {op: 0x30}, {op: 0x47}})
		case mAdd:
			ins = decode(richInsns)
		}
		body := &abc.MethodBody{Method: uint32(mi), MaxStack: 2, LocalCount: 1, Insns: ins}
		if uint32(mi) == mAdd {
			// 异常表：try 覆盖 pushbyte 9，handler 位于 jump 之后的
			// 不可达区（DecodeCode 将其保留为 raw），目标即 raw 区起点。
			handler := -1
			for _, in := range ins {
				if in.Op == abc.OpRaw && len(in.Raw) >= 2 && in.Raw[0] == 0x24 && int8(in.Raw[1]) == 2 {
					handler = in.Offs
					break
				}
			}
			if handler < 0 {
				panic("testutil: 未找到 handler raw 区")
			}
			tryStart := findOff(ins, 0x24, 9)
			tryEnd := nextOff(ins, tryStart)
			body.Exceptions = []abc.Exception{{
				From: uint32(tryStart), To: uint32(tryEnd), Target: uint32(handler),
			}}
		}
		f.Bodies = append(f.Bodies, body)
	}

	data, err := f.Serialize()
	if err != nil {
		panic(err)
	}
	return data
}

func findOff(ins []*abc.Insn, op byte, v int64) int {
	for _, in := range ins {
		if in.Op == op {
			var val int64
			if op == 0x24 {
				val = int64(int8(in.U8s[0]))
			} else {
				val = int64(int32(in.U30s[0]))
			}
			if val == v {
				return in.Offs
			}
		}
	}
	panic("testutil: findOff 未找到")
}

func nextOff(ins []*abc.Insn, after int) int {
	for _, in := range ins {
		if in.Offs == after {
			return in.Offs + in.Size
		}
	}
	panic("testutil: nextOff 未找到")
}

// BuildGameSWF 将游戏 ABC 包装为 CWS（zlib）SWF。
func BuildGameSWF() []byte {
	abcData := BuildGameABC()
	tag := &swf.Tag{Code: 82}
	tag.Data = []byte{0, 0, 0, 0} // flags = 0
	tag.Data = append(tag.Data, "game"...)
	tag.Data = append(tag.Data, 0)
	tag.Data = append(tag.Data, abcData...)

	f := &swf.File{Header: &swf.Header{
		Signature:  swf.SigZlib,
		Version:    32,
		FrameSize:  swf.Rect{Nbits: 15, XMin: 0, XMax: 800 * 20, YMin: 0, YMax: 600 * 20},
		FrameRate:  24 << 8,
		FrameCount: 1,
	}}
	f.Tags = []*swf.Tag{
		{Code: 9}, // SetBackgroundColor（空数据仅作占位）
		tag,
		{Code: 1}, // ShowFrame
		{Code: 0}, // End
	}
	data, err := f.Save()
	if err != nil {
		panic(err)
	}
	return data
}

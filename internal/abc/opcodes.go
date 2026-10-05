package abc

import "fmt"

// ArgKind 为指令操作数编码种类（对应 AVM2 指令表的参数类型）。
type ArgKind uint8

const (
	argNone ArgKind = iota // 之后无更多操作数
	argU30                 // 变长 u30（cpool 索引 / 寄存器 / 变长字面量）
	argU8                  // 裸 1 字节
	argU16                 // 裸 2 字节（小端）
	argS24                 // 3 字节有符号分支偏移（相对指令末尾）
)

// OpInfo 为单条指令的元信息。
type OpInfo struct {
	Name   string
	Args   [4]ArgKind // 最多 4 个操作数（debug 指令占满）
	Jump   bool       // 含 JumpTarget（s24，相对指令末尾）
	Switch bool       // lookupswitch 特殊格式
	Valid  bool       // 0x00 与未定义操作码不可解码
}

// opcodeTable 为 AVM2 完整操作码表。
// 依据 Adobe《AVM2 Overview》指令集，并逐项对照 RABCDAsm（业界事实标准实现）
// 的 opcodeInfo 表转录核对（2026-10，含 initproperty 单操作数、debug 四操作数、
// getscopeobject 裸字节等易错项）；少数 RABCDAsm 标 Unknown 的罕见指令按
// Tamarin 解释器实现补齐（bkpt=u8、bkptline=u30 等）。
var opcodeTable = [256]OpInfo{}

func init() {
	set := func(op byte, name string, args ...ArgKind) {
		opcodeTable[op] = OpInfo{Name: name, Valid: true}
		copy(opcodeTable[op].Args[:], args)
	}
	setJump := func(op byte, name string) {
		opcodeTable[op] = OpInfo{Name: name, Valid: true, Jump: true, Args: [4]ArgKind{argS24}}
	}
	invalid := func(op byte) {
		opcodeTable[op] = OpInfo{Name: fmt.Sprintf("op_%02X", op)}
	}

	// 0x00 为非法定义（RABCDAsm 的 "db" 伪指令），真实代码流不应出现
	invalid(0x00)
	set(0x01, "bkpt", argU8)
	set(0x02, "nop")
	set(0x03, "throw")
	set(0x04, "getsuper", argU30)
	set(0x05, "setsuper", argU30)
	set(0x06, "dxns", argU30)
	set(0x07, "dxnslate")
	set(0x08, "kill", argU30)
	set(0x09, "label")
	invalid(0x0A)
	invalid(0x0B)
	setJump(0x0C, "ifnlt")
	setJump(0x0D, "ifnle")
	setJump(0x0E, "ifngt")
	setJump(0x0F, "ifnge")
	setJump(0x10, "jump")
	setJump(0x11, "iftrue")
	setJump(0x12, "iffalse")
	setJump(0x13, "ifeq")
	setJump(0x14, "ifne")
	setJump(0x15, "iflt")
	setJump(0x16, "ifle")
	setJump(0x17, "ifgt")
	setJump(0x18, "ifge")
	setJump(0x19, "ifstricteq")
	setJump(0x1A, "ifstrictne")
	// lookupswitch：s24 默认目标 + u30 N + N+1 个 s24（均相对指令起点）
	opcodeTable[0x1B] = OpInfo{Name: "lookupswitch", Valid: true, Switch: true}
	set(0x1C, "pushwith")
	set(0x1D, "popscope")
	set(0x1E, "nextname")
	set(0x1F, "hasnext")
	set(0x20, "pushnull")
	set(0x21, "pushundefined")
	set(0x22, "pushuninitialized")
	set(0x23, "nextvalue")
	set(0x24, "pushbyte", argU8)
	set(0x25, "pushshort", argU30)
	set(0x26, "pushtrue")
	set(0x27, "pushfalse")
	set(0x28, "pushnan")
	set(0x29, "pop")
	set(0x2A, "dup")
	set(0x2B, "swap")
	set(0x2C, "pushstring", argU30)
	set(0x2D, "pushint", argU30)
	set(0x2E, "pushuint", argU30)
	set(0x2F, "pushdouble", argU30)
	set(0x30, "pushscope")
	set(0x31, "pushnamespace", argU30)
	set(0x32, "hasnext2", argU30, argU30)
	set(0x33, "pushdecimal", argU30)
	set(0x34, "pushdnan")
	for op := 0x35; op <= 0x3E; op++ { // li8..sf64 内存访问（Alchemy）
		invalid(byte(op))
	}
	invalid(0x3F)
	set(0x40, "newfunction", argU30)
	set(0x41, "call", argU30)
	set(0x42, "construct", argU30)
	set(0x43, "callmethod", argU30, argU30)
	set(0x44, "callstatic", argU30, argU30)
	set(0x45, "callsuper", argU30, argU30)
	set(0x46, "callproperty", argU30, argU30)
	set(0x47, "returnvoid")
	set(0x48, "returnvalue")
	set(0x49, "constructsuper", argU30)
	set(0x4A, "constructprop", argU30, argU30)
	set(0x4B, "callsuperid", argU30, argU30)
	set(0x4C, "callproplex", argU30, argU30)
	set(0x4D, "callinterface", argU30)
	set(0x4E, "callsupervoid", argU30, argU30)
	set(0x4F, "callpropvoid", argU30, argU30)
	set(0x50, "sxi1")
	set(0x51, "sxi8")
	set(0x52, "sxi16")
	set(0x53, "applytype", argU30)
	invalid(0x54)
	set(0x55, "newobject", argU30)
	set(0x56, "newarray", argU30)
	set(0x57, "newactivation")
	set(0x58, "newclass", argU30)
	set(0x59, "getdescendants", argU30)
	set(0x5A, "newcatch", argU30)
	set(0x5B, "deldescendants", argU30)
	invalid(0x5C)
	set(0x5D, "findpropstrict", argU30)
	set(0x5E, "findproperty", argU30)
	set(0x5F, "finddef", argU30)
	set(0x60, "getlex", argU30)
	set(0x61, "setproperty", argU30)
	set(0x62, "getlocal", argU30)
	set(0x63, "setlocal", argU30)
	set(0x64, "getglobalscope")
	set(0x65, "getscopeobject", argU8)
	set(0x66, "getproperty", argU30)
	set(0x67, "getouterscope", argU30)
	set(0x68, "initproperty", argU30)
	set(0x69, "setpropertylate")
	set(0x6A, "deleteproperty", argU30)
	set(0x6B, "deletepropertylate")
	set(0x6C, "getslot", argU30)
	set(0x6D, "setslot", argU30)
	set(0x6E, "getglobalslot", argU30)
	set(0x6F, "setglobalslot", argU30)
	set(0x70, "convert_s")
	set(0x71, "esc_xelem")
	set(0x72, "esc_xattr")
	set(0x73, "convert_i")
	set(0x74, "convert_u")
	set(0x75, "convert_d")
	set(0x76, "convert_b")
	set(0x77, "convert_o")
	set(0x78, "checkfilter")
	set(0x79, "convert_m")
	set(0x7A, "convert_m_p", argU30)
	for op := 0x7B; op <= 0x7F; op++ {
		invalid(byte(op))
	}
	set(0x80, "coerce", argU30)
	set(0x81, "coerce_b")
	set(0x82, "coerce_a")
	set(0x83, "coerce_i")
	set(0x84, "coerce_d")
	set(0x85, "coerce_s")
	set(0x86, "astype", argU30)
	set(0x87, "astypelate")
	set(0x88, "coerce_u", argU30)
	set(0x89, "coerce_o")
	for op := 0x8A; op <= 0x8E; op++ {
		invalid(byte(op))
	}
	set(0x8F, "negate_p")
	set(0x90, "negate")
	set(0x91, "increment")
	set(0x92, "inclocal", argU30)
	set(0x93, "decrement")
	set(0x94, "declocal", argU30)
	set(0x95, "typeof")
	set(0x96, "not")
	set(0x97, "bitnot")
	invalid(0x98)
	invalid(0x99)
	set(0x9A, "concat")
	set(0x9B, "add_d")
	set(0x9C, "increment_p")
	set(0x9D, "inclocal_p", argU30)
	set(0x9E, "decrement_p")
	set(0x9F, "declocal_p", argU30)
	set(0xA0, "add")
	set(0xA1, "subtract")
	set(0xA2, "multiply")
	set(0xA3, "divide")
	set(0xA4, "modulo")
	set(0xA5, "lshift")
	set(0xA6, "rshift")
	set(0xA7, "urshift")
	set(0xA8, "bitand")
	set(0xA9, "bitor")
	set(0xAA, "bitxor")
	set(0xAB, "equals")
	set(0xAC, "strictequals")
	set(0xAD, "lessthan")
	set(0xAE, "lessequals")
	set(0xAF, "greaterthan")
	set(0xB0, "greaterequals")
	set(0xB1, "instanceof")
	set(0xB2, "istype", argU30)
	set(0xB3, "istypelate")
	set(0xB4, "in")
	set(0xB5, "add_p")
	set(0xB6, "subtract_p")
	set(0xB7, "multiply_p")
	set(0xB8, "divide_p")
	set(0xB9, "modulo_p")
	for op := 0xBA; op <= 0xBF; op++ {
		invalid(byte(op))
	}
	set(0xC0, "increment_i")
	set(0xC1, "decrement_i")
	set(0xC2, "inclocal_i", argU30)
	set(0xC3, "declocal_i", argU30)
	set(0xC4, "negate_i")
	set(0xC5, "add_i")
	set(0xC6, "subtract_i")
	set(0xC7, "multiply_i")
	for op := 0xC8; op <= 0xCF; op++ {
		invalid(byte(op))
	}
	set(0xD0, "getlocal0")
	set(0xD1, "getlocal1")
	set(0xD2, "getlocal2")
	set(0xD3, "getlocal3")
	set(0xD4, "setlocal0")
	set(0xD5, "setlocal1")
	set(0xD6, "setlocal2")
	set(0xD7, "setlocal3")
	for op := 0xD8; op <= 0xEE; op++ {
		invalid(byte(op))
	}
	// debug：u8 调试类型 + u30 符号索引 + u8 寄存器 + u30 附加
	opcodeTable[0xEF] = OpInfo{Name: "debug", Valid: true, Args: [4]ArgKind{argU8, argU30, argU8, argU30}}
	set(0xF0, "debugline", argU30)
	set(0xF1, "debugfile", argU30)
	set(0xF2, "bkptline", argU30)
	set(0xF3, "timestamp")
	for op := 0xF4; op <= 0xFF; op++ {
		invalid(byte(op))
	}
}

// OpName 返回指令助记名（未知操作码返回空串）。
func OpName(op byte) string { return opcodeTable[op].Name }

// OpRaw 为内部伪指令：表示代码流中无法解码的原始字节区（死代码/内嵌数据），
// 写回时原样透传，保证有瑕疵文件也能无损往返。
const OpRaw byte = 0x00

// insnTerminates 报告指令是否终结基本块（无穿透执行）。
func insnTerminates(op byte) bool {
	switch op {
	case 0x02, 0x10, 0x47, 0x48, 0x1B: // throw / jump / returnvoid / returnvalue / lookupswitch
		return true
	}
	return false
}

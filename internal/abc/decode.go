package abc

import (
	"fmt"
	"sort"
)

// Insn 为解码后的一条指令。Offs/Size 描述其在原始 code 流中的位置；
// 补丁改变编码长度后写回时按新偏移重算所有跳转与异常边界。
type Insn struct {
	Op   byte
	Offs int // 原始 code 内偏移（指令起点）
	Size int // 原始编码总字节数

	U30s [4]uint32 // argU30 操作数值
	U8s  [4]byte   // argU8 操作数值
	Raw  []byte    // OpRaw 伪指令的原始字节

	// 跳转目标（解码时已换算为绝对 code 偏移）：
	JumpTarget    int   // 分支指令目标（相对指令末尾）
	SwitchDefault int   // lookupswitch 默认目标（相对指令起点）
	SwitchCases   []int // lookupswitch case 目标（共 N+1 个）
}

func (in *Insn) Name() string {
	if in.Op == OpRaw {
		return "<raw>"
	}
	return OpName(in.Op)
}

// DecodeCode 以踪迹式算法解码 AVM2 指令流：
// 从入口 0 与全部跳转/switch 目标出发遍历可达代码；不可达区（死代码、
// 编译器内嵌数据表）保持原始字节，写回时透传。这与 RABCDAsm 的做法一致，
// 可正确处理混淆器产出的非常规代码。
func DecodeCode(code []byte) ([]*Insn, error) {
	const (
		stUnknown = iota // 未访问
		stBody           // 指令内部字节
		stStart          // 指令起始字节（已入队）
	)
	if len(code) == 0 {
		return nil, nil
	}
	state := make([]uint8, len(code))
	var pending []int
	var decoded []*Insn
	queued := map[int]bool{}

	// queue 将偏移加入待解码队列；跳入已解码指令中间视为非法
	// （解码全部结束后还有一次终检，防止"先入队、后被前序指令覆盖"的顺序问题）。
	queue := func(off int) error {
		if off < 0 || off > len(code) {
			return fmt.Errorf("跳转目标 %d 越界（code 长度 %d）", off, len(code))
		}
		if off == len(code) { // 指向 code 末尾合法
			return nil
		}
		if state[off] == stBody {
			return fmt.Errorf("跳转目标 %d 落在指令中间", off)
		}
		if state[off] != stUnknown {
			return nil // 已入队/已解码
		}
		state[off] = stStart
		queued[off] = true
		pending = append(pending, off)
		return nil
	}

	decodeAt := func(off int) error {
		if state[off] == stBody {
			// 入队时还不是指令内部，但已被更早起始的指令覆盖
			return fmt.Errorf("跳转目标 %d 落在指令中间", off)
		}
		info := opcodeTable[code[off]]
		if !info.Valid {
			return fmt.Errorf("偏移 %d 出现不可解码操作码 0x%02X（%s）", off, code[off], info.Name)
		}
		in := &Insn{Op: code[off], Offs: off}
		rd := &reader{data: code, pos: off + 1}
		if info.Switch {
			def := rd.s24()
			if rd.err != nil {
				return fmt.Errorf("lookupswitch 操作数截断: %w", rd.err)
			}
			in.SwitchDefault = off + int(def)
			n := int(rd.u30())
			if rd.err != nil || n < 0 || n > 1<<20 {
				return fmt.Errorf("lookupswitch case 数异常: %d", n)
			}
			for i := 0; i <= n; i++ {
				d := rd.s24()
				if rd.err != nil {
					return fmt.Errorf("lookupswitch case 截断: %w", rd.err)
				}
				in.SwitchCases = append(in.SwitchCases, off+int(d))
			}
		} else {
			for slot := 0; slot < 4; slot++ {
				switch kind := info.Args[slot]; kind {
				case argNone:
					slot = 4 // 结束操作数循环
					continue
				case argU30:
					in.U30s[slot] = rd.varLen()
				case argU8:
					in.U8s[slot] = rd.u8()
				case argU16:
					b := rd.bytes(2)
					if rd.err == nil {
						in.U8s[slot] = b[0]
						if slot+1 >= 4 {
							return fmt.Errorf("argU16 操作数槽位溢出")
						}
						in.U8s[slot+1] = b[1]
					}
				case argS24:
					d := rd.s24()
					in.U30s[3] = uint32(d) // 暂存 delta，Size 确定后换算绝对目标
				}
				if rd.err != nil {
					return fmt.Errorf("指令 %s 操作数截断: %w", info.Name, rd.err)
				}
			}
		}
		in.Size = rd.pos - off
		if in.Size > len(code)-off {
			return fmt.Errorf("指令 %s 越过 code 末尾", info.Name)
		}
		if info.Jump {
			in.JumpTarget = off + in.Size + int(int32(in.U30s[3]))
		}
		for i := off; i < off+in.Size; i++ {
			state[i] = stBody
		}
		state[off] = stStart
		decoded = append(decoded, in)

		// 后继：跳转目标与穿透路径
		if info.Switch {
			for _, t := range append([]int{in.SwitchDefault}, in.SwitchCases...) {
				if err := queue(t); err != nil {
					return fmt.Errorf("lookupswitch: %w", err)
				}
			}
		} else {
			if info.Jump {
				if err := queue(in.JumpTarget); err != nil {
					return fmt.Errorf("%s: %w", info.Name, err)
				}
			}
			if !insnTerminates(in.Op) {
				if err := queue(off + in.Size); err != nil {
					return err
				}
			}
		}
		return nil
	}

	if err := queue(0); err != nil {
		return nil, err
	}
	for len(pending) > 0 {
		off := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if err := decodeAt(off); err != nil {
			return nil, fmt.Errorf("abc: code 偏移 %d: %w", off, err)
		}
	}
	// 终检：全部解码完成后，每个入队目标必须仍是某条指令的起点
	for off := range queued {
		if state[off] != stStart {
			return nil, fmt.Errorf("abc: 跳转目标 %d 落在指令中间", off)
		}
	}

	// 按偏移排序；未访问空洞作为 raw 伪指令补齐
	sort.Slice(decoded, func(i, j int) bool { return decoded[i].Offs < decoded[j].Offs })
	result := make([]*Insn, 0, len(decoded)+2)
	pos := 0
	for _, in := range decoded {
		if in.Offs > pos {
			result = append(result, &Insn{Op: OpRaw, Offs: pos, Size: in.Offs - pos, Raw: code[pos:in.Offs:in.Offs]})
		}
		result = append(result, in)
		pos = in.Offs + in.Size
	}
	if pos < len(code) {
		result = append(result, &Insn{Op: OpRaw, Offs: pos, Size: len(code) - pos, Raw: code[pos:len(code):len(code)]})
	}
	return result, nil
}

// offsetMap 将旧 code 偏移映射到新 code 偏移。
type offsetMap struct {
	m      map[int]int
	starts []int // 排序的旧指令起点（异常边界回退用）
}

func (om *offsetMap) resolve(old int) int {
	if v, ok := om.m[old]; ok {
		return v
	}
	// 回退到最近的指令起点（异常边界指向指令中间时按起点处理，与 RABCDAsm 一致）
	idx := sort.SearchInts(om.starts, old) - 1
	if idx >= 0 {
		return om.m[om.starts[idx]]
	}
	return old
}

// EncodeCode 重编码指令流，返回新 code 字节与偏移映射。
func EncodeCode(ins []*Insn) ([]byte, *offsetMap, error) {
	// 第一遍：逐条编码并计算新偏移（分支/switch 大小与位置无关）
	newOffs := make([]int, len(ins)+1)
	encoded := make([][]byte, len(ins))
	pos := 0
	for i, in := range ins {
		newOffs[i] = pos
		b, err := encodeInsn(in)
		if err != nil {
			return nil, nil, fmt.Errorf("abc: 旧偏移 %d 指令 %s: %w", in.Offs, in.Name(), err)
		}
		encoded[i] = b
		pos += len(b)
	}
	total := pos

	om := &offsetMap{m: make(map[int]int, len(ins)*2+1)}
	for i, in := range ins {
		if in.Op == OpRaw {
			for k := 0; k < len(in.Raw); k++ {
				om.m[in.Offs+k] = newOffs[i] + k
			}
		} else {
			om.m[in.Offs] = newOffs[i]
			om.starts = append(om.starts, in.Offs)
		}
	}
	endOld := 0
	if len(ins) > 0 {
		last := ins[len(ins)-1]
		endOld = last.Offs + last.Size
	}
	om.m[endOld] = total // 旧 code 末尾（跳转到末尾的合法目标）

	// 第二遍：写出并重算跳转 delta
	out := make([]byte, 0, total)
	for i, in := range ins {
		start := newOffs[i]
		switch {
		case in.Op == OpRaw:
			out = append(out, in.Raw...)
		case opcodeTable[in.Op].Switch:
			out = append(out, in.Op)
			out = appendS24(out, int32(om.resolve(in.SwitchDefault)-start))
			out = varLenAppend(out, uint32(len(in.SwitchCases)-1))
			for _, c := range in.SwitchCases {
				out = appendS24(out, int32(om.resolve(c)-start))
			}
		case opcodeTable[in.Op].Jump:
			out = append(out, in.Op)
			out = appendS24(out, int32(om.resolve(in.JumpTarget)-(start+4))) // 相对新指令末尾
		default:
			out = append(out, encoded[i]...)
		}
	}
	return out, om, nil
}

func encodeInsn(in *Insn) ([]byte, error) {
	if in.Op == OpRaw {
		return in.Raw, nil
	}
	info := opcodeTable[in.Op]
	if !info.Valid {
		return nil, fmt.Errorf("无效操作码 0x%02X", in.Op)
	}
	out := []byte{in.Op}
	if info.Switch {
		// 用占位 delta 编码以保证尺寸正确；最终内容由 EncodeCode 第二遍重写
		out = appendS24(out, 0)
		out = varLenAppend(out, uint32(len(in.SwitchCases)-1))
		for range in.SwitchCases {
			out = appendS24(out, 0)
		}
		return out, nil
	}
	if info.Jump {
		return appendS24(out, 0), nil
	}
	for slot := 0; slot < 4; slot++ {
		switch kind := info.Args[slot]; kind {
		case argNone:
			slot = 4
			continue
		case argU30:
			out = varLenAppend(out, in.U30s[slot])
		case argU8:
			out = append(out, in.U8s[slot])
		case argU16:
			if slot+1 >= 4 {
				return nil, fmt.Errorf("argU16 操作数槽位溢出")
			}
			out = append(out, in.U8s[slot], in.U8s[slot+1])
		}
	}
	return out, nil
}

func appendS24(b []byte, v int32) []byte {
	return append(b, byte(v), byte(v>>8), byte(v>>16))
}

func varLenAppend(b []byte, v uint32) []byte {
	for {
		if v < 0x80 {
			return append(b, byte(v))
		}
		b = append(b, byte(v&0x7F)|0x80)
		v >>= 7
	}
}

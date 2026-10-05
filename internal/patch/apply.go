package patch

import (
	"fmt"
	"math"

	"swfkit/internal/abc"
	"swfkit/internal/swf"
)

// Op 为一次补丁操作。
type Op struct {
	Hit       *Hit
	NewInt    int64   // 整型新值（IsFloat=false 时生效）
	NewDouble float64 // 浮点新值（IsFloat=true 时生效）
}

// Apply 将补丁写入 SWF（就地修改 tag 数据；调用方随后 Save 落盘）。
// 策略：优先复用常量池已有等值条目；否则追加新条目并重定向该条指令，
// 绝不改动共享常量本身，避免波及未知引用。
func Apply(sw *swf.File, ops []*Op) error {
	if len(ops) == 0 {
		return nil
	}
	byTag := map[int][]*Op{}
	for _, op := range ops {
		if op.Hit == nil {
			return fmt.Errorf("补丁缺少命中位置")
		}
		byTag[op.Hit.ABCIdx] = append(byTag[op.Hit.ABCIdx], op)
	}
	tags, err := abc.ParseAll(sw)
	if err != nil {
		return err
	}
	for ai, ops := range byTag {
		if ai < 0 || ai >= len(tags) {
			return fmt.Errorf("DoABC 序号 %d 越界", ai)
		}
		t := tags[ai]
		for _, op := range ops {
			if err := applyOne(t, op); err != nil {
				return fmt.Errorf("DoABC %q: %w", t.Name, err)
			}
		}
		data, err := t.Serialize()
		if err != nil {
			return fmt.Errorf("DoABC %q 序列化失败: %w", t.Name, err)
		}
		sw.Tags[t.Index].Data = data
	}
	return nil
}

func applyOne(t *abc.TagABC, op *Op) error {
	f := t.File
	if op.Hit.BodyIdx >= len(f.Bodies) {
		return fmt.Errorf("method_body 下标 %d 越界", op.Hit.BodyIdx)
	}
	body := f.Bodies[op.Hit.BodyIdx]
	if op.Hit.InsnIdx >= len(body.Insns) {
		return fmt.Errorf("指令下标 %d 越界", op.Hit.InsnIdx)
	}
	in := body.Insns[op.Hit.InsnIdx]

	if op.Hit.IsFloat {
		switch in.Op {
		case opPushDouble:
			in.U30s[0] = findOrAddDouble(f.Pool, op.NewDouble)
			return nil
		default:
			return fmt.Errorf("命中类型 %s 与浮点补丁不匹配", in.Name())
		}
	}

	nv := op.NewInt
	switch in.Op {
	case opPushByte:
		switch {
		case nv >= math.MinInt8 && nv <= math.MaxInt8:
			in.U8s[0] = byte(nv)
		case nv >= -32768 && nv <= 65535:
			upgrade(in, opPushShort, uint32(int32(nv)))
		default:
			in.U30s[0] = findOrAddInt(f.Pool, nv)
			in.Op = opPushInt
		}
	case opPushShort:
		if nv >= -32768 && nv <= 65535 {
			in.U30s[0] = uint32(int32(nv))
		} else {
			in.U30s[0] = findOrAddInt(f.Pool, nv)
			in.Op = opPushInt
		}
	case opPushInt:
		if nv < math.MinInt32 || nv > math.MaxInt32 {
			return fmt.Errorf("新值 %d 超出 int32 范围；该位置为整型压栈，如需更大数值请确认游戏以 Number 存储", nv)
		}
		in.U30s[0] = findOrAddInt(f.Pool, nv)
	case opPushUInt:
		if nv < 0 || nv > math.MaxUint32 {
			return fmt.Errorf("新值 %d 超出 uint32 范围", nv)
		}
		in.U30s[0] = findOrAddUInt(f.Pool, uint32(nv))
	case opPushDouble:
		// 整型补丁打到浮点指令：按整数写入 double 值
		in.U30s[0] = findOrAddDouble(f.Pool, float64(nv))
	default:
		return fmt.Errorf("命中指令 %s 不是数值压栈指令", in.Name())
	}
	return nil
}

// upgrade 将窄化指令升位（pushbyte → pushshort）。
func upgrade(in *abc.Insn, newOp byte, v uint32) {
	in.Op = newOp
	in.U30s = [4]uint32{}
	in.U8s = [4]byte{}
	in.U30s[0] = v
}

func findOrAddInt(p *abc.Pool, v int64) uint32 {
	if v < math.MinInt32 || v > math.MaxInt32 {
		// 调用方已保证范围；防御性截断会改变语义，这里直接回退为按低位写入
		v = int64(int32(v))
	}
	want := int32(v)
	for i, x := range p.Ints {
		if x == want {
			return uint32(i + 1)
		}
	}
	return p.AddInt(want)
}

func findOrAddUInt(p *abc.Pool, v uint32) uint32 {
	for i, x := range p.UInts {
		if x == v {
			return uint32(i + 1)
		}
	}
	return p.AddUInt(v)
}

func findOrAddDouble(p *abc.Pool, v float64) uint32 {
	for i, x := range p.Doubles {
		if x == v {
			return uint32(i + 1)
		}
	}
	return p.AddDouble(v)
}

// ClassEntry 为 abc-list 输出的类条目。
type ClassEntry struct {
	ABCIdx      int
	ABCName     string
	Name        string
	Super       string
	IsInterface bool
	Traits      []string
}

// ListClasses 枚举全部 ABC 模块的类定义。
func ListClasses(sw *swf.File) ([]ClassEntry, error) {
	tags, err := abc.ParseAll(sw)
	if err != nil {
		return nil, err
	}
	var out []ClassEntry
	for ai, t := range tags {
		for _, inst := range t.File.Instances {
			e := ClassEntry{
				ABCIdx:      ai,
				ABCName:     t.Name,
				Name:        t.File.Pool.MultinameString(inst.Name),
				Super:       t.File.Pool.MultinameString(inst.SuperName),
				IsInterface: inst.Flags&abc.ClassInterface != 0,
			}
			for ti := range inst.Traits {
				e.Traits = append(e.Traits, shortName(t.File, inst.Traits[ti].Name))
			}
			out = append(out, e)
		}
	}
	return out, nil
}

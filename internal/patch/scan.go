package patch

import (
	"fmt"
	"math"

	"swfkit/internal/abc"
	"swfkit/internal/swf"
)

// push 操作码。
const (
	opPushByte   = 0x24
	opPushShort  = 0x25
	opPushInt    = 0x2D
	opPushUInt   = 0x2E
	opPushDouble = 0x2F
	opPushString = 0x2C
)

// ScanMode 决定数值匹配口径。
type ScanMode int

const (
	ScanAny    ScanMode = iota // 跨类型（类 CE 体验）：整型比 IntValue、浮点比 DoubleValue
	ScanInt                    // 仅有符号整型来源（pushbyte/short/int）
	ScanUInt                   // 仅无符号来源（pushuint）
	ScanDouble                 // 仅浮点来源（pushdouble）
)

// ScanOpts 为扫描参数。
type ScanOpts struct {
	Mode        ScanMode
	IntValue    int64   // 整型比较值（ScanAny/Int/UInt）
	DoubleValue float64 // 浮点比较值（ScanAny/Double）
	Limit       int     // 命中上限（0 = 10000）
}

// Hit 为一个可补丁的数值指令位置。
type Hit struct {
	ABCIdx   int    // DoABC tag 序号（ParseAll 顺序）
	ABCName  string // DoABC 模块名
	BodyIdx  int    // method_body 下标
	InsnIdx  int    // 指令下标
	Source   string // pushbyte / pushshort / pushint / pushuint / pushdouble
	IntVal   int64  // 命中数值（整型口径）
	FloatVal float64
	IsFloat  bool   // 数值来自 double 常量
	Where    string // 归因限定名
	Hints    []string
}

// Describe 生成人类可读的一行描述（CLI/Web 共用）。
func (h *Hit) Describe() string {
	if h.IsFloat {
		return fmt.Sprintf("%s = %s @ %s", h.Source, formatFloat(h.FloatVal), h.Where)
	}
	return fmt.Sprintf("%s = %d @ %s", h.Source, h.IntVal, h.Where)
}

func formatFloat(f float64) string {
	if f == math.Trunc(f) && math.Abs(f) < 1e15 {
		return fmt.Sprintf("%.0f", f)
	}
	return fmt.Sprintf("%g", f)
}

// Scan 在 SWF 全部 ABC 模块中搜索数值常量（push 指令）。
func Scan(sw *swf.File, opts ScanOpts) ([]*Hit, error) {
	if opts.Limit <= 0 {
		opts.Limit = 10000
	}
	tags, err := abc.ParseAll(sw)
	if err != nil {
		return nil, err
	}
	var hits []*Hit
	for ai, t := range tags {
		nt := buildNameTable(t.File)
		for bi, body := range t.File.Bodies {
			var hints []string
			for ii, in := range body.Insns {
				if in.Op == abc.OpRaw {
					continue
				}
				var h *Hit
				switch in.Op {
				case opPushByte:
					if v := int64(int8(in.U8s[0])); matchInt(opts, v, false) {
						h = newHit(ai, t, bi, ii, "pushbyte", v, 0, false)
					}
				case opPushShort:
					if v := int64(int32(in.U30s[0])); matchInt(opts, v, false) {
						h = newHit(ai, t, bi, ii, "pushshort", v, 0, false)
					}
				case opPushInt:
					if v, ok := poolInt(t.File, in.U30s[0]); ok && matchInt(opts, v, false) {
						h = newHit(ai, t, bi, ii, "pushint", v, 0, false)
					}
				case opPushUInt:
					if v, ok := poolUInt(t.File, in.U30s[0]); ok && matchInt(opts, v, true) {
						h = newHit(ai, t, bi, ii, "pushuint", v, 0, false)
					}
				case opPushDouble:
					if v, ok := poolDouble(t.File, in.U30s[0]); ok && matchDouble(opts, v) {
						h = newHit(ai, t, bi, ii, "pushdouble", 0, v, true)
					}
				}
				if h != nil {
					if hints == nil {
						hints = stringHints(t.File, body.Insns)
					}
					h.Where = nt.methodOf(body.Method)
					h.Hints = hints
					hits = append(hits, h)
					if len(hits) >= opts.Limit {
						return hits, nil
					}
				}
			}
		}
	}
	return hits, nil
}

func newHit(ai int, t *abc.TagABC, bi, ii int, src string, iv int64, fv float64, isF bool) *Hit {
	return &Hit{ABCIdx: ai, ABCName: t.Name, BodyIdx: bi, InsnIdx: ii,
		Source: src, IntVal: iv, FloatVal: fv, IsFloat: isF}
}

func poolInt(f *abc.File, idx uint32) (int64, bool) {
	if idx == 0 {
		return 0, true
	}
	if int(idx) <= len(f.Pool.Ints) {
		return int64(f.Pool.Ints[idx-1]), true
	}
	return 0, false
}

func poolUInt(f *abc.File, idx uint32) (int64, bool) {
	if idx == 0 {
		return 0, true
	}
	if int(idx) <= len(f.Pool.UInts) {
		return int64(f.Pool.UInts[idx-1]), true
	}
	return 0, false
}

func poolDouble(f *abc.File, idx uint32) (float64, bool) {
	if idx == 0 {
		return 0, true
	}
	if int(idx) <= len(f.Pool.Doubles) {
		return f.Pool.Doubles[idx-1], true
	}
	return 0, false
}

// matchInt 按模式匹配整型来源命中。
func matchInt(opts ScanOpts, v int64, unsigned bool) bool {
	switch opts.Mode {
	case ScanInt:
		return !unsigned && v == opts.IntValue
	case ScanUInt:
		return unsigned && v == opts.IntValue
	case ScanDouble:
		return false
	}
	return v == opts.IntValue
}

// matchDouble 按模式匹配浮点来源命中。
func matchDouble(opts ScanOpts, v float64) bool {
	switch opts.Mode {
	case ScanInt, ScanUInt:
		return false
	}
	return v == opts.DoubleValue
}

package patch

import (
	"testing"

	"swfkit/internal/abc"
	"swfkit/internal/swf"
	"swfkit/internal/testutil"
)

// findHit 在命中列表中按来源与归因过滤。
func findHit(hits []*Hit, source, whereSub string) *Hit {
	for _, h := range hits {
		if h.Source == source && contains(h.Where, whereSub) {
			return h
		}
	}
	return nil
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestScanAndPatchEndToEnd(t *testing.T) {
	raw := testutil.BuildGameSWF()

	// 1. 扫描整数 100 → 应命中 Player::addMoney 的 pushbyte
	sw, err := swf.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	hits, err := Scan(sw, ScanOpts{Mode: ScanAny, IntValue: 100, DoubleValue: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("扫描 100 命中 %d 处: %v", len(hits), hits)
	}
	h := hits[0]
	if h.Source != "pushbyte" || h.IntVal != 100 {
		t.Fatalf("命中异常: %+v", h)
	}
	if h.Where != "com.test.Player::addMoney" {
		t.Fatalf("归因异常: %q", h.Where)
	}

	// 2. 扫描浮点 3.14
	hitsF, err := Scan(sw, ScanOpts{Mode: ScanAny, IntValue: 0, DoubleValue: 3.14})
	if err != nil {
		t.Fatal(err)
	}
	if len(hitsF) != 1 || hitsF[0].Source != "pushdouble" {
		t.Fatalf("扫描 3.14 异常: %v", hitsF)
	}

	// 3. 补丁：100 → 9999（pushbyte 溢出，应升位为 pushshort）
	err = Apply(sw, []*Op{{Hit: h, NewInt: 9999}})
	if err != nil {
		t.Fatal(err)
	}
	patched, err := sw.Save()
	if err != nil {
		t.Fatal(err)
	}

	// 4. 重解析验证
	sw2, err := swf.Parse(patched)
	if err != nil {
		t.Fatalf("补丁后 SWF 解析失败: %v", err)
	}
	hits2, err := Scan(sw2, ScanOpts{Mode: ScanAny, IntValue: 9999, DoubleValue: 9999})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits2) != 1 || hits2[0].Source != "pushshort" || hits2[0].IntVal != 9999 {
		t.Fatalf("补丁后扫描 9999 异常: %v", hits2)
	}
	hits3, _ := Scan(sw2, ScanOpts{Mode: ScanAny, IntValue: 100, DoubleValue: 100})
	if len(hits3) != 0 {
		t.Fatalf("旧值 100 仍存在: %v", hits3)
	}

	// 5. 原值保留项不受影响：777 / 30000 / 3.14 仍可扫描到且数量正确
	for _, tc := range []struct {
		iv  int64
		dv  float64
		src string
	}{{777, 777, "pushint"}, {30000, 30000, "pushshort"}} {
		hs, _ := Scan(sw2, ScanOpts{Mode: ScanAny, IntValue: tc.iv, DoubleValue: tc.dv})
		if len(hs) != 1 || hs[0].Source != tc.src {
			t.Fatalf("原值 %d 校验异常: %v", tc.iv, hs)
		}
	}
	hs, _ := Scan(sw2, ScanOpts{Mode: ScanAny, DoubleValue: 3.14})
	if len(hs) != 1 || hs[0].Source != "pushdouble" {
		t.Fatalf("3.14 校验异常: %v", hs)
	}
}

func TestPatchFloatValue(t *testing.T) {
	raw := testutil.BuildGameSWF()
	sw, _ := swf.Parse(raw)
	hits, err := Scan(sw, ScanOpts{Mode: ScanDouble, DoubleValue: 3.14})
	if err != nil || len(hits) != 1 {
		t.Fatalf("扫描 3.14 异常: %v %v", hits, err)
	}
	if err := Apply(sw, []*Op{{Hit: hits[0], NewDouble: 999.5}}); err != nil {
		t.Fatal(err)
	}
	out, err := sw.Save()
	if err != nil {
		t.Fatal(err)
	}
	sw2, _ := swf.Parse(out)
	hits2, _ := Scan(sw2, ScanOpts{Mode: ScanDouble, DoubleValue: 999.5})
	if len(hits2) != 1 || hits2[0].FloatVal != 999.5 {
		t.Fatalf("浮点补丁后异常: %v", hits2)
	}
}

func TestExceptionOffsetRemap(t *testing.T) {
	// pushbyte 100 → 9999 会使 rich 方法体后续字节 +1 偏移，
	// 异常表 From/To/Target 必须同步重映射。
	raw := testutil.BuildGameSWF()
	sw, _ := swf.Parse(raw)
	tags, err := abc.ParseAll(sw)
	if err != nil {
		t.Fatal(err)
	}
	var body *abc.MethodBody
	for _, b := range tags[0].File.Bodies {
		if len(b.Exceptions) > 0 {
			body = b
		}
	}
	if body == nil {
		t.Fatal("测试样本缺少异常表")
	}
	orig := body.Exceptions[0]

	hits, _ := Scan(sw, ScanOpts{Mode: ScanAny, IntValue: 100, DoubleValue: 100})
	if len(hits) != 1 {
		t.Fatalf("扫描异常: %v", hits)
	}
	if err := Apply(sw, []*Op{{Hit: hits[0], NewInt: 9999}}); err != nil {
		t.Fatal(err)
	}
	out, err := sw.Save()
	if err != nil {
		t.Fatal(err)
	}

	sw2, _ := swf.Parse(out)
	tags2, err := abc.ParseAll(sw2)
	if err != nil {
		t.Fatal(err)
	}
	var body2 *abc.MethodBody
	for _, b := range tags2[0].File.Bodies {
		if len(b.Exceptions) > 0 {
			body2 = b
		}
	}
	if body2 == nil {
		t.Fatal("补丁后异常表丢失")
	}
	ne := body2.Exceptions[0]
	// pushbyte(2B) → pushshort 9999（varlen 2B，共 3B）：整体后移 +1
	if int(ne.From) != int(orig.From)+1 || int(ne.To) != int(orig.To)+1 || int(ne.Target) != int(orig.Target)+1 {
		t.Fatalf("异常偏移重映射异常: 原 from=%d to=%d target=%d → 新 from=%d to=%d target=%d",
			orig.From, orig.To, orig.Target, ne.From, ne.To, ne.Target)
	}
	// handler 目标必须仍落在某条指令/raw 区的起点（handler 位于不可达 raw 区）
	ok := false
	for _, in := range body2.Insns {
		if int(in.Offs) == int(ne.Target) {
			ok = true
		}
	}
	if !ok {
		t.Fatalf("异常 Target %d 未落在指令起点", ne.Target)
	}
}

func TestListClasses(t *testing.T) {
	raw := testutil.BuildGameSWF()
	sw, _ := swf.Parse(raw)
	classes, err := ListClasses(sw)
	if err != nil {
		t.Fatal(err)
	}
	if len(classes) != 1 {
		t.Fatalf("类数 = %d, 期望 1", len(classes))
	}
	c := classes[0]
	if c.Name != "com.test.Player" {
		t.Fatalf("类名 = %q", c.Name)
	}
	if len(c.Traits) != 1 || c.Traits[0] != "addMoney" {
		t.Fatalf("trait 异常: %v", c.Traits)
	}
}

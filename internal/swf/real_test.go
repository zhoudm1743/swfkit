package swf_test

import (
	"bytes"
	"os"
	"testing"

	"swfkit/internal/abc"
	"swfkit/internal/swf"
)

// TestRealWorldRoundTrip 用真实编译器产物（Flex SDK compc 编译的 flexunit SWC
// 内的 library.swf）验证解析器/写回器。设置 SWFKIT_REAL_SWF 环境变量启用：
//
//	SWFKIT_REAL_SWF=/path/to/library.swf go test -run RealWorld ./...
func TestRealWorldRoundTrip(t *testing.T) {
	path := os.Getenv("SWFKIT_REAL_SWF")
	if path == "" {
		t.Skip("未设置 SWFKIT_REAL_SWF，跳过真实样本验证")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			t.Skipf("SWFKIT_REAL_SWF 指向的文件不存在（%s），跳过", path)
		}
		t.Fatal(err)
	}
	f, err := swf.Parse(raw)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	var checked int
	for i, tg := range f.Tags {
		if !tg.IsDoABC() {
			continue
		}
		ta, err := abc.ParseTag(tg, i)
		if err != nil {
			t.Fatalf("DoABC[%d] 解析失败: %v", i, err)
		}
		d1, err := ta.Serialize()
		if err != nil {
			t.Fatalf("DoABC[%d] 序列化失败: %v", i, err)
		}
		ta2, err := abc.ParseTag(&swf.Tag{Code: tg.Code, Data: d1}, i)
		if err != nil {
			t.Fatalf("DoABC[%d] 重解析失败: %v", i, err)
		}
		d2, _ := ta2.Serialize()
		if !bytes.Equal(d1, d2) {
			t.Fatalf("DoABC[%d] 往返不一致", i)
		}
		checked++
	}
	t.Logf("真实样本：%d 个 DoABC 模块往返一致", checked)
	if checked == 0 {
		t.Fatal("样本中无 DoABC tag")
	}

	// SWF 级稳定往返（zlib 输出可能因压缩器不同与原始字节不一致，故验证二次稳定性）
	out1, err := f.Save()
	if err != nil {
		t.Fatal(err)
	}
	f2, err := swf.Parse(out1)
	if err != nil {
		t.Fatalf("保存后重解析失败: %v", err)
	}
	out2, _ := f2.Save()
	if !bytes.Equal(out1, out2) {
		t.Fatal("SWF 级稳定往返不一致")
	}
}

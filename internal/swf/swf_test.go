package swf_test

import (
	"bytes"
	"testing"

	"swfkit/internal/swf"
	"swfkit/internal/testutil"
)

func TestSWFRoundTrip(t *testing.T) {
	raw := testutil.BuildGameSWF()
	f, err := swf.Parse(raw)
	if err != nil {
		t.Fatalf("Parse 失败: %v", err)
	}
	out, err := f.Save()
	if err != nil {
		t.Fatalf("Save 失败: %v", err)
	}
	if !bytes.Equal(raw, out) {
		t.Fatalf("SWF 往返不一致（%d vs %d 字节）", len(raw), len(out))
	}
}

func TestSWFUncompressedRoundTrip(t *testing.T) {
	// CWS 版本转 FWS 后仍需可解析（重算 FileLength）
	raw := testutil.BuildGameSWF()
	f, err := swf.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	f.Header.Signature = swf.SigUncompressed
	out, err := f.Save()
	if err != nil {
		t.Fatal(err)
	}
	f2, err := swf.Parse(out)
	if err != nil {
		t.Fatalf("FWS 重新解析失败: %v", err)
	}
	if f2.Header.Signature != swf.SigUncompressed || len(f2.Tags) != len(f.Tags) {
		t.Fatalf("FWS 往返异常: sig=%s tags=%d", f2.Header.Signature, len(f2.Tags))
	}
}

func TestDoABCTags(t *testing.T) {
	raw := testutil.BuildGameSWF()
	f, err := swf.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	tags := f.DoABCTags()
	if len(tags) != 1 {
		t.Fatalf("DoABC tag 数 = %d, 期望 1", len(tags))
	}
}

package swf

import (
	"errors"
	"fmt"
)

// File 为解析后的 SWF 文件。Tags 中非 DoABC tag 保持原始字节，写回时原样透传。
type File struct {
	Header *Header
	Tags   []*Tag
}

// Parse 解析 SWF 字节流（自动处理 zlib 压缩）。
func Parse(raw []byte) (*File, error) {
	h, body, err := parseHeader(raw)
	if err != nil {
		return nil, err
	}
	tags, err := parseTags(body)
	if err != nil {
		return nil, err
	}
	return &File{Header: h, Tags: tags}, nil
}

// Save 序列化整个文件（保持原压缩格式，重算 FileLength）。
func (f *File) Save() ([]byte, error) {
	body := serializeTags(f.Tags)
	return serializeHeader(f.Header, body)
}

// DoABCTags 返回全部 AVM2 字节码 tag。
func (f *File) DoABCTags() []*Tag {
	var out []*Tag
	for _, t := range f.Tags {
		if t.IsDoABC() {
			out = append(out, t)
		}
	}
	return out
}

// HasABC 报告文件是否含有 AVM2 字节码（AS3）。
func (f *File) HasABC() bool { return len(f.DoABCTags()) > 0 }

// String 供调试与 info 命令输出概要。
func (f *File) String() string {
	if f.Header == nil {
		return "<empty>"
	}
	compress := "无"
	switch f.Header.Signature {
	case SigZlib:
		compress = "zlib (CWS)"
	case SigLzma:
		compress = "lzma (ZWS)"
	}
	return fmt.Sprintf("SWF v%d 压缩=%s 帧=%dx%d 帧率=%.2f tag数=%d",
		f.Header.Version, compress,
		(f.Header.FrameSize.XMax-f.Header.FrameSize.XMin)/20,
		(f.Header.FrameSize.YMax-f.Header.FrameSize.YMin)/20,
		float64(f.Header.FrameRate>>8)+float64(f.Header.FrameRate&0xFF)/256,
		len(f.Tags))
}

// ErrNoABC 表示文件中不含 AVM2 字节码（可能是 AS2 时代的 SWF）。
var ErrNoABC = errors.New("swf: 文件中未找到 DoABC tag（可能是 AS2 旧格式游戏，本工具不支持）")

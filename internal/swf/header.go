// Package swf 提供 SWF 容器文件的最小读写能力：
// 解析头与 tag 流、修改后重新序列化。非 DoABC 的 tag 一律原始字节透传，保证往返无损。
// 格式依据 Adobe 公开规范《SWF File Format Specification》(v19)。
package swf

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// 签名常量
const (
	SigUncompressed = "FWS" // 未压缩
	SigZlib         = "CWS" // zlib 压缩（Flash 6+）
	SigLzma         = "ZWS" // lzma 压缩（Flash 13+），v1 暂不支持
)

// Rect 为 SWF 位压缩的矩形（单位 twip，1/20 像素）。
type Rect struct {
	Nbits uint8
	XMin  int32
	XMax  int32
	YMin  int32
	YMax  int32
}

// Header 为 SWF 文件头（压缩体解压后的逻辑视图）。
type Header struct {
	Signature  string // FWS / CWS / ZWS
	Version    uint8
	FileLength uint32 // 解压后总长度（含头），写回时重算
	FrameSize  Rect
	FrameRate  uint16 // 8.8 定点：高 8 位整数、低 8 位小数
	FrameCount uint16
}

// bitReader 按位读取（SWF 大端位序）。
type bitReader struct {
	r    io.Reader
	cur  byte
	bits int // cur 中剩余位数
	err  error
}

func newBitReader(r io.Reader) *bitReader { return &bitReader{r: r} }

func (b *bitReader) readBits(n int) uint32 {
	var v uint32
	for n > 0 && b.err == nil {
		if b.bits == 0 {
			b.bits = 8
			b.cur, b.err = b.readByte()
			if b.err != nil {
				break
			}
		}
		take := n
		if take > b.bits {
			take = b.bits
		}
		shift := uint(b.bits - take)
		mask := byte((1<<take)-1) << shift
		v = v<<uint(take) | uint32((b.cur&mask)>>shift)
		b.bits -= take
		n -= take
	}
	return v
}

func (b *bitReader) readSBits(n int) int32 {
	v := int32(b.readBits(n))
	if n > 0 && v&(1<<(n-1)) != 0 { // 符号扩展
		v |= -1 << uint(n)
	}
	return v
}

func (b *bitReader) readByte() (byte, error) {
	var buf [1]byte
	if _, err := io.ReadFull(b.r, buf[:]); err != nil {
		return 0, err
	}
	return buf[0], nil
}

func (b *bitReader) alignByte() { b.bits = 0 }

// parseRect 从字节流当前位置解析 RECT，返回消耗的字节数。
func parseRect(data []byte) (Rect, int, error) {
	br := newBitReader(bytes.NewReader(data))
	nbits := int(br.readBits(5))
	if br.err != nil {
		return Rect{}, 0, br.err
	}
	rect := Rect{
		Nbits: uint8(nbits),
		XMin:  br.readSBits(nbits),
		XMax:  br.readSBits(nbits),
		YMin:  br.readSBits(nbits),
		YMax:  br.readSBits(nbits),
	}
	if br.err != nil {
		return Rect{}, 0, br.err
	}
	br.alignByte()
	total := (5 + 4*nbits + 7) / 8
	return rect, total, nil
}

// serializeRect 位压缩写回 RECT。
func serializeRect(r Rect) []byte {
	var out bytes.Buffer
	bw := &bitWriter{w: &out}
	nbits := int(r.Nbits)
	bw.writeBits(uint32(nbits), 5)
	bw.writeBits(uint32(r.XMin), nbits)
	bw.writeBits(uint32(r.XMax), nbits)
	bw.writeBits(uint32(r.YMin), nbits)
	bw.writeBits(uint32(r.YMax), nbits)
	bw.flush()
	return out.Bytes()
}

// bitWriter 按位写出（大端位序，与 bitReader 对应）。
type bitWriter struct {
	w    *bytes.Buffer
	cur  byte
	bits int // cur 中已占用位数
}

func (b *bitWriter) writeBits(v uint32, n int) {
	for i := n - 1; i >= 0; i-- {
		bit := byte((v >> uint(i)) & 1)
		b.cur = b.cur<<1 | bit
		b.bits++
		if b.bits == 8 {
			b.w.WriteByte(b.cur)
			b.cur, b.bits = 0, 0
		}
	}
}

func (b *bitWriter) flush() {
	if b.bits > 0 {
		b.cur <<= uint(8 - b.bits)
		b.w.WriteByte(b.cur)
		b.cur, b.bits = 0, 0
	}
}

// parseHeader 解析文件头并返回解压后的 body（FrameSize 起始）。
func parseHeader(raw []byte) (*Header, []byte, error) {
	if len(raw) < 8 {
		return nil, nil, errors.New("swf: 文件过短，不足 8 字节头")
	}
	h := &Header{Signature: string(raw[:3])}
	switch h.Signature {
	case SigUncompressed, SigZlib:
	case SigLzma:
		return nil, nil, fmt.Errorf("swf: 暂不支持 ZWS(LZMA) 压缩格式（Flash 13+ 导出），请先转换为 CWS")
	default:
		return nil, nil, fmt.Errorf("swf: 非法签名 %q，不是 SWF 文件", h.Signature)
	}
	h.Version = raw[3]
	h.FileLength = binary.LittleEndian.Uint32(raw[4:8])

	body := raw[8:]
	if h.Signature == SigZlib {
		zr, err := zlib.NewReader(bytes.NewReader(body))
		if err != nil {
			return nil, nil, fmt.Errorf("swf: zlib 解压失败: %w", err)
		}
		defer zr.Close()
		// FileLength - 8 为解压后 body 的期望长度
		expected := int(h.FileLength) - 8
		if expected <= 0 {
			return nil, nil, errors.New("swf: FileLength 字段非法")
		}
		body, err = io.ReadAll(io.LimitReader(zr, int64(expected)))
		if err != nil {
			return nil, nil, fmt.Errorf("swf: zlib 解压失败: %w", err)
		}
	}

	rect, n, err := parseRect(body)
	if err != nil {
		return nil, nil, fmt.Errorf("swf: 解析 FrameSize 失败: %w", err)
	}
	h.FrameSize = rect
	rest := body[n:]
	if len(rest) < 4 {
		return nil, nil, errors.New("swf: 头部不完整，缺帧率/帧数字段")
	}
	h.FrameRate = binary.LittleEndian.Uint16(rest[0:2])
	h.FrameCount = binary.LittleEndian.Uint16(rest[2:4])
	return h, rest[4:], nil
}

// serializeHeader 序列化头部并接上 body；signature 决定是否压缩 body。
func serializeHeader(h *Header, body []byte) ([]byte, error) {
	if h.Signature == SigLzma {
		return nil, errors.New("swf: 暂不支持写出 ZWS(LZMA) 格式")
	}
	rect := serializeRect(h.FrameSize)
	plain := make([]byte, 0, len(rect)+4+len(body))
	plain = append(plain, rect...)
	var fr [4]byte
	binary.LittleEndian.PutUint16(fr[0:2], h.FrameRate)
	binary.LittleEndian.PutUint16(fr[2:4], h.FrameCount)
	plain = append(plain, fr[:]...)
	plain = append(plain, body...)

	var out bytes.Buffer
	out.WriteString(h.Signature)
	out.WriteByte(h.Version)
	// FileLength 恒记录未压缩总长（含 8 字节头），与压缩与否无关
	var fl [4]byte
	binary.LittleEndian.PutUint32(fl[:], uint32(8+len(plain)))
	out.Write(fl[:])
	if h.Signature == SigZlib {
		zw := zlib.NewWriter(&out)
		if _, err := zw.Write(plain); err != nil {
			return nil, err
		}
		if err := zw.Close(); err != nil {
			return nil, err
		}
	} else {
		out.Write(plain)
	}
	return out.Bytes(), nil
}

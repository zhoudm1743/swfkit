package swf

import (
	"encoding/binary"
	"fmt"
)

// SWF tag code（本工具关心的部分；其余透传）。
const (
	TagEnd         = 0 // End
	TagShowFrame   = 1 // ShowFrame
	TagDoABC       = 72
	TagSymbolClass = 76
)

// Tag 为原始 tag：Code + 原始数据。修改工具对不认识的 tag 只做字节透传。
type Tag struct {
	Code int
	Data []byte
}

// IsDoABC 报告该 tag 是否为 AVM2 字节码容器（tag 72 DoABC 无名版 / 82 DoABC 带标志与名称）。
func (t *Tag) IsDoABC() bool { return t.Code == TagDoABC || t.Code == 82 }

// parseTags 遍历 tag 流，返回 tag 列表。data 应为头部之后的完整 body。
func parseTags(data []byte) ([]*Tag, error) {
	var tags []*Tag
	pos := 0
	for {
		if pos >= len(data) {
			return nil, fmt.Errorf("swf: tag 流意外截断于偏移 %d（缺 End tag）", pos)
		}
		codeAndLen := binary.LittleEndian.Uint16(data[pos:])
		code := int(codeAndLen >> 6)
		length := int(codeAndLen & 0x3F)
		pos += 2
		if length == 0x3F { // 长格式：后跟 32 位长度
			if pos+4 > len(data) {
				return nil, fmt.Errorf("swf: 长格式 tag 长度字段截断于偏移 %d", pos)
			}
			length = int(binary.LittleEndian.Uint32(data[pos:]))
			pos += 4
		}
		if pos+length > len(data) {
			return nil, fmt.Errorf("swf: tag(code=%d) 数据截断：声明 %d 字节，实际剩 %d", code, length, len(data)-pos)
		}
		tags = append(tags, &Tag{Code: code, Data: data[pos : pos+length : pos+length]})
		pos += length
		if code == TagEnd {
			return tags, nil
		}
	}
}

// serializeTags 重序列化 tag 列表（自动按长度选择短/长格式）。
func serializeTags(tags []*Tag) []byte {
	var out []byte
	for _, t := range tags {
		length := len(t.Data)
		if length < 0x3F {
			codeAndLen := uint16(t.Code)<<6 | uint16(length)
			out = binary.LittleEndian.AppendUint16(out, codeAndLen)
		} else {
			codeAndLen := uint16(t.Code)<<6 | 0x3F
			out = binary.LittleEndian.AppendUint16(out, codeAndLen)
			out = binary.LittleEndian.AppendUint32(out, uint32(length))
		}
		out = append(out, t.Data...)
	}
	return out
}

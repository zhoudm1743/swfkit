package sol

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// JSON 映射约定（sol-dump / sol-build 往返无损）：
//   nil → null；Undefined → {"$undefined":true}
//   bool/float64/int32/string → 原生 JSON
//   Date → {"$date":毫秒,"$tz":时区}
//   []byte(ByteArray) → {"$bytes":base64}
//   *XMLDoc → {"$xml":"…","$isdoc":true|false}
//   *StrictArray → JSON 数组
//   *Object → JSON 对象（键序保持）；typed object 额外注入 "$class"
// 注：以 "$" 开头的业务键会被转义为 "$$" 前缀，避免歧义。

// MarshalValue 序列化单个 AMF 值为 JSON 片段（供 API 按路径取值时使用）。
func MarshalValue(v any) ([]byte, error) {
	var buf bytes.Buffer
	if err := writeOrderedValue(&buf, v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// MarshalJSON 输出保持键序的 JSON。
func (d *Doc) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	if err := writeOrderedValue(&buf, d.Root); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// UnmarshalJSON 从（MarshalJSON 产出的）JSON 重建文档。
func UnmarshalJSON(data []byte) (*Doc, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	delim, ok := tok.(json.Delim)
	if !ok || delim != '{' {
		return nil, fmt.Errorf("sol: JSON 顶层必须是对象")
	}
	root, err := readOrderedObject(dec)
	if err != nil {
		return nil, err
	}
	return &Doc{Name: "imported", Root: root}, nil
}

func escapeKey(k string) string {
	if strings.HasPrefix(k, "$") {
		return "$" + k
	}
	return k
}

func unescapeKey(k string) string {
	if len(k) >= 2 && k[0] == '$' && k[1] == '$' {
		return k[1:]
	}
	return k
}

func writeOrderedValue(buf *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case nil:
		buf.WriteString("null")
	case Undefined:
		buf.WriteString(`{"$undefined":true}`)
	case bool, int32, float64:
		return writeJSONNumber(buf, x)
	case string:
		return writeJSONString(buf, x)
	case Date:
		buf.WriteString(`{"$date":`)
		b, _ := json.Marshal(x.Millis)
		buf.Write(b)
		buf.WriteString(`,"$tz":`)
		buf.WriteString(strconv.Itoa(int(x.TZ)))
		buf.WriteString("}")
	case []byte:
		buf.WriteString(`{"$bytes":"`)
		buf.WriteString(base64.StdEncoding.EncodeToString(x))
		buf.WriteString(`"}`)
	case *XMLDoc:
		buf.WriteString(`{"$xml":`)
		if err := writeJSONString(buf, x.Text); err != nil {
			return err
		}
		if x.IsDoc {
			buf.WriteString(`,"$isdoc":true`)
		}
		buf.WriteString("}")
	case *StrictArray:
		buf.WriteByte('[')
		for i, it := range x.Items {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeOrderedValue(buf, it); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case *Object:
		buf.WriteByte('{')
		first := true
		if x.ClassName != "" {
			buf.WriteString(`"$class":`)
			if err := writeJSONString(buf, x.ClassName); err != nil {
				return err
			}
			first = false
		}
		if x.IsArray {
			if !first {
				buf.WriteByte(',')
			}
			buf.WriteString(`"$ecma":true`)
			first = false
		}
		for _, k := range x.Keys {
			if !first {
				buf.WriteByte(',')
			}
			first = false
			if err := writeJSONString(buf, escapeKey(k)); err != nil {
				return err
			}
			buf.WriteByte(':')
			if err := writeOrderedValue(buf, x.Vals[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	default:
		return fmt.Errorf("sol: 不支持的 JSON 值类型 %T", v)
	}
	return nil
}

func writeJSONNumber(buf *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case bool:
		if x {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case int32:
		buf.WriteString(strconv.FormatInt(int64(x), 10))
	case float64:
		b, err := json.Marshal(x)
		if err != nil {
			return err
		}
		buf.Write(b)
	default:
		return fmt.Errorf("sol: 非数值类型 %T", v)
	}
	return nil
}

func writeJSONString(buf *bytes.Buffer, s string) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	buf.Write(b)
	return nil
}

// readOrderedObject 以 token 流读取 JSON 对象并保持键序。
func readOrderedObject(dec *json.Decoder) (*Object, error) {
	obj := NewObject()
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := tok.(string)
		if !ok {
			return nil, fmt.Errorf("sol: 期望对象键，得到 %v", tok)
		}
		v, err := readOrderedValue(dec)
		if err != nil {
			return nil, err
		}
		obj.Set(unescapeKey(key), v)
	}
	if _, err := dec.Token(); err != nil { // 消费 '}'
		return nil, err
	}
	return obj, nil
}

func readOrderedValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			// 先探测是否为注解对象
			return readAnnotatedObject(dec)
		case '[':
			arr := &StrictArray{}
			for dec.More() {
				v, err := readOrderedValue(dec)
				if err != nil {
					return nil, err
				}
				arr.Items = append(arr.Items, v)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return arr, nil
		default:
			return nil, fmt.Errorf("sol: 意外的闭合符 %v", t)
		}
	case json.Number:
		f, err := t.Float64()
		if err != nil {
			return nil, err
		}
		return f, nil
	case string:
		return t, nil
	case bool:
		return t, nil
	case nil:
		return nil, nil
	default:
		return nil, fmt.Errorf("sol: 意外的 JSON token %v", tok)
	}
}

// readAnnotatedObject 解析带 $ 注解的对象；注解键之后的其余键
// 一并并入同一对象（$class/$ecma 与字段共存）。
func readAnnotatedObject(dec *json.Decoder) (any, error) {
	obj := NewObject()
	var (
		undef      bool
		dateMillis float64
		hasDate    bool
		dateTZ     int16
		b64        string
		hasBytes   bool
		xmlText    string
		hasXML     bool
		xmlIsDoc   bool
	)
	simple := false
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, _ := tok.(string)
		switch key {
		case "$undefined":
			undef = true
			simple = true
			if _, err := dec.Token(); err != nil { // true
				return nil, err
			}
		case "$date":
			hasDate = true
			simple = true
			v, err := readOrderedValue(dec)
			if err != nil {
				return nil, err
			}
			dateMillis = v.(float64)
		case "$tz":
			v, err := readOrderedValue(dec)
			if err != nil {
				return nil, err
			}
			dateTZ = int16(v.(float64))
		case "$bytes":
			hasBytes = true
			simple = true
			v, err := readOrderedValue(dec)
			if err != nil {
				return nil, err
			}
			b64 = v.(string)
		case "$xml":
			hasXML = true
			simple = true
			v, err := readOrderedValue(dec)
			if err != nil {
				return nil, err
			}
			xmlText = v.(string)
		case "$isdoc":
			v, err := readOrderedValue(dec)
			if err != nil {
				return nil, err
			}
			xmlIsDoc, _ = v.(bool)
		case "$class":
			v, err := readOrderedValue(dec)
			if err != nil {
				return nil, err
			}
			obj.ClassName, _ = v.(string)
		case "$ecma":
			v, err := readOrderedValue(dec)
			if err != nil {
				return nil, err
			}
			if b, ok := v.(bool); ok && b {
				obj.IsArray = true
			}
		default:
			v, err := readOrderedValue(dec)
			if err != nil {
				return nil, err
			}
			obj.Set(unescapeKey(key), v)
		}
	}
	if _, err := dec.Token(); err != nil { // 消费 '}'
		return nil, err
	}
	switch {
	case undef:
		return Undefined{}, nil
	case hasDate:
		return Date{Millis: dateMillis, TZ: dateTZ}, nil
	case hasBytes:
		return base64.StdEncoding.DecodeString(b64)
	case hasXML:
		return &XMLDoc{IsDoc: xmlIsDoc, Text: xmlText}, nil
	}
	if simple {
		return nil, fmt.Errorf("sol: 注解对象字段混用")
	}
	return obj, nil
}

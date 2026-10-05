package sol

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"math"
	"testing"
)

// buildTestSOL 手工构造一份 .sol：
// 名称 "gamesave"，字段：
//
//	gold   = 1234.5          (AMF0 number)
//	hp     = 100 (AMF3 integer，经 AVM+ 切换)
//	blob   = ByteArray{1,2,3}（经 AVM+ 切换）
//	player = object{ name="hero", bag=ecma{0="sword",1="shield"}, flag=true, day=Date }
func buildTestSOL(t *testing.T) []byte {
	t.Helper()
	var body []byte
	putU16 := func(v int) { body = binary.BigEndian.AppendUint16(body, uint16(v)) }
	putStr := func(s string) { putU16(len(s)); body = append(body, s...) }

	// gold = 1234.5
	putStr("gold")
	body = append(body, amf0Number)
	body = binary.BigEndian.AppendUint64(body, math.Float64bits(1234.5))
	// hp = 100（AMF3 integer 经 AVM+）
	putStr("hp")
	body = append(body, amf0AVMPlus, amf3Integer, 0x64) // u29 单字节 100
	// blob = ByteArray{1,2,3}
	putStr("blob")
	body = append(body, amf0AVMPlus, amf3ByteArray, (3<<1)|1, 1, 2, 3)
	// player = object
	putStr("player")
	body = append(body, amf0Object)
	putStr("name")
	body = append(body, amf0String)
	putStr("hero")
	putStr("bag")
	body = append(body, amf0ECMAArray)
	body = binary.BigEndian.AppendUint32(body, 2)
	putStr("0")
	body = append(body, amf0String)
	putStr("sword")
	putStr("1")
	body = append(body, amf0String)
	putStr("shield")
	putU16(0)
	body = append(body, amf0ObjectEnd)
	putStr("flag")
	body = append(body, amf0Boolean, 1)
	putStr("day")
	body = append(body, amf0Date)
	body = binary.BigEndian.AppendUint64(body, math.Float64bits(1700000000000))
	body = binary.BigEndian.AppendUint16(body, 0)
	putU16(0)
	body = append(body, amf0ObjectEnd)
	// 顶层终止 + 尾垫
	putU16(0)
	body = append(body, amf0ObjectEnd, 0x00)

	var out []byte
	out = append(out, 0x00, 0xBF)
	out = binary.BigEndian.AppendUint32(out, uint32(len(body)))
	out = append(out, "TCSO"...)
	out = append(out, 0x00, 0x04, 0x00, 0x00, 0x00, 0x00)
	name := "gamesave"
	out = binary.BigEndian.AppendUint16(out, uint16(len(name)))
	out = append(out, name...)
	out = append(out, 0, 0, 0, 0)
	out = append(out, body...)
	return out
}

func TestSOLParseAndRoundTrip(t *testing.T) {
	raw := buildTestSOL(t)
	doc, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse 失败: %v", err)
	}
	if doc.Name != "gamesave" {
		t.Fatalf("存档名 = %q", doc.Name)
	}
	// 字段断言
	if v, ok := doc.Get("gold"); !ok || v.(float64) != 1234.5 {
		t.Fatalf("gold 异常: %v", v)
	}
	if v, _ := doc.Get("hp"); v.(int32) != 100 {
		t.Fatalf("hp 异常: %v", v)
	}
	if v, _ := doc.Get("blob"); !bytes.Equal(v.([]byte), []byte{1, 2, 3}) {
		t.Fatalf("blob 异常: %v", v)
	}
	player, _ := doc.Get("player")
	po := player.(*Object)
	if name, _ := po.Get("name"); name != "hero" {
		t.Fatalf("player.name 异常: %v", name)
	}
	bag, _ := po.Get("bag")
	bo := bag.(*Object)
	if !bo.IsArray {
		t.Fatal("bag 应为 ECMA array")
	}
	if v, _ := bo.Get("1"); v != "shield" {
		t.Fatalf("bag.1 异常: %v", v)
	}
	if day, _ := po.Get("day"); day.(Date).Millis != 1700000000000 {
		t.Fatalf("day 异常: %v", day)
	}

	// 序列化往返：结构等价（引用内联化，此处无引用，字节应一致）
	out, err := doc.Serialize()
	if err != nil {
		t.Fatalf("Serialize 失败: %v", err)
	}
	doc2, err := Parse(out)
	if err != nil {
		t.Fatalf("往返解析失败: %v", err)
	}
	if !bytes.Equal(serializeJSON(doc), serializeJSON(doc2)) {
		t.Fatalf("往返 JSON 不一致:\n%s\n%s", serializeJSON(doc), serializeJSON(doc2))
	}
}

func serializeJSON(d *Doc) []byte {
	j, _ := d.MarshalJSON()
	return j
}

func TestSOLSetAndScalar(t *testing.T) {
	raw := buildTestSOL(t)
	doc, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	// 修改嵌套字段
	if err := doc.Set("player.bag.0", "magic_sword"); err != nil {
		t.Fatal(err)
	}
	if err := doc.Set("gold", 999999.0); err != nil {
		t.Fatal(err)
	}
	out, err := doc.Serialize()
	if err != nil {
		t.Fatal(err)
	}
	doc2, err := Parse(out)
	if err != nil {
		t.Fatal(err)
	}
	bag, _ := doc2.Get("player.bag.0")
	if bag != "magic_sword" {
		t.Fatalf("修改未生效: %v", bag)
	}
	if g, _ := doc2.Get("gold"); g.(float64) != 999999 {
		t.Fatalf("gold 修改未生效: %v", g)
	}
	// 中间层自动创建
	if err := doc2.Set("newobj.inner", true); err != nil {
		t.Fatal(err)
	}
	if v, ok := doc2.Get("newobj.inner"); !ok || v != true {
		t.Fatalf("自动建层失败: %v %v", v, ok)
	}
}

func TestSOLJSONRoundTrip(t *testing.T) {
	raw := buildTestSOL(t)
	doc, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	j, err := doc.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	doc2, err := UnmarshalJSON(j)
	if err != nil {
		t.Fatalf("JSON 回建失败: %v", err)
	}
	// sol-build → Serialize → Parse 等价
	out, err := doc2.Serialize()
	if err != nil {
		t.Fatal(err)
	}
	doc3, err := Parse(out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(serializeJSON(doc), serializeJSON(doc3)) {
		t.Fatalf("JSON 往返不一致:\n%s\n%s", serializeJSON(doc), serializeJSON(doc3))
	}
	// ByteArray 经 JSON base64 保真
	j2, _ := doc3.MarshalJSON()
	if !bytes.Contains(j2, []byte(base64.StdEncoding.EncodeToString([]byte{1, 2, 3}))) {
		t.Fatalf("ByteArray base64 丢失: %s", j2)
	}
}

func TestSOLAMF3Values(t *testing.T) {
	// AVM+ 内嵌 AMF3 对象（密封 traits + 动态键）
	var body []byte
	putU16 := func(v int) { body = binary.BigEndian.AppendUint16(body, uint16(v)) }
	putStr := func(s string) { putU16(len(s)); body = append(body, s...) }

	putStr("inv")
	body = append(body, amf0AVMPlus, amf3Object)
	// u29: bit0=1 内联, bit1=1, bit2=0, bit3=1 动态, 成员数 1 → (1<<4)|0x0B = 0x1B
	body = append(body, 0x1B)
	// 类名（AMF3 字符串：len<<1|1）
	cn := "game.Inventory"
	body = append(body, byte(len(cn)<<1|1))
	body = append(body, cn...)
	// 密封成员 "size"（AMF3 int 42）
	m := "size"
	body = append(body, byte(len(m)<<1|1))
	body = append(body, m...)
	body = append(body, amf3Integer, 42)
	// 动态键 "desc" = "bag"，随后空键结束
	dk := "desc"
	body = append(body, byte(len(dk)<<1|1))
	body = append(body, dk...)
	body = append(body, amf3String, byte(3<<1|1), 'b', 'a', 'g')
	body = append(body, 0x01) // 空字符串 → 动态区终止
	// 顶层终止 + 尾垫
	putU16(0)
	body = append(body, amf0ObjectEnd, 0x00)

	var out []byte
	out = append(out, 0x00, 0xBF)
	out = binary.BigEndian.AppendUint32(out, uint32(len(body)))
	out = append(out, "TCSO"...)
	out = append(out, 0x00, 0x04, 0x00, 0x00, 0x00, 0x00)
	out = binary.BigEndian.AppendUint16(out, 1)
	out = append(out, 'x')
	out = append(out, 0, 0, 0, 0)
	out = append(out, body...)

	doc, err := Parse(out)
	if err != nil {
		t.Fatalf("Parse 失败: %v", err)
	}
	inv, ok := doc.Get("inv")
	if !ok {
		t.Fatal("缺少 inv")
	}
	io := inv.(*Object)
	if io.ClassName != "game.Inventory" {
		t.Fatalf("类名 = %q", io.ClassName)
	}
	if v, _ := io.Get("size"); v.(int32) != 42 {
		t.Fatalf("size = %v", v)
	}
	if v, _ := io.Get("desc"); v != "bag" {
		t.Fatalf("desc = %v", v)
	}
}

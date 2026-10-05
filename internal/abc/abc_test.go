package abc

import (
	"bytes"
	"testing"
)

// mustParse 解析 ABC 并在失败时终止测试。
func mustParse(t *testing.T, data []byte) *File {
	t.Helper()
	f, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse 失败: %v", err)
	}
	return f
}

func TestABCBuildRoundTrip(t *testing.T) {
	data := buildTestModule(t)
	f := mustParse(t, data)

	// 结构断言
	if len(f.Methods) != 4 {
		t.Fatalf("method 数 = %d, 期望 4", len(f.Methods))
	}
	if len(f.Bodies) != 4 {
		t.Fatalf("body 数 = %d, 期望 4", len(f.Bodies))
	}
	if got := f.Pool.String(1); got != "com.test" {
		t.Fatalf("字符串 1 = %q, 期望 com.test", got)
	}
	if len(f.Pool.Ints) == 0 || f.Pool.Ints[0] != 777 {
		t.Fatalf("int 常量池异常: %v", f.Pool.Ints)
	}
	if len(f.Pool.Doubles) == 0 || f.Pool.Doubles[0] != 3.14 {
		t.Fatalf("double 常量池异常: %v", f.Pool.Doubles)
	}
	// rich 方法体指令核对
	var add *MethodBody
	for _, b := range f.Bodies {
		for _, in := range b.Insns {
			if in.Op == 0x2C { // pushstring
				add = b
			}
		}
	}
	if add == nil {
		t.Fatal("未找到含 pushstring 的 rich 方法体")
	}
	// 序列化 → 再解析 → 再序列化必须字节一致
	again := mustParse(t, data)
	b1, err := again.Serialize()
	if err != nil {
		t.Fatalf("Serialize 失败: %v", err)
	}
	b2, err := f.Serialize()
	if err != nil {
		t.Fatalf("Serialize 失败: %v", err)
	}
	if !bytes.Equal(b1, b2) {
		t.Fatalf("往返序列化不一致（%d vs %d 字节）", len(b1), len(b2))
	}
	if !bytes.Equal(b1, data) {
		t.Fatalf("序列化与原始字节不一致（%d vs %d）——解析器/写回器存在失真", len(b1), len(data))
	}
}

// buildTestModule 复用 testutil 的游戏模块构建（避免测试二进制相互依赖，
// 这里内联一份最小构建逻辑）。
func buildTestModule(t *testing.T) []byte {
	t.Helper()
	f := &File{MinorVersion: 16, MajorVersion: 46, Pool: &Pool{}}
	nsCom := f.Pool.AddString("com.test")
	strAdd := f.Pool.AddString("addMoney")
	f.Pool.AddString("gold")
	nsIdx := uint32(len(f.Pool.Namespaces) + 1)
	f.Pool.Namespaces = append(f.Pool.Namespaces, Namespace{Kind: 0x16, Name: nsCom})
	f.Pool.Multinames = append(f.Pool.Multinames, Multiname{Kind: MultinameQName, NS: nsIdx, Name: strAdd})
	mnAdd := uint32(len(f.Pool.Multinames))
	intIdx := f.Pool.AddInt(777)
	dblIdx := f.Pool.AddDouble(3.14)

	// addMoney 方法体（预编码字节流）：
	// pushbyte 100; pushshort 30000; pushint 777; pushdouble 3.14;
	// pushstring "gold"; iftrue L; pushbyte 1; pop; L: pop; returnvoid
	code := []byte{
		0xD0,       // getlocal0
		0x30,       // pushscope
		0x24, 0x64, // pushbyte 100
		0x25, 0xB0, 0xEA, 0x01, // pushshort 30000 (u30)
		0x2D, byte(intIdx), // pushint 777
		0x2F, byte(dblIdx), // pushdouble 3.14
		0x2C, 0x04, // pushstring "gold"（索引 4）
		0x11, 0x03, 0x00, 0x00, // iftrue +3 → 21（相对指令末尾 18）
		0x24, 0x01, // pushbyte 1
		0x29, // pop
		0x29, // pop
		0x47, // returnvoid
	}
	// 修正 iftrue delta：目标为偏移 21 的 pop，指令末尾 18，delta = +3
	if len(code) != 23 {
		t.Fatalf("测试布局异常：code 长度 %d，期望 23", len(code))
	}

	f.Methods = append(f.Methods, &MethodInfo{Name: strAdd})
	f.Bodies = append(f.Bodies, &MethodBody{Method: 0, MaxStack: 2, LocalCount: 1})
	ins, err := DecodeCode(code)
	if err != nil {
		t.Fatalf("DecodeCode 失败: %v", err)
	}
	f.Bodies[0].Insns = ins

	f.Instances = append(f.Instances, &InstanceInfo{
		Name: 1, Flags: ClassSealed, IInit: 0,
		Traits: []Trait{{Name: mnAdd, Kind: TraitMethod, Method: 0}},
	})
	f.Classes = append(f.Classes, &ClassInfo{CInit: 0})
	f.Scripts = append(f.Scripts, &ScriptInfo{Init: 0})
	// 上面的 body 已挂 Methods[0]；补充三个空方法保持结构完整
	for i := 1; i < 4; i++ {
		f.Methods = append(f.Methods, &MethodInfo{})
		ins, err := DecodeCode([]byte{0xD0, 0x30, 0x47})
		if err != nil {
			t.Fatal(err)
		}
		f.Bodies = append(f.Bodies, &MethodBody{Method: uint32(i), Insns: ins})
	}

	data, err := f.Serialize()
	if err != nil {
		t.Fatalf("Serialize 失败: %v", err)
	}
	return data
}

func TestBranchAndExceptionRoundTrip(t *testing.T) {
	// 分支 + lookupswitch + 跳转目标重映射的字节级往返
	// pushbyte 1; jump L2; L1: pushbyte 2; L2: lookupswitch ->L1(默认),[L1];
	// pushbyte 3; pop; returnvoid
	f := &File{MinorVersion: 16, MajorVersion: 46, Pool: &Pool{}}
	f.Methods = append(f.Methods, &MethodInfo{})
	f.Bodies = append(f.Bodies, &MethodBody{Method: 0})

	f.Bodies[0].Insns = nil
	// 直接用手汇字节验证解码正确性
	// 布局: pushbyte1 / jump+2→6 / pushbyte2(L1=6) / lookupswitch(8)
	//       default=-2→6, count=1(2个case), case0=-2→6, case1=+11→19
	//       pushbyte3(19) / pop(22) / returnvoid(23)
	code := []byte{
		0x24, 0x01, // 0: pushbyte 1
		0x10, 0x00, 0x00, 0x00, // 2: jump +0（相对指令末尾 6）→ 6
		0x24, 0x02, // 6: pushbyte 2 (L1)
		0x1B,             // 8: lookupswitch
		0xFE, 0xFF, 0xFF, //    default -2 → 6（相对指令起点）
		0x01,             //    case 数 1 → 共 2 个 case 目标
		0xFE, 0xFF, 0xFF, //    case0 -2 → 6
		0x0B, 0x00, 0x00, //    case1 +11 → 19
		0x24, 0x03, // 19: pushbyte 3
		0x29, // 22: pop
		0x47, // 23: returnvoid
	}
	ins, err := DecodeCode(code)
	if err != nil {
		t.Fatalf("DecodeCode 失败: %v", err)
	}
	f.Bodies[0].Insns = ins

	// 校验解码结果
	var jump, sw *Insn
	for _, in := range ins {
		switch in.Op {
		case 0x10:
			jump = in
		case 0x1B:
			sw = in
		}
	}
	if jump == nil || sw == nil {
		t.Fatalf("分支指令解码缺失: jump=%v switch=%v", jump != nil, sw != nil)
	}
	if jump.JumpTarget != 6 {
		t.Fatalf("jump 目标 = %d, 期望 6", jump.JumpTarget)
	}
	if sw.SwitchDefault != 6 || len(sw.SwitchCases) != 2 || sw.SwitchCases[0] != 6 {
		t.Fatalf("switch 解析异常: default=%d cases=%v", sw.SwitchDefault, sw.SwitchCases)
	}

	// 重编码必须与原字节一致
	out, _, err := EncodeCode(ins)
	if err != nil {
		t.Fatalf("EncodeCode 失败: %v", err)
	}
	if !bytes.Equal(out, code) {
		t.Fatalf("重编码不一致:\n原 % X\n新 % X", code, out)
	}

	// 完整 ABC 往返
	data, err := f.Serialize()
	if err != nil {
		t.Fatal(err)
	}
	f2 := mustParse(t, data)
	b2, err := f2.Serialize()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, b2) {
		t.Fatal("ABC 往返不一致")
	}
}

func TestRawPassthrough(t *testing.T) {
	// jump 跳过的死代码区（含非法字节）必须作为 raw 透传保留
	code := []byte{
		0x10, 0x04, 0x00, 0x00, // 0: jump +4（相对指令末尾 4）→ 8
		0xDE, 0xAD, 0xBE, 0xEF, // 4-7: 死代码（0xDE 为非法操作码）
		0x47, // 8: returnvoid
	}
	ins, err := DecodeCode(code)
	if err != nil {
		t.Fatalf("DecodeCode 失败: %v", err)
	}
	if len(ins) != 3 {
		t.Fatalf("指令数 = %d, 期望 3（jump + raw + returnvoid）", len(ins))
	}
	if ins[1].Op != OpRaw || !bytes.Equal(ins[1].Raw, code[4:8]) {
		t.Fatalf("raw 区异常: %+v", ins[1])
	}
	out, _, err := EncodeCode(ins)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, code) {
		t.Fatal("raw 透传不一致")
	}
}

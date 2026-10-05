// Package patch 实现类 Cheat Engine 的 SWF 数值扫描与补丁引擎：
// 在 AVM2 指令流中定位数值常量（push 指令）并安全改写。
package patch

import (
	"fmt"
	"sort"
	"strings"

	"swfkit/internal/abc"
)

// nameTable 为单个 ABC 模块的方法归因表（method 索引 → 限定名）。
type nameTable struct {
	names map[uint32]string
}

// buildNameTable 遍历 script/class trait，为每个有名字的 method 建立归因。
// 同一 method 多处定义时优先类方法，其次脚本函数。
func buildNameTable(f *abc.File) *nameTable {
	nt := &nameTable{names: make(map[uint32]string)}
	claim := func(idx uint32, name string, pri int) {
		if idx == 0 {
			return
		}
		if old, ok := nt.names[idx]; !ok || pri < priorityOf(old) {
			nt.names[idx] = name
		}
	}
	// pri 越小优先级越高：类方法(0) > 类构造(1) > 脚本函数(2) > 其他(3)
	for si, s := range f.Scripts {
		for ti := range s.Traits {
			t := &s.Traits[ti]
			if t.KindBase() == abc.TraitMethod || t.KindBase() == abc.TraitFunction {
				claim(t.Method, shortName(f, t.Name), 2)
			}
		}
		claim(s.Init, fmt.Sprintf("script#%d$init", si), 3)
	}
	for ci, inst := range f.Instances {
		cn := f.Pool.MultinameString(inst.Name)
		claim(inst.IInit, cn+"::$iinit", 1)
		for ti := range inst.Traits {
			t := &inst.Traits[ti]
			var suffix string
			switch t.KindBase() {
			case abc.TraitMethod:
				suffix = ""
			case abc.TraitGetter:
				suffix = "get "
			case abc.TraitSetter:
				suffix = "set "
			default:
				continue
			}
			claim(t.Method, cn+"::"+suffix+shortName(f, t.Name), 0)
		}
		if ci < len(f.Classes) {
			cls := f.Classes[ci]
			claim(cls.CInit, cn+"::$cinit", 1)
			for ti := range cls.Traits {
				t := &cls.Traits[ti]
				if t.KindBase() == abc.TraitMethod || t.KindBase() == abc.TraitFunction {
					claim(t.Method, cn+"::"+shortName(f, t.Name), 0)
				}
			}
		}
	}
	return nt
}

// shortName 取多名中的名称分量（去除包前缀），用于 trait 显示；
// "com.test.hp" → "hp"，"get com.test.hp" → "get hp"。
func shortName(f *abc.File, mnIdx uint32) string {
	full := f.Pool.MultinameString(mnIdx)
	if i := strings.LastIndex(full, "."); i >= 0 {
		return full[i+1:]
	}
	return full
}

func priorityOf(name string) int {
	switch {
	case strings.Contains(name, "::$iinit"), strings.Contains(name, "::$cinit"):
		return 1
	case strings.Contains(name, "script#"):
		return 3
	case strings.Contains(name, "::"):
		return 0
	}
	return 2
}

// methodOf 返回 method 索引的可读名。
func (nt *nameTable) methodOf(idx uint32) string {
	if n, ok := nt.names[idx]; ok {
		return n
	}
	return fmt.Sprintf("method#%d", idx)
}

// stringHints 收集函数体内的字符串字面量作为上下文提示。
func stringHints(f *abc.File, insns []*abc.Insn) []string {
	seen := map[string]bool{}
	var out []string
	for _, in := range insns {
		if in.Op != 0x2C { // pushstring
			continue
		}
		s := f.Pool.String(in.U30s[0])
		if s == "" || len(s) > 40 || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
		if len(out) >= 4 {
			break
		}
	}
	sort.Strings(out)
	return out
}

// Package cli 组装 swfkit 命令行界面。
package cli

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"swfkit/internal/abc"
	"swfkit/internal/patch"
	"swfkit/internal/sol"
	"swfkit/internal/swf"
	"swfkit/internal/web"
)

// NewRootCmd 构建根命令。
func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:          "swfkit",
		Short:        "通用 SWF 数值修改工具：AVM2 字节码补丁 + .sol 存档编辑",
		SilenceUsage: true,
	}
	root.AddCommand(
		newInfoCmd(),
		newABCListCmd(),
		newABCScanCmd(),
		newABCPatchCmd(),
		newSOLDumpCmd(),
		newSOLBuildCmd(),
		newSOLSetCmd(),
		newServeCmd(),
	)
	return root
}

func readSWF(path string) (*swf.File, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return swf.Parse(raw)
}

// parseValue 解析用户输入的数值（支持 0x 十六进制与负数）。
func parseValue(s string) (int64, float64, error) {
	s = strings.TrimSpace(s)
	if iv, err := strconv.ParseInt(s, 0, 64); err == nil {
		return iv, float64(iv), nil
	}
	if fv, err := strconv.ParseFloat(s, 64); err == nil {
		return int64(fv), fv, nil
	}
	return 0, 0, fmt.Errorf("无法解析数值 %q", s)
}

func newInfoCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "info <file.swf>",
		Short: "查看 SWF 概要与 ABC 模块统计",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			f, err := readSWF(args[0])
			if err != nil {
				return err
			}
			fmt.Println(f.String())
			abcCount := 0
			for i, t := range f.Tags {
				if !t.IsDoABC() {
					continue
				}
				abcCount++
				ta, err := abc.ParseTag(t, i)
				if err != nil {
					return err
				}
				fmt.Printf("  DoABC[%d] %q: 方法=%d 类=%d 脚本=%d 方法体=%d\n",
					i, ta.Name, len(ta.File.Methods), len(ta.File.Instances),
					len(ta.File.Scripts), len(ta.File.Bodies))
			}
			if abcCount == 0 {
				fmt.Println("  （未找到 DoABC：AS1/AS2 游戏，静态字节码补丁不适用；Web 运行时模式可正常扫内存改值）")
			}
			return nil
		},
	}
}

func newABCListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "abc-list <file.swf>",
		Short: "列出全部 ABC 类与方法",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			f, err := readSWF(args[0])
			if err != nil {
				return err
			}
			classes, err := patch.ListClasses(f)
			if err != nil {
				return err
			}
			for _, c := range classes {
				kind := "class"
				if c.IsInterface {
					kind = "interface"
				}
				fmt.Printf("[%d] %s %s extends %s (%d traits)\n", c.ABCIdx, kind, c.Name, c.Super, len(c.Traits))
				for _, tn := range c.Traits {
					fmt.Printf("      - %s\n", tn)
				}
			}
			if len(classes) == 0 {
				fmt.Println("(无类定义)")
			}
			return nil
		},
	}
}

func scanOptsFromFlags(mode string, value string) (patch.ScanOpts, error) {
	var opts patch.ScanOpts
	switch mode {
	case "any", "":
		opts.Mode = patch.ScanAny
	case "int":
		opts.Mode = patch.ScanInt
	case "uint":
		opts.Mode = patch.ScanUInt
	case "double":
		opts.Mode = patch.ScanDouble
	default:
		return opts, fmt.Errorf("未知模式 %q（可选 any/int/uint/double）", mode)
	}
	iv, fv, err := parseValue(value)
	if err != nil {
		return opts, err
	}
	opts.IntValue, opts.DoubleValue = iv, fv
	return opts, nil
}

func newABCScanCmd() *cobra.Command {
	var mode, value, where string
	cmd := &cobra.Command{
		Use:   "abc-scan <file.swf>",
		Short: "扫描 SWF 中的数值常量（类 CE 搜索）",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts, err := scanOptsFromFlags(mode, value)
			if err != nil {
				return err
			}
			f, err := readSWF(args[0])
			if err != nil {
				return err
			}
			hits, err := patch.Scan(f, opts)
			if err != nil {
				return err
			}
			for i, h := range hits {
				hints := strings.Join(h.Hints, ", ")
				fmt.Printf("#%-4d %-11s %-45s 字符串提示: [%s]\n", i+1, h.Source, h.Describe(), hints)
			}
			fmt.Printf("共 %d 处命中\n", len(hits))
			return nil
		},
	}
	cmd.Flags().StringVar(&mode, "mode", "any", "匹配口径：any/int/uint/double")
	cmd.Flags().StringVar(&value, "value", "", "搜索值（支持 100 / -5 / 0xFF / 3.14）")
	cmd.Flags().StringVar(&where, "where", "", "按归因子串过滤")
	_ = cmd.MarkFlagRequired("value")
	_ = where // 过滤在输出侧由 abc-patch 使用
	return cmd
}

func newABCPatchCmd() *cobra.Command {
	var mode, value, to, out, where string
	var index int
	var all bool
	cmd := &cobra.Command{
		Use:   "abc-patch <in.swf> -o <out.swf>",
		Short: "将扫描命中的数值改为新值并写出补丁后的 SWF",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts, err := scanOptsFromFlags(mode, value)
			if err != nil {
				return err
			}
			newInt, newDouble, err := parseValue(to)
			if err != nil {
				return fmt.Errorf("--to: %w", err)
			}
			f, err := readSWF(args[0])
			if err != nil {
				return err
			}
			hits, err := patch.Scan(f, opts)
			if err != nil {
				return err
			}
			if where != "" {
				hits = filterHits(hits, where)
			}
			var ops []*patch.Op
			switch {
			case all:
				for _, h := range hits {
					ops = append(ops, newOp(h, newInt, newDouble))
				}
			case index > 0:
				if index > len(hits) {
					return fmt.Errorf("--index=%d 超出命中范围（共 %d 处）", index, len(hits))
				}
				ops = append(ops, newOp(hits[index-1], newInt, newDouble))
			default:
				return fmt.Errorf("请指定 --index=N（见 abc-scan 输出序号）或 --all")
			}
			if len(ops) == 0 {
				return fmt.Errorf("没有可补丁的命中")
			}
			if err := patch.Apply(f, ops); err != nil {
				return err
			}
			data, err := f.Save()
			if err != nil {
				return err
			}
			if out == "" {
				out = strings.TrimSuffix(args[0], ".swf") + ".patched.swf"
			}
			if err := os.WriteFile(out, data, 0o644); err != nil {
				return err
			}
			fmt.Printf("已补丁 %d 处并写出 %s\n", len(ops), out)
			return nil
		},
	}
	cmd.Flags().StringVar(&mode, "mode", "any", "匹配口径：any/int/uint/double")
	cmd.Flags().StringVar(&value, "value", "", "搜索值")
	cmd.Flags().StringVar(&to, "to", "", "新值")
	cmd.Flags().StringVar(&where, "where", "", "按归因子串过滤命中（配合 --all）")
	cmd.Flags().IntVar(&index, "index", 0, "仅补丁 abc-scan 输出中的第 N 处（1 起始）")
	cmd.Flags().BoolVar(&all, "all", false, "补丁全部命中（先用 --where 收敛范围）")
	cmd.Flags().StringVarP(&out, "out", "o", "", "输出文件（默认 <in>.patched.swf）")
	_ = cmd.MarkFlagRequired("value")
	_ = cmd.MarkFlagRequired("to")
	return cmd
}

func newOp(h *patch.Hit, newInt int64, newDouble float64) *patch.Op {
	return &patch.Op{Hit: h, NewInt: newInt, NewDouble: newDouble}
}

func filterHits(hits []*patch.Hit, sub string) []*patch.Hit {
	var out []*patch.Hit
	for _, h := range hits {
		if strings.Contains(h.Where, sub) {
			out = append(out, h)
		}
	}
	return out
}

func newSOLDumpCmd() *cobra.Command {
	var out string
	cmd := &cobra.Command{
		Use:   "sol-dump <file.sol>",
		Short: "将 .sol 存档转为 JSON",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			raw, err := os.ReadFile(args[0])
			if err != nil {
				return err
			}
			doc, err := sol.Parse(raw)
			if err != nil {
				return err
			}
			jsonData, err := doc.MarshalJSON()
			if err != nil {
				return err
			}
			if out == "" {
				fmt.Println(string(jsonData))
				return nil
			}
			return os.WriteFile(out, jsonData, 0o644)
		},
	}
	cmd.Flags().StringVarP(&out, "out", "o", "", "输出 JSON 文件（默认打印到 stdout）")
	return cmd
}

func newSOLBuildCmd() *cobra.Command {
	var out string
	cmd := &cobra.Command{
		Use:   "sol-build <in.json> -o <out.sol>",
		Short: "将（sol-dump 产出的）JSON 重建为 .sol 存档",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			raw, err := os.ReadFile(args[0])
			if err != nil {
				return err
			}
			doc, err := sol.UnmarshalJSON(raw)
			if err != nil {
				return err
			}
			data, err := doc.Serialize()
			if err != nil {
				return err
			}
			if out == "" {
				return fmt.Errorf("请用 -o 指定输出 .sol 文件")
			}
			return os.WriteFile(out, data, 0o644)
		},
	}
	cmd.Flags().StringVarP(&out, "out", "o", "", "输出 .sol 文件")
	_ = cmd.MarkFlagRequired("out")
	return cmd
}

func newSOLSetCmd() *cobra.Command {
	var path, value, out string
	cmd := &cobra.Command{
		Use:   "sol-set <file.sol> --path=a.b.c --value=100 -o <out.sol>",
		Short: "按点分路径修改 .sol 字段后写出",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			raw, err := os.ReadFile(args[0])
			if err != nil {
				return err
			}
			doc, err := sol.Parse(raw)
			if err != nil {
				return err
			}
			v, err := sol.ParseScalar(value)
			if err != nil {
				return err
			}
			if err := doc.Set(path, v); err != nil {
				return err
			}
			data, err := doc.Serialize()
			if err != nil {
				return err
			}
			if out == "" {
				out = strings.TrimSuffix(args[0], ".sol") + ".patched.sol"
			}
			if err := os.WriteFile(out, data, 0o644); err != nil {
				return err
			}
			fmt.Printf("已写入 %s = %v → %s\n", path, v, out)
			return nil
		},
	}
	cmd.Flags().StringVar(&path, "path", "", "点分路径（如 player.gold；数组用 a.0.b）")
	cmd.Flags().StringVar(&value, "value", "", "新值（数字/true/false/null/字符串）")
	cmd.Flags().StringVarP(&out, "out", "o", "", "输出文件（默认 <in>.patched.sol）")
	_ = cmd.MarkFlagRequired("path")
	_ = cmd.MarkFlagRequired("value")
	return cmd
}

func newServeCmd() *cobra.Command {
	var addr string
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "启动本地 Web 修改器界面",
		RunE: func(cmd *cobra.Command, args []string) error {
			return web.Serve(addr)
		},
	}
	cmd.Flags().StringVar(&addr, "addr", "127.0.0.1:7777", "监听地址")
	return cmd
}

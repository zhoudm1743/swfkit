// swfkit —— 通用 SWF 数值修改工具。
package main

import (
	"os"

	"swfkit/internal/cli"
)

func main() {
	if err := cli.NewRootCmd().Execute(); err != nil {
		os.Exit(1)
	}
}

// 中文导读：命令入口——main；对应上游 packages/coding-agent/src/cli.ts。
package main

import (
	"os"

	"github.com/MichaelKinsy/PiG/coding/cli"
)

// Ports packages/coding-agent/src/cli.ts

func main() {
	cli.Main(os.Args[1:], nil)
}

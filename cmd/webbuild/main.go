// Command webbuild 把 cmd/gateway/web/src 下的模板与片段拼成 cmd/gateway/web/index.html。
//
// 产物随仓库提交：它要被 //go:embed 打进二进制，而构建镜像里不跑这一步
// （Dockerfile 只有 go build）。改完片段用 `make web-html` 重新生成。
//
// -check 只校验不写盘，给 CI 用：产物与源码不一致时非零退出。
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"ai-gateway/internal/webbuild"
)

func main() {
	webDir := flag.String("web", "cmd/gateway/web", "前端目录（含 src/ 与产物 index.html）")
	check := flag.Bool("check", false, "只校验产物是否与源码一致，不写盘")
	flag.Parse()

	out, err := webbuild.Assemble(*webDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "拼装失败:", err)
		os.Exit(1)
	}

	target := filepath.Join(*webDir, "index.html")

	if *check {
		current, err := os.ReadFile(target)
		if err != nil {
			fmt.Fprintln(os.Stderr, "读取产物失败:", err)
			os.Exit(1)
		}
		if !bytes.Equal(current, out) {
			fmt.Fprintf(os.Stderr,
				"%s 与 src/ 下的源码不一致（产物 %d 字节，重新拼装 %d 字节）。\n"+
					"请运行 `make web-html` 后提交产物。\n", target, len(current), len(out))
			os.Exit(1)
		}
		fmt.Printf("%s 与源码一致（%d 字节）\n", target, len(out))
		return
	}

	if err := os.WriteFile(target, out, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "写入产物失败:", err)
		os.Exit(1)
	}
	fmt.Printf("已生成 %s（%d 字节）\n", target, len(out))
}

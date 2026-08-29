// Package webbuild 把拆分后的前端片段拼回单个 index.html。
//
// 为什么需要它：`gatewayApp()` 一个对象曾有 1774 行、161 个成员，
// 单文件改起来定位困难。拆成 src/app/*.js.part 之后，部署形态仍必须是
// 单个 web/index.html——`//go:embed web/index.html web/vendor/*` 要求如此。
//
// 同一个 Assemble 供两处使用，避免两份拼装逻辑各自漂移：
//   - cmd/webbuild 生成随仓库提交的 web/index.html
//   - 开发模式（AI_GATEWAY_WEB_DIR）下由 server 实时拼装，保住热加载
package webbuild

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Marker 是模板里被成员体替换掉的占位行。
//
// 故意不写成合法 JS 注释：模板万一没被替换就该在浏览器里立刻语法报错，
// 而不是静默产出一个成员全空、页面处处 undefined 的 index.html。
const Marker = "        //<<<GW_APP_MEMBERS>>>"

// 片段目录与模板相对 web 根目录的位置。
const (
	templateRel = "src/index.template.html"
	fragmentRel = "src/app"
	fragmentExt = ".js.part"
)

// Assemble 读取 webDir 下的模板与片段，返回拼好的 index.html 内容。
//
// 片段按文件名排序拼接，所以文件名前缀（00-、01-…）就是成员顺序。
// 顺序会影响行为：同名成员后者覆盖前者，且 state 必须在方法之前可读。
func Assemble(webDir string) ([]byte, error) {
	tplPath := filepath.Join(webDir, filepath.FromSlash(templateRel))
	tpl, err := os.ReadFile(tplPath)
	if err != nil {
		return nil, fmt.Errorf("读取模板失败 (%s): %w", templateRel, err)
	}

	names, err := fragmentNames(filepath.Join(webDir, filepath.FromSlash(fragmentRel)))
	if err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("片段目录 %s 下没有 %s 文件", fragmentRel, fragmentExt)
	}

	dir := filepath.Join(webDir, filepath.FromSlash(fragmentRel))
	slices := make([][]byte, 0, len(names))
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, fmt.Errorf("读取片段失败 (%s): %w", name, err)
		}
		// 每个片段文件 = 原文的一段 + 一个行尾换行，所以只脱掉「一个」换行就还原成原始段落。
		// 不能用 TrimRight：它会把段落自身末尾的空行一并吃掉。
		// 成员之间靠空行分隔，切点又正好落在成员起始行上，于是有些片段本就以空行结尾——
		// 多吃一个换行，产物就会比原文少若干空行（实测少 8 行）。
		slices = append(slices, bytes.TrimSuffix(data, []byte("\n")))
	}
	body := bytes.Join(slices, []byte("\n"))

	marker := []byte(Marker + "\n")
	idx := bytes.Index(tpl, marker)
	if idx < 0 {
		// 容忍模板最后一行没有结尾换行的情况。
		if bytes.HasSuffix(tpl, []byte(Marker)) {
			idx = len(tpl) - len(Marker)
			marker = []byte(Marker)
		} else {
			return nil, fmt.Errorf("模板 %s 里找不到占位行 %q", templateRel, strings.TrimSpace(Marker))
		}
	}
	if bytes.Count(tpl, []byte(Marker)) != 1 {
		return nil, fmt.Errorf("模板 %s 里占位行出现 %d 次，应恰好 1 次", templateRel, bytes.Count(tpl, []byte(Marker)))
	}

	// marker 含结尾换行时，替换区间要停在换行之前（idx+len(marker)-1），
	// 让模板自带的那个换行留下来充当成员体的行尾；不减 1 会吞掉它、把 `};` 顶到同一行。
	tail := idx + len(marker)
	if bytes.HasSuffix(marker, []byte("\n")) {
		tail--
	}

	out := make([]byte, 0, len(tpl)+len(body))
	out = append(out, tpl[:idx]...)
	out = append(out, body...)
	out = append(out, tpl[tail:]...)
	return out, nil
}

// fragmentNames 列出片段文件名并排序。
func fragmentNames(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("读取片段目录失败 (%s): %w", fragmentRel, err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), fragmentExt) {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names, nil
}

// SourcePaths 返回参与拼装的全部源文件绝对路径。
// 开发模式的 SSE 监听要盯着这些文件的 mtime——只盯 index.html 的话，
// 改片段不会触发浏览器刷新，热加载就等于坏了。
func SourcePaths(webDir string) ([]string, error) {
	paths := []string{filepath.Join(webDir, filepath.FromSlash(templateRel))}
	dir := filepath.Join(webDir, filepath.FromSlash(fragmentRel))
	names, err := fragmentNames(dir)
	if err != nil {
		return nil, err
	}
	for _, name := range names {
		paths = append(paths, filepath.Join(dir, name))
	}
	return paths, nil
}

// HasSources 判断 webDir 是否是拆分后的源码目录。
// 开发模式挂载的目录可能只有构建好的 index.html（比如挂的是产物快照），
// 那种情况下应退回直接读 index.html，而不是报错。
func HasSources(webDir string) bool {
	if _, err := os.Stat(filepath.Join(webDir, filepath.FromSlash(templateRel))); err != nil {
		return false
	}
	names, err := fragmentNames(filepath.Join(webDir, filepath.FromSlash(fragmentRel)))
	return err == nil && len(names) > 0
}

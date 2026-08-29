package webbuild

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newSourceTree 造一个最小的 web 目录：模板 + 两个片段。
func newSourceTree(t *testing.T, fragments map[string]string, tpl string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "src", "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "src", "index.template.html"), []byte(tpl), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, body := range fragments {
		if err := os.WriteFile(filepath.Join(dir, "src", "app", name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestAssembleJoinsFragmentsInFilenameOrder(t *testing.T) {
	// 文件名顺序就是成员顺序，且必须与读目录的返回顺序无关。
	dir := newSourceTree(t, map[string]string{
		"01-second.js.part": "                second: 2,\n",
		"00-first.js.part":  "                first: 1,\n",
		"02-third.js.part":  "                third: 3\n",
	}, "head\n"+Marker+"\ntail\n")

	out, err := Assemble(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := "head\n                first: 1,\n                second: 2,\n                third: 3\ntail\n"
	if string(out) != want {
		t.Errorf("assembled =\n%q\nwant\n%q", out, want)
	}
}

// 片段末尾的空行必须保留。切点落在成员起始行上，成员之间又靠空行分隔，
// 于是不少片段本就以空行结尾。这里曾用 TrimRight 把它们一起吃掉，
// 产物比原文少 8 行——正是这个 bug 的回归测试。
func TestAssemblePreservesTrailingBlankLinesInsideFragments(t *testing.T) {
	dir := newSourceTree(t, map[string]string{
		// 该片段内容是 "a: 1," + 空行，文件再补一个行尾换行 => "a: 1,\n\n"
		"00-a.js.part": "                a: 1,\n\n",
		"01-b.js.part": "                b: 2\n",
	}, Marker+"\n")

	out, err := Assemble(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := "                a: 1,\n\n                b: 2\n"
	if string(out) != want {
		t.Errorf("assembled = %q, want %q (片段自带的空行被吃掉了)", out, want)
	}
}

func TestAssembleRejectsMissingOrDuplicateMarker(t *testing.T) {
	frags := map[string]string{"00-a.js.part": "                a: 1\n"}

	t.Run("missing", func(t *testing.T) {
		dir := newSourceTree(t, frags, "no marker here\n")
		if _, err := Assemble(dir); err == nil {
			t.Fatal("模板缺占位行时应报错")
		} else if !strings.Contains(err.Error(), "占位行") {
			t.Errorf("错误信息未说明占位行: %v", err)
		}
	})

	t.Run("duplicate", func(t *testing.T) {
		// 占位行出现两次时必须报错：静默只替换第一处会产出一个成员体重复、
		// 另一处残留裸标记的 index.html，浏览器里表现为语法错误。
		dir := newSourceTree(t, frags, Marker+"\n"+Marker+"\n")
		if _, err := Assemble(dir); err == nil {
			t.Fatal("占位行重复时应报错")
		}
	})
}

func TestAssembleRejectsEmptyFragmentDir(t *testing.T) {
	dir := newSourceTree(t, map[string]string{}, Marker+"\n")
	if _, err := Assemble(dir); err == nil {
		t.Fatal("片段目录为空时应报错，而不是产出成员全空的页面")
	}
}

func TestHasSourcesAndSourcePaths(t *testing.T) {
	dir := newSourceTree(t, map[string]string{
		"00-a.js.part": "                a: 1\n",
		"01-b.js.part": "                b: 2\n",
		"notes.md":     "不是片段，不该被算进去\n",
	}, Marker+"\n")

	if !HasSources(dir) {
		t.Error("HasSources 应为 true")
	}
	paths, err := SourcePaths(dir)
	if err != nil {
		t.Fatal(err)
	}
	// 模板 + 2 个片段；notes.md 后缀不符，必须排除。
	if len(paths) != 3 {
		t.Fatalf("SourcePaths 返回 %d 个路径，want 3: %v", len(paths), paths)
	}
	if !strings.HasSuffix(paths[0], "index.template.html") {
		t.Errorf("首个路径应是模板: %s", paths[0])
	}
	for _, p := range paths[1:] {
		if !strings.HasSuffix(p, fragmentExt) {
			t.Errorf("非片段文件混进来了: %s", p)
		}
	}

	// 只有产物、没有 src/ 的目录（比如挂了产物快照）应判为 false，让调用方退回读 index.html。
	bare := t.TempDir()
	if err := os.WriteFile(filepath.Join(bare, "index.html"), []byte("<html></html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if HasSources(bare) {
		t.Error("只有 index.html 的目录不该被判为源码目录")
	}
}

// 真实仓库校验：产物必须与 src/ 拼装结果逐字节一致。
// 这是「改了片段忘了跑 make web-html」的兜底，CI 的 -check 步骤同判据。
func TestRepoArtifactMatchesSources(t *testing.T) {
	webDir := filepath.Join("..", "..", "cmd", "gateway", "web")
	if !HasSources(webDir) {
		t.Skip("源码目录不存在，跳过")
	}
	want, err := Assemble(webDir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(webDir, "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("cmd/gateway/web/index.html 与 src/ 不一致（产物 %d 字节，拼装 %d 字节），请跑 `make web-html`", len(got), len(want))
	}
}

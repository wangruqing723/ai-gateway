package backupcrypto

import (
	"bytes"
	"flag"
	"os"
	"testing"
)

var updateFixture = flag.Bool("update", false, "重新生成加密容器格式夹具（仅假数据）")

// 在仓库根目录的验证容器中执行 go test ./internal/backupcrypto -run '^TestV1Fixture$' -update，
// 可用固定的假密码、盐、nonce 与 DefaultIterations 重新生成夹具。夹具本身不带尾随换行。
func TestV1Fixture(t *testing.T) {
	const path = "testdata/v1.aigw-backup"
	options := SealOptions{Iterations: DefaultIterations, Salt: []byte("0123456789abcdef"), Nonce: []byte("0123456789ab")}
	want, err := SealWith(testPlaintext, testPassword, options)
	if err != nil {
		t.Fatal(err)
	}
	if *updateFixture {
		if err := os.MkdirAll("testdata", 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, want, 0644); err != nil {
			t.Fatal(err)
		}
	}
	fixture, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(fixture, want) {
		t.Fatal("容器格式与v1夹具不一致；确认格式变更后使用 -update 更新")
	}
	got, err := Open(fixture, testPassword)
	if err != nil || !bytes.Equal(got, testPlaintext) {
		t.Fatalf("夹具解密失败：%v", err)
	}
	if info, err := Inspect(fixture); err != nil || info.Iterations != DefaultIterations {
		t.Fatalf("夹具参数=%+v 错误=%v", info, err)
	}
}

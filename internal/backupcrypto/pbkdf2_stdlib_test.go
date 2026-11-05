//go:build go1.24

package backupcrypto

import (
	"bytes"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"math/big"
	"testing"
)

// 与 Go 1.24+ 标准库 crypto/pbkdf2 交叉校验。文件级构建约束让 stdversion 分析按 1.24 处理本文件，
// 非测试代码仍不依赖 crypto/pbkdf2（模块声明 go 1.23）。
func TestPBKDF2SHA256MatchesStdlib(t *testing.T) {
	randInt := func(max int64) int {
		n, err := rand.Int(rand.Reader, big.NewInt(max))
		if err != nil {
			t.Fatal(err)
		}
		return int(n.Int64())
	}
	randBytes := func(n int) []byte {
		b := make([]byte, n)
		if _, err := rand.Read(b); err != nil {
			t.Fatal(err)
		}
		return b
	}
	for i := 0; i < 32; i++ {
		password := randBytes(randInt(200))
		salt := randBytes(randInt(64))
		iterations := 1 + randInt(3000)
		keyLen := 1 + randInt(100)
		want, err := pbkdf2.Key(sha256.New, string(password), salt, iterations, keyLen)
		if err != nil {
			t.Fatalf("标准库派生失败：%v", err)
		}
		got := pbkdf2SHA256(password, salt, iterations, keyLen)
		if !bytes.Equal(got, want) {
			t.Fatalf("第 %d 组参数（pwLen=%d saltLen=%d c=%d dkLen=%d）与标准库不符",
				i, len(password), len(salt), iterations, keyLen)
		}
	}
	// 再覆盖本包实际使用的参数形状：16 字节盐、32 字节密钥、MinIterations。
	password, salt := []byte("fake-backup-password"), randBytes(SaltSize)
	want, err := pbkdf2.Key(sha256.New, string(password), salt, MinIterations, KeySize)
	if err != nil {
		t.Fatal(err)
	}
	if got := pbkdf2SHA256(password, salt, MinIterations, KeySize); !bytes.Equal(got, want) {
		t.Fatal("MinIterations 参数下与标准库不符")
	}
}

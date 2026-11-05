package backupcrypto

import (
	"bytes"
	"encoding/hex"
	"testing"
)

// RFC 7914 第 11 节给出的 PBKDF2-HMAC-SHA256 测试向量。
func TestPBKDF2SHA256RFC7914Vectors(t *testing.T) {
	tests := []struct {
		name       string
		password   string
		salt       string
		iterations int
		keyLen     int
		want       string
	}{
		{
			name: "passwd/salt/c=1", password: "passwd", salt: "salt", iterations: 1, keyLen: 64,
			want: "55ac046e56e3089fec1691c22544b605f94185216dde0465e68b9d57c20dacbc" +
				"49ca9cccf179b645991664b39d77ef317c71b845b1e30bd509112041d3a19783",
		},
		{
			name: "Password/NaCl/c=80000", password: "Password", salt: "NaCl", iterations: 80000, keyLen: 64,
			want: "4ddcd8f60b98be21830cee5ef22701f9641a4418d04c0414aeff08876b34ab56" +
				"a1d425a1225833549adb841b51c9b3176a272bdebba1d078478f62b397f33c8d",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			want, err := hex.DecodeString(tt.want)
			if err != nil {
				t.Fatal(err)
			}
			got := pbkdf2SHA256([]byte(tt.password), []byte(tt.salt), tt.iterations, tt.keyLen)
			if !bytes.Equal(got, want) {
				t.Fatalf("派生结果不符\n got %x\nwant %x", got, want)
			}
		})
	}
}

// 密钥长度不是 32 的整数倍时按 RFC 8018 截断最后一块；前缀必须与更长输出一致。
func TestPBKDF2SHA256TruncatesLastBlock(t *testing.T) {
	long := pbkdf2SHA256([]byte("passwd"), []byte("salt"), 3, 64)
	for _, n := range []int{1, 16, 31, 32, 33, 50} {
		got := pbkdf2SHA256([]byte("passwd"), []byte("salt"), 3, n)
		if len(got) != n || !bytes.Equal(got, long[:n]) {
			t.Fatalf("keyLen=%d 截断结果不符：%x", n, got)
		}
	}
}

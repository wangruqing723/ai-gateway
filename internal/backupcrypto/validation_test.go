package backupcrypto

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestPasswordRules(t *testing.T) {
	tests := []struct {
		name, password string
		export         bool
		valid          bool
	}{
		{"空", "", false, false},
		{"恢复短密码", "短", false, true},
		{"导出过短", strings.Repeat("中", 11), true, false},
		{"导出按码点", strings.Repeat("中", 12), true, true},
		{"字节上限", strings.Repeat("a", MaxPasswordBytes), true, true},
		{"超字节上限", strings.Repeat("a", MaxPasswordBytes+1), false, false},
		{"非法UTF8", string([]byte{255}), false, false},
		{"NUL", "fake\x00backup-password", true, false},
		{"不规范化", strings.Repeat("é", 6), true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidatePassword(tt.password, tt.export)
			if tt.valid && err != nil {
				t.Fatal(err)
			}
			if !tt.valid && !errors.Is(err, ErrInvalidPassword) {
				t.Fatalf("错误=%v", err)
			}
		})
	}
	container := sealFast(t, testPlaintext)
	t.Run("Open密码检查先于派生", func(t *testing.T) {
		forbidDerive(t)
		if _, err := Open(container, ""); !errors.Is(err, ErrInvalidPassword) {
			t.Fatal(err)
		}
	})
}

func TestSealValidationAndInputOwnership(t *testing.T) {
	t.Run("参数检查先于派生", func(t *testing.T) {
		forbidDerive(t)
		tests := []struct {
			password  string
			plaintext []byte
			options   SealOptions
			want      error
		}{
			{"", nil, fastOptions, ErrInvalidPassword},
			{testPassword, make([]byte, MaxPlaintextBytes+1), fastOptions, ErrTooLarge},
			{testPassword, nil, SealOptions{Iterations: MinIterations - 1}, ErrUnsupported},
			{testPassword, nil, SealOptions{Iterations: MaxIterations + 1}, ErrUnsupported},
			{testPassword, nil, SealOptions{Iterations: -1}, ErrUnsupported},
			{testPassword, nil, SealOptions{Iterations: MinIterations, Salt: []byte{}}, ErrInvalidContainer},
			{testPassword, nil, SealOptions{Iterations: MinIterations, Nonce: []byte{1}}, ErrInvalidContainer},
		}
		for _, tt := range tests {
			if _, err := SealWith(tt.plaintext, tt.password, tt.options); !errors.Is(err, tt.want) {
				t.Fatalf("错误=%v，期望%v", err, tt.want)
			}
		}
	})
	plaintext := bytes.Clone(testPlaintext)
	salt := bytes.Repeat([]byte{1}, SaltSize)
	nonce := bytes.Repeat([]byte{2}, NonceSize)
	if _, err := SealWith(plaintext, testPassword, SealOptions{Iterations: MinIterations, Salt: salt, Nonce: nonce}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(plaintext, testPlaintext) || !bytes.Equal(salt, bytes.Repeat([]byte{1}, SaltSize)) || !bytes.Equal(nonce, bytes.Repeat([]byte{2}, NonceSize)) {
		t.Fatal("调用方切片被修改")
	}
}

func TestContainerParameterValidation(t *testing.T) {
	container := sealFast(t, testPlaintext)
	tests := []struct {
		name string
		edit func(map[string]any, map[string]any, map[string]any)
		want error
	}{
		{"迭代下界", func(_, k, _ map[string]any) { k["iterations"] = MinIterations - 1 }, ErrUnsupported},
		{"迭代上界", func(_, k, _ map[string]any) { k["iterations"] = MaxIterations + 1 }, ErrUnsupported},
		{"零迭代", func(_, k, _ map[string]any) { k["iterations"] = 0 }, ErrUnsupported},
		{"盐base64", func(_, k, _ map[string]any) { k["salt"] = "%" }, ErrInvalidContainer},
		{"盐长度", func(_, k, _ map[string]any) { k["salt"] = b64(SaltSize-1, 1) }, ErrInvalidContainer},
		{"盐换行", func(_, k, _ map[string]any) { k["salt"] = b64(SaltSize, 1) + "\n" }, ErrInvalidContainer},
		{"noncebase64", func(_, _, c map[string]any) { c["nonce"] = "%" }, ErrInvalidContainer},
		{"nonce长度", func(_, _, c map[string]any) { c["nonce"] = b64(NonceSize+1, 1) }, ErrInvalidContainer},
		{"密文base64", func(top, _, _ map[string]any) { top["ciphertext"] = "%" }, ErrInvalidContainer},
		{"短密文", func(top, _, _ map[string]any) { top["ciphertext"] = b64(15, 1) }, ErrInvalidContainer},
		{"空密文", func(top, _, _ map[string]any) { top["ciphertext"] = "" }, ErrInvalidContainer},
		{"长密文", func(top, _, _ map[string]any) { top["ciphertext"] = b64(MaxPlaintextBytes+17, 1) }, ErrInvalidContainer},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			forbidDerive(t)
			changed := mutate(t, container, tt.edit)
			if _, err := Inspect(changed); !errors.Is(err, tt.want) {
				t.Fatalf("Inspect=%v", err)
			}
			if _, err := Open(changed, testPassword); !errors.Is(err, tt.want) {
				t.Fatalf("Open=%v", err)
			}
		})
	}
	t.Run("容器大小优先", func(t *testing.T) {
		forbidDerive(t)
		large := make([]byte, MaxContainerBytes+1)
		if _, err := Inspect(large); !errors.Is(err, ErrTooLarge) {
			t.Fatal(err)
		}
		if _, err := Open(large, ""); !errors.Is(err, ErrTooLarge) {
			t.Fatal(err)
		}
	})
	t.Run("Inspect无需密码或派生", func(t *testing.T) {
		forbidDerive(t)
		if _, err := Inspect(container); err != nil {
			t.Fatal(err)
		}
		changed := mutate(t, container, func(_, k, _ map[string]any) { k["iterations"] = MaxIterations })
		if info, err := Inspect(changed); err != nil || info.Iterations != MaxIterations {
			t.Fatalf("Info=%+v 错误=%v", info, err)
		}
	})
}

func TestMaximumPlaintextRoundTrip(t *testing.T) {
	plaintext := bytes.Repeat([]byte{42}, MaxPlaintextBytes)
	container := sealFast(t, plaintext)
	if len(container) > MaxContainerBytes {
		t.Fatal("合法最大明文无法放入容器")
	}
	got, err := Open(container, testPassword)
	if err != nil || !bytes.Equal(got, plaintext) {
		t.Fatalf("最大明文往返失败：%v", err)
	}
}

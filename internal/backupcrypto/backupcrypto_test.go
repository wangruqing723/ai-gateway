package backupcrypto

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
)

// 本文件的测试不使用 t.Parallel：部分测试会临时替换包级 deriveKey。

const testPassword = "fake-backup-password"

var (
	testPlaintext = []byte(`{"version":1,"accounts":[]}`)
	fastOptions   = SealOptions{Iterations: MinIterations}
)

func sealFast(t *testing.T, plaintext []byte) []byte {
	t.Helper()
	container, err := SealWith(plaintext, testPassword, fastOptions)
	if err != nil {
		t.Fatalf("SealWith 失败：%v", err)
	}
	return container
}

// forbidDerive 让本测试期间任何密钥派生都判为失败，用来断言检查发生在派生之前。
func forbidDerive(t *testing.T) {
	t.Helper()
	original := deriveKey
	deriveKey = func([]byte, []byte, int) []byte {
		t.Errorf("不应走到密钥派生")
		return make([]byte, KeySize)
	}
	t.Cleanup(func() { deriveKey = original })
}

// mutate 把容器解码为通用 map，交给 edit 修改后重新编码。数字保持 json.Number 原样。
func mutate(t *testing.T, container []byte, edit func(top, kdf, cip map[string]any)) []byte {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(container))
	dec.UseNumber()
	var top map[string]any
	if err := dec.Decode(&top); err != nil {
		t.Fatal(err)
	}
	edit(top, top["kdf"].(map[string]any), top["cipher"].(map[string]any))
	out, err := json.Marshal(top)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func decodeOut(t *testing.T, container []byte) containerOut {
	t.Helper()
	var out containerOut
	if err := json.Unmarshal(container, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func b64(n int, fill byte) string {
	return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{fill}, n))
}

func TestRoundTrip(t *testing.T) {
	cases := map[string][]byte{
		"假 JSON": testPlaintext,
		"空明文":    {},
		"二进制":    {0x00, 0xff, 0x10, 0x00, 0x80},
	}
	for name, plaintext := range cases {
		t.Run(name, func(t *testing.T) {
			container := sealFast(t, plaintext)
			info, err := Inspect(container)
			if err != nil {
				t.Fatalf("Inspect 失败：%v", err)
			}
			want := Info{Version: Version, KDF: KDFName, Iterations: MinIterations, Cipher: CipherName}
			if info != want {
				t.Fatalf("Info = %+v，期望 %+v", info, want)
			}
			got, err := Open(container, testPassword)
			if err != nil {
				t.Fatalf("Open 失败：%v", err)
			}
			if got == nil || !bytes.Equal(got, plaintext) {
				t.Fatalf("明文不符：%q", got)
			}
		})
	}
}

// Seal 的默认参数用桩替换派生函数验证，避免在常规测试里多跑 600,000 次迭代；
// 真实的 DefaultIterations 派生由夹具测试覆盖。
func TestSealUsesDefaultIterations(t *testing.T) {
	original := deriveKey
	var seen []int
	deriveKey = func(_ []byte, _ []byte, iterations int) []byte {
		seen = append(seen, iterations)
		return bytes.Repeat([]byte{0x42}, KeySize)
	}
	t.Cleanup(func() { deriveKey = original })

	container, err := Seal(testPlaintext, testPassword)
	if err != nil {
		t.Fatal(err)
	}
	info, err := Inspect(container)
	if err != nil {
		t.Fatal(err)
	}
	if info.Iterations != DefaultIterations {
		t.Fatalf("迭代次数 = %d，期望 %d", info.Iterations, DefaultIterations)
	}
	got, err := Open(container, testPassword)
	if err != nil || !bytes.Equal(got, testPlaintext) {
		t.Fatalf("Open 失败：%v %q", err, got)
	}
	if len(seen) != 2 || seen[0] != DefaultIterations || seen[1] != DefaultIterations {
		t.Fatalf("派生时的迭代次数 = %v", seen)
	}
}

func TestSealOutputShape(t *testing.T) {
	container := sealFast(t, testPlaintext)
	prefix := `{"format":"ai-gateway-chatgpt-backup","version":1,"kdf":{"name":"pbkdf2-hmac-sha256","iterations":100000,"salt":"`
	if !bytes.HasPrefix(container, []byte(prefix)) {
		t.Fatalf("容器字段顺序或取值不符：%s", container)
	}
	out := decodeOut(t, container)
	wantTail := `"},"cipher":{"name":"aes-256-gcm","nonce":"` + out.Cipher.Nonce + `"},"ciphertext":"` + out.Ciphertext + `"}`
	if !bytes.HasSuffix(container, []byte(wantTail)) {
		t.Fatalf("容器尾部不符：%s", container)
	}
	raw, _ := base64.StdEncoding.DecodeString(out.Ciphertext)
	if len(raw) != len(testPlaintext)+gcmTagSize {
		t.Fatalf("密文长度 = %d", len(raw))
	}
}

func TestSealProducesFreshSaltAndNonce(t *testing.T) {
	a := decodeOut(t, sealFast(t, testPlaintext))
	b := decodeOut(t, sealFast(t, testPlaintext))
	if a.KDF.Salt == b.KDF.Salt {
		t.Fatal("两次 Seal 的盐相同")
	}
	if a.Cipher.Nonce == b.Cipher.Nonce {
		t.Fatal("两次 Seal 的 nonce 相同")
	}
	if a.Ciphertext == b.Ciphertext {
		t.Fatal("两次 Seal 的密文相同")
	}
}

func TestWrongPassword(t *testing.T) {
	container := sealFast(t, testPlaintext)
	got, err := Open(container, testPassword+"x")
	if !errors.Is(err, ErrDecrypt) {
		t.Fatalf("错误 = %v，期望 ErrDecrypt", err)
	}
	if got != nil {
		t.Fatal("失败时不应返回明文")
	}
	if err.Error() != ErrDecrypt.Error() {
		t.Fatalf("解密错误文本不应附带额外信息：%q", err.Error())
	}
	// 恢复只要求非空，短密码也应走到解密并得到 ErrDecrypt。
	if _, err := Open(container, "a"); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("短恢复密码错误 = %v，期望 ErrDecrypt", err)
	}
}

func TestAADTampering(t *testing.T) {
	container := sealFast(t, testPlaintext)
	tests := []struct {
		name string
		edit func(top, kdf, cip map[string]any)
		want error
	}{
		{"format", func(top, _, _ map[string]any) { top["format"] = Format + "x" }, ErrUnsupported},
		{"version", func(top, _, _ map[string]any) { top["version"] = 2 }, ErrUnsupported},
		{"kdf.name", func(_, kdf, _ map[string]any) { kdf["name"] = "sha256" }, ErrUnsupported},
		{"cipher.name", func(_, _, cip map[string]any) { cip["name"] = "aes-128-gcm" }, ErrUnsupported},
		{"iterations", func(_, kdf, _ map[string]any) { kdf["iterations"] = MinIterations + 1 }, ErrDecrypt},
		{"salt", func(_, kdf, _ map[string]any) { kdf["salt"] = b64(SaltSize, 9) }, ErrDecrypt},
		{"nonce", func(_, _, cip map[string]any) { cip["nonce"] = b64(NonceSize, 9) }, ErrDecrypt},
		{"ciphertext", func(top, _, _ map[string]any) {
			raw, _ := base64.StdEncoding.DecodeString(top["ciphertext"].(string))
			raw[0] ^= 1
			top["ciphertext"] = base64.StdEncoding.EncodeToString(raw)
		}, ErrDecrypt},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			changed := mutate(t, container, tt.edit)
			if tt.want != ErrDecrypt {
				forbidDerive(t)
			}
			got, err := Open(changed, testPassword)
			if got != nil || !errors.Is(err, tt.want) {
				t.Fatalf("Open = %q, %v，期望 %v", got, err, tt.want)
			}
		})
	}
}

// 独立构造 AAD 与 GCM 密文，避免 Seal/Open 同时漏掉某字段而让往返测试产生假阳性。
func TestAADExactDefinition(t *testing.T) {
	salt := bytes.Repeat([]byte{0x12}, SaltSize)
	nonce := bytes.Repeat([]byte{0x34}, NonceSize)
	saltB64, nonceB64 := base64.StdEncoding.EncodeToString(salt), base64.StdEncoding.EncodeToString(nonce)
	wantAAD := []byte(Format + "\x00v1\x00" + KDFName + "\x00" + strconv.Itoa(MinIterations) + "\x00" +
		saltB64 + "\x00" + CipherName + "\x00" + nonceB64)
	if got := additionalData(MinIterations, saltB64, nonceB64); !bytes.Equal(got, wantAAD) {
		t.Fatalf("AAD 不符：%q，期望 %q", got, wantAAD)
	}
	key := pbkdf2SHA256([]byte(testPassword), salt, MinIterations, KeySize)
	defer clear(key)
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	want := gcm.Seal(nil, nonce, testPlaintext, wantAAD)
	container, err := SealWith(testPlaintext, testPassword, SealOptions{Iterations: MinIterations, Salt: salt, Nonce: nonce})
	if err != nil {
		t.Fatal(err)
	}
	out := decodeOut(t, container)
	if out.Ciphertext != base64.StdEncoding.EncodeToString(want) {
		t.Fatal("Seal 的密文与独立构造的 AAD/GCM 不符")
	}
}

func TestStrictContainerJSON(t *testing.T) {
	base := string(sealFast(t, testPlaintext))
	tests := map[string]string{
		"空输入":          "",
		"非对象/null":     "null",
		"非对象/数组":       "[]",
		"非对象/字符串":      `"x"`,
		"截断":           base[:len(base)-1],
		"尾随对象":         base + `{}`,
		"尾随垃圾":         base + `x`,
		"顶层未知字段":       strings.Replace(base, `"format":`, `"extra":1,"format":`, 1),
		"嵌套未知字段":       strings.Replace(base, `"name":`, `"extra":1,"name":`, 1),
		"顶层重复键":        strings.Replace(base, `"version":1`, `"version":1,"version":1`, 1),
		"嵌套重复键/kdf":    strings.Replace(base, `"iterations":100000`, `"iterations":100000,"iterations":100000`, 1),
		"嵌套重复键/cipher": strings.Replace(base, `"name":"aes-256-gcm"`, `"name":"aes-256-gcm","name":"aes-256-gcm"`, 1),
		"转义键重复":        strings.Replace(base, `"version":1`, `"version":1,"vers\u0069on":1`, 1),
		"键名大小写":        strings.Replace(base, `"format":`, `"Format":`, 1),
		"类型不符/字符串":     strings.Replace(base, `"version":1`, `"version":"1"`, 1),
		"类型不符/小数":      strings.Replace(base, `"version":1`, `"version":1.0`, 1),
		"类型不符/数组":      strings.Replace(base, `"version":1`, `"version":[]`, 1),
		"类型不符/对象":      strings.Replace(base, `"version":1`, `"version":{}`, 1),
		"类型不符/布尔":      strings.Replace(base, `"version":1`, `"version":true`, 1),
		"过深嵌套":         `{"version":` + strings.Repeat("[", 100000) + strings.Repeat("]", 100000) + "}",
	}
	for name, data := range tests {
		t.Run(name, func(t *testing.T) {
			forbidDerive(t)
			if _, err := Inspect([]byte(data)); !errors.Is(err, ErrInvalidContainer) {
				t.Fatalf("Inspect 错误 = %v", err)
			}
			if _, err := Open([]byte(data), testPassword); !errors.Is(err, ErrInvalidContainer) {
				t.Fatalf("Open 错误 = %v", err)
			}
		})
	}
	// 必需字段逐个删去或设为 null（顶层与嵌套）。
	for _, field := range []string{"format", "version", "kdf", "cipher", "ciphertext", "kdf.name", "kdf.iterations", "kdf.salt", "cipher.name", "cipher.nonce"} {
		for _, mode := range []string{"缺失", "null"} {
			t.Run(field+"/"+mode, func(t *testing.T) {
				forbidDerive(t)
				data := mutate(t, []byte(base), func(top, kdf, cip map[string]any) {
					m, key := top, field
					if strings.HasPrefix(field, "kdf.") {
						m, key = kdf, strings.TrimPrefix(field, "kdf.")
					} else if strings.HasPrefix(field, "cipher.") {
						m, key = cip, strings.TrimPrefix(field, "cipher.")
					}
					if mode == "缺失" {
						delete(m, key)
					} else {
						m[key] = nil
					}
				})
				if _, err := Open(data, testPassword); !errors.Is(err, ErrInvalidContainer) {
					t.Fatalf("错误 = %v", err)
				}
			})
		}
	}
}

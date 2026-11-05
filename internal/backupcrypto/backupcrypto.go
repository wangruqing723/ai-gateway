// Package backupcrypto 实现 ChatGPT 账户原生备份的版本化加密容器（DESIGN 8.4、API_SPEC 8.5）。
// 只依赖标准库：AES-256-GCM 认证加密，PBKDF2-HMAC-SHA256 按 RFC 8018 用 crypto/hmac 实现。
// 模块声明 go 1.23，标准库 crypto/pbkdf2 需要 1.24，按 D08 不上抬 go 指令、不引入新依赖。
package backupcrypto

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"
)

// 容器格式常量。全部纳入 GCM 附加认证数据。
const (
	Format  = "ai-gateway-chatgpt-backup"
	Version = 1

	KDFName    = "pbkdf2-hmac-sha256"
	CipherName = "aes-256-gcm"

	// DefaultIterations 是导出使用的迭代次数（OWASP Password Storage Cheat Sheet，2026-11-05 核验）。
	DefaultIterations = 600_000
	// MinIterations、MaxIterations 是解密前接受的迭代次数范围；越界在派生密钥前拒绝。
	MinIterations = 100_000
	MaxIterations = 10_000_000

	SaltSize  = 16
	NonceSize = 12
	KeySize   = 32

	// MaxContainerBytes 是容器文件的大小上限，在解析前检查。
	MaxContainerBytes = 16 << 20
	// MaxPlaintextBytes 是明文上限：base64 膨胀与外层字段之后仍能放进 MaxContainerBytes。
	MaxPlaintextBytes = 11 << 20

	// MinExportPasswordChars 是导出密码的最少字符数（按 Unicode 码点计）；恢复只要求非空。
	MinExportPasswordChars = 12
	// MaxPasswordBytes 是密码字节上限。
	MaxPasswordBytes = 1024
)

var (
	// ErrInvalidContainer 表示容器不是严格的 JSON 对象、字段缺失或多余、base64 不合法或密文长度越界。
	ErrInvalidContainer = errors.New("备份文件结构无效")
	// ErrUnsupported 表示格式、版本、算法或迭代次数不受支持。
	ErrUnsupported = errors.New("备份格式或参数不受支持")
	// ErrDecrypt 表示密码错误或密文被篡改；两者故意不区分。
	ErrDecrypt = errors.New("密码错误或备份已损坏")
	// ErrTooLarge 表示容器或明文超过大小上限。
	ErrTooLarge = errors.New("备份文件超过大小上限")
	// ErrInvalidPassword 表示密码不符合要求（为空、过长、非法 UTF-8、含 NUL，导出时少于 12 个字符）。
	ErrInvalidPassword = errors.New("备份密码不符合要求")
)

// SealOptions 只供测试与夹具生成指定确定性参数；零值字段使用默认迭代次数与随机盐、nonce。
type SealOptions struct {
	Iterations int
	Salt       []byte
	Nonce      []byte
}

// Info 是不需要密码即可读取的外层参数，已通过与 Open 相同的结构与上下界检查。
type Info struct {
	Version    int
	KDF        string
	Iterations int
	Cipher     string
}

// deriveKey 是密钥派生入口。测试会临时替换它，用来断言被拒绝的容器根本没有走到派生这一步。
var deriveKey = func(password []byte, salt []byte, iterations int) []byte {
	return pbkdf2SHA256(password, salt, iterations, KeySize)
}

// ValidatePassword 校验密码；forExport 为 true 时另外要求至少 MinExportPasswordChars 个字符。
// 密码按原字节使用，不做 Unicode 规范化。
func ValidatePassword(password string, forExport bool) error {
	switch {
	case password == "":
		return fmt.Errorf("%w：密码为空", ErrInvalidPassword)
	case len(password) > MaxPasswordBytes:
		return fmt.Errorf("%w：密码超过 %d 字节", ErrInvalidPassword, MaxPasswordBytes)
	case !utf8.ValidString(password):
		return fmt.Errorf("%w：密码不是合法 UTF-8", ErrInvalidPassword)
	case strings.IndexByte(password, 0) >= 0:
		return fmt.Errorf("%w：密码包含 NUL 字符", ErrInvalidPassword)
	case forExport && utf8.RuneCountInString(password) < MinExportPasswordChars:
		return fmt.Errorf("%w：导出密码至少 %d 个字符", ErrInvalidPassword, MinExportPasswordChars)
	}
	return nil
}

// Seal 用默认参数加密明文，返回容器 JSON 字节。导出密码按 ValidatePassword(password, true) 校验。
func Seal(plaintext []byte, password string) ([]byte, error) {
	return SealWith(plaintext, password, SealOptions{})
}

// SealWith 与 Seal 相同，允许指定迭代次数（须在 MinIterations–MaxIterations 内）、盐和 nonce。
// 调用方传入的切片只读不改。
func SealWith(plaintext []byte, password string, options SealOptions) ([]byte, error) {
	if err := ValidatePassword(password, true); err != nil {
		return nil, err
	}
	if len(plaintext) > MaxPlaintextBytes {
		return nil, fmt.Errorf("%w：明文超过 %d 字节", ErrTooLarge, MaxPlaintextBytes)
	}

	iterations := options.Iterations
	if iterations == 0 {
		iterations = DefaultIterations
	}
	if iterations < MinIterations || iterations > MaxIterations {
		return nil, fmt.Errorf("%w：迭代次数须在 %d–%d 之间", ErrUnsupported, MinIterations, MaxIterations)
	}

	salt, err := sealParam(options.Salt, SaltSize, "盐")
	if err != nil {
		return nil, err
	}
	nonce, err := sealParam(options.Nonce, NonceSize, "nonce")
	if err != nil {
		return nil, err
	}

	saltB64 := base64.StdEncoding.EncodeToString(salt)
	nonceB64 := base64.StdEncoding.EncodeToString(nonce)
	aead, err := newAEAD(password, salt, iterations)
	if err != nil {
		return nil, err
	}
	sealed := aead.Seal(nil, nonce, plaintext, additionalData(iterations, saltB64, nonceB64))

	out := containerOut{
		Format:     Format,
		Version:    Version,
		KDF:        kdfOut{Name: KDFName, Iterations: iterations, Salt: saltB64},
		Cipher:     cipherOut{Name: CipherName, Nonce: nonceB64},
		Ciphertext: base64.StdEncoding.EncodeToString(sealed),
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("备份容器编码失败：%w", err)
	}
	return encoded, nil
}

// Inspect 只做结构与参数检查，不派生密钥。
func Inspect(container []byte) (Info, error) {
	parsed, err := parseContainer(container)
	if err != nil {
		return Info{}, err
	}
	return parsed.info(), nil
}

// Open 校验结构与参数后派生密钥并解密。密码按 ValidatePassword(password, false) 校验。
// 所有结构、参数与密码检查都在派生密钥之前完成；认证失败统一返回 ErrDecrypt，不区分密码错误与篡改。
func Open(container []byte, password string) ([]byte, error) {
	parsed, err := parseContainer(container)
	if err != nil {
		return nil, err
	}
	if err := ValidatePassword(password, false); err != nil {
		return nil, err
	}

	aead, err := newAEAD(password, parsed.salt, parsed.iterations)
	if err != nil {
		return nil, err
	}
	plaintext, err := aead.Open(nil, parsed.nonce, parsed.ciphertext,
		additionalData(parsed.iterations, parsed.saltB64, parsed.nonceB64))
	if err != nil {
		// 不包装底层错误，避免把任何密文相关信息带进错误文本。
		return nil, ErrDecrypt
	}
	if len(plaintext) > MaxPlaintextBytes {
		return nil, fmt.Errorf("%w：明文超过 %d 字节", ErrTooLarge, MaxPlaintextBytes)
	}
	if plaintext == nil {
		plaintext = []byte{}
	}
	return plaintext, nil
}

// sealParam 复制调用方给定的盐或 nonce 并校验长度；nil 时用 crypto/rand 生成。
func sealParam(given []byte, size int, label string) ([]byte, error) {
	if given == nil {
		buf := make([]byte, size)
		if _, err := io.ReadFull(rand.Reader, buf); err != nil {
			return nil, fmt.Errorf("生成随机%s失败：%w", label, err)
		}
		return buf, nil
	}
	if len(given) != size {
		return nil, fmt.Errorf("%w：%s长度须为 %d 字节", ErrInvalidContainer, label, size)
	}
	return bytes.Clone(given), nil
}

// newAEAD 派生密钥并构造 AES-256-GCM。
// 派生出的 key 与密码副本在构造完成后清零。这只是尽力而为：Go 的 GC 可能已复制过这些内存，
// AES 的轮密钥和 HMAC 内部缓冲也不在本包控制之内，无法保证内存中不留副本。
func newAEAD(password string, salt []byte, iterations int) (cipher.AEAD, error) {
	pw := []byte(password)
	key := deriveKey(pw, salt, iterations)
	defer clear(key)
	clear(pw)

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("初始化 AES 失败：%w", err)
	}
	aead, err := cipher.NewGCMWithNonceSize(block, NonceSize)
	if err != nil {
		return nil, fmt.Errorf("初始化 GCM 失败：%w", err)
	}
	return aead, nil
}

// additionalData 按 API_SPEC 8.5 构造附加认证数据：各段用单个 \x00 拼接。
// 盐与 nonce 使用容器里的 base64 文本；解析时已要求其为规范编码，因此与解码值一一对应。
func additionalData(iterations int, saltB64, nonceB64 string) []byte {
	parts := []string{
		Format,
		"v" + strconv.Itoa(Version),
		KDFName,
		strconv.Itoa(iterations),
		saltB64,
		CipherName,
		nonceB64,
	}
	return []byte(strings.Join(parts, "\x00"))
}

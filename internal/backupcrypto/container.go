package backupcrypto

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
)

// gcmTagSize 是 AES-GCM 认证标签长度。
const gcmTagSize = 16

// maxJSONDepth 限制容器 JSON 的嵌套深度。合法容器只有两层，限制深度可防止
// 恶意的深层嵌套在递归遍历时耗尽栈空间。
const maxJSONDepth = 32

// containerOut 是 Seal 输出的容器结构，字段顺序即输出顺序。
type containerOut struct {
	Format     string    `json:"format"`
	Version    int       `json:"version"`
	KDF        kdfOut    `json:"kdf"`
	Cipher     cipherOut `json:"cipher"`
	Ciphertext string    `json:"ciphertext"`
}

type kdfOut struct {
	Name       string `json:"name"`
	Iterations int    `json:"iterations"`
	Salt       string `json:"salt"`
}

type cipherOut struct {
	Name  string `json:"name"`
	Nonce string `json:"nonce"`
}

// containerIn 用指针字段区分「缺失或 null」与零值。
type containerIn struct {
	Format     *string   `json:"format"`
	Version    *int      `json:"version"`
	KDF        *kdfIn    `json:"kdf"`
	Cipher     *cipherIn `json:"cipher"`
	Ciphertext *string   `json:"ciphertext"`
}

type kdfIn struct {
	Name       *string `json:"name"`
	Iterations *int    `json:"iterations"`
	Salt       *string `json:"salt"`
}

type cipherIn struct {
	Name  *string `json:"name"`
	Nonce *string `json:"nonce"`
}

// parsedContainer 是通过全部结构与参数检查后的容器。
type parsedContainer struct {
	iterations int
	saltB64    string
	salt       []byte
	nonceB64   string
	nonce      []byte
	ciphertext []byte
}

func (p *parsedContainer) info() Info {
	return Info{Version: Version, KDF: KDFName, Iterations: p.iterations, Cipher: CipherName}
}

// schemaNode 描述某一层对象允许的键。值为 nil 表示该键的值不需要再按 schema 约束键名
// （仍会做重复键检查）；非 nil 表示该键的值必须按子 schema 检查。
type schemaNode map[string]schemaNode

// containerSchema 是容器 JSON 的精确键集合。键名区分大小写：标准库解码对键名大小写不敏感，
// 只靠 DisallowUnknownFields 会放过 "Format" 之类的变体。
var containerSchema = schemaNode{
	"format":     nil,
	"version":    nil,
	"kdf":        {"name": nil, "iterations": nil, "salt": nil},
	"cipher":     {"name": nil, "nonce": nil},
	"ciphertext": nil,
}

// errInvalid 返回不带任何输入片段的固定错误，避免把容器内容带进错误文本或日志。
func errInvalid(reason string) error {
	return fmt.Errorf("%w：%s", ErrInvalidContainer, reason)
}

func errUnsupported(reason string) error {
	return fmt.Errorf("%w：%s", ErrUnsupported, reason)
}

// parseContainer 按 DESIGN 8.4 的顺序检查容器，全部在派生密钥之前完成：
// 大小上限 → 严格 JSON → 格式、版本、算法与迭代次数 → 盐、nonce 与密文的编码和长度。
func parseContainer(container []byte) (*parsedContainer, error) {
	if len(container) > MaxContainerBytes {
		return nil, fmt.Errorf("%w：容器超过 %d 字节", ErrTooLarge, MaxContainerBytes)
	}

	if err := checkStrictJSON(container); err != nil {
		return nil, err
	}
	var in containerIn
	dec := json.NewDecoder(bytes.NewReader(container))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		// 不包装 json 的错误：其文本可能描述输入内容。
		return nil, errInvalid("容器字段类型不符")
	}
	if in.Format == nil || in.Version == nil || in.KDF == nil || in.Cipher == nil || in.Ciphertext == nil ||
		in.KDF.Name == nil || in.KDF.Iterations == nil || in.KDF.Salt == nil ||
		in.Cipher.Name == nil || in.Cipher.Nonce == nil {
		return nil, errInvalid("容器缺少必需字段")
	}

	switch {
	case *in.Format != Format:
		return nil, errUnsupported("未知的备份格式")
	case *in.Version != Version:
		return nil, errUnsupported("不支持的备份版本")
	case *in.KDF.Name != KDFName:
		return nil, errUnsupported("不支持的密钥派生算法")
	case *in.Cipher.Name != CipherName:
		return nil, errUnsupported("不支持的加密算法")
	case *in.KDF.Iterations < MinIterations || *in.KDF.Iterations > MaxIterations:
		return nil, errUnsupported("迭代次数超出允许范围")
	}

	salt, ok := decodeCanonicalBase64(*in.KDF.Salt)
	if !ok || len(salt) != SaltSize {
		return nil, errInvalid("盐编码或长度无效")
	}
	nonce, ok := decodeCanonicalBase64(*in.Cipher.Nonce)
	if !ok || len(nonce) != NonceSize {
		return nil, errInvalid("nonce 编码或长度无效")
	}
	// 先按编码长度粗筛，避免为超长密文分配内存；解码后再精确检查。
	if base64.StdEncoding.DecodedLen(len(*in.Ciphertext)) > MaxPlaintextBytes+gcmTagSize+2 {
		return nil, errInvalid("密文长度越界")
	}
	ciphertext, ok := decodeCanonicalBase64(*in.Ciphertext)
	if !ok {
		return nil, errInvalid("密文编码无效")
	}
	if len(ciphertext) < gcmTagSize || len(ciphertext) > MaxPlaintextBytes+gcmTagSize {
		return nil, errInvalid("密文长度越界")
	}

	return &parsedContainer{
		iterations: *in.KDF.Iterations,
		saltB64:    *in.KDF.Salt,
		salt:       salt,
		nonceB64:   *in.Cipher.Nonce,
		nonce:      nonce,
		ciphertext: ciphertext,
	}, nil
}

// decodeCanonicalBase64 只接受 StdEncoding 的规范编码。标准库解码会忽略 \r、\n，
// 也接受填充位非零的写法；这里要求重新编码后与原文一致，使 AAD 中的 base64 文本与解码值一一对应。
func decodeCanonicalBase64(s string) ([]byte, bool) {
	decoded, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, false
	}
	if base64.StdEncoding.EncodeToString(decoded) != s {
		return nil, false
	}
	return decoded, true
}

// checkStrictJSON 确认输入恰好是一个 JSON 对象：任何层级都没有重复键，schema 覆盖的层级没有未知键
// （区分大小写），对象之后没有尾随内容。
func checkStrictJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return errInvalid("容器不是合法 JSON")
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return errInvalid("容器必须是 JSON 对象")
	}
	if err := walkObject(dec, containerSchema, 1); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errInvalid("容器对象之后存在多余内容")
	}
	return nil
}

// walkObject 在已读入 '{' 之后遍历对象直到对应的 '}'。schema 为 nil 时不限制键名。
func walkObject(dec *json.Decoder, schema schemaNode, depth int) error {
	if depth > maxJSONDepth {
		return errInvalid("容器嵌套过深")
	}
	seen := make(map[string]struct{})
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return errInvalid("容器不是合法 JSON")
		}
		key, ok := tok.(string)
		if !ok {
			return errInvalid("容器不是合法 JSON")
		}
		if _, dup := seen[key]; dup {
			return errInvalid("容器存在重复键")
		}
		seen[key] = struct{}{}
		var child schemaNode
		if schema != nil {
			sub, known := schema[key]
			if !known {
				return errInvalid("容器存在未知字段")
			}
			child = sub
		}
		if err := walkValue(dec, child, depth); err != nil {
			return err
		}
	}
	if _, err := dec.Token(); err != nil { // 读掉 '}'
		return errInvalid("容器不是合法 JSON")
	}
	return nil
}

// walkValue 读取一个值；对象与数组递归检查。
func walkValue(dec *json.Decoder, schema schemaNode, depth int) error {
	tok, err := dec.Token()
	if err != nil {
		return errInvalid("容器不是合法 JSON")
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		return walkObject(dec, schema, depth+1)
	case '[':
		if depth+1 > maxJSONDepth {
			return errInvalid("容器嵌套过深")
		}
		for dec.More() {
			if err := walkValue(dec, nil, depth+1); err != nil {
				return err
			}
		}
		if _, err := dec.Token(); err != nil { // 读掉 ']'
			return errInvalid("容器不是合法 JSON")
		}
		return nil
	default:
		return errInvalid("容器不是合法 JSON")
	}
}

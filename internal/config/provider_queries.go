package config

import (
	"fmt"
	"math/big"
	"net"
	"net/url"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// ProviderBalance 显式声明查询适配器及身份，不从模型协议猜测上游类型。
type ProviderBalance struct {
	Type                   string `yaml:"type" json:"type"`
	Source                 string `yaml:"source,omitempty" json:"source,omitempty"`
	BaseURL                string `yaml:"baseUrl,omitempty" json:"baseUrl,omitempty"`
	AccessToken            string `yaml:"accessToken,omitempty" json:"accessToken,omitempty"`
	UserID                 string `yaml:"userId,omitempty" json:"userId,omitempty"`
	APIKeyID               string `yaml:"apiKeyId,omitempty" json:"apiKeyId,omitempty"`
	QuotaPerUnit           string `yaml:"quotaPerUnit,omitempty" json:"quotaPerUnit,omitempty"`
	RefreshIntervalMinutes *int   `yaml:"refreshIntervalMinutes,omitempty" json:"refreshIntervalMinutes,omitempty"`
}

// ModelCatalog 仅配置 OAuth 目录兼容版本，不允许自定义查询地址。
type ModelCatalog struct {
	ClientVersion string `yaml:"clientVersion,omitempty" json:"clientVersion,omitempty"`
}

// UnmarshalYAML 在解码前检查字段和类型，防止隐式字符串转换及错误回显令牌。
func (b *ProviderBalance) UnmarshalYAML(node *yaml.Node) error {
	type plain ProviderBalance
	if err := transferNodeShape(node, reflect.TypeOf(plain{}), "balance", 0); err != nil {
		return fmt.Errorf("balance 字段类型、名称或结构不合法")
	}
	if err := node.Decode((*plain)(b)); err != nil {
		return fmt.Errorf("balance 字段解码失败，请检查字段类型与数值范围")
	}
	return nil
}

// UnmarshalYAML 保持目录字段严格解码，与普通配置及迁移使用相同规则。
func (m *ModelCatalog) UnmarshalYAML(node *yaml.Node) error {
	type plain ModelCatalog
	if err := transferNodeShape(node, reflect.TypeOf(plain{}), "modelCatalog", 0); err != nil {
		return fmt.Errorf("modelCatalog 字段类型、名称或结构不合法")
	}
	if err := node.Decode((*plain)(m)); err != nil {
		return fmt.Errorf("modelCatalog 字段解码失败，请检查字段类型")
	}
	return nil
}

// EffectiveSource 将 auto 按独立令牌是否存在解析为唯一查询身份。
func (b *ProviderBalance) EffectiveSource() string {
	if b == nil {
		return ""
	}
	if b.Source == "" || b.Source == "auto" {
		if b.AccessToken != "" {
			return "account"
		}
		return "apiKey"
	}
	return b.Source
}

// IntervalMinutes 返回自动刷新周期；未配置表示只允许手动刷新。
func (b *ProviderBalance) IntervalMinutes() int {
	if b == nil {
		return 0
	}
	return IntOr(b.RefreshIntervalMinutes, 0)
}

// BalanceBaseURL 返回规范化的管理基址，默认仅移除调用基址末尾的 /v1。
// 非法输入返回空值；配置入口会明确拒绝，不将非法地址交给出网路径。
func (p *Provider) BalanceBaseURL() string {
	if p == nil || p.Balance == nil {
		return ""
	}
	raw := p.Balance.BaseURL
	inherited := raw == ""
	if inherited {
		raw = p.BaseURL
	}
	u, err := balanceURL(raw)
	if err != nil {
		return ""
	}
	// 保留转义路径，不将编码的斜杠或 v1 误识别成标准 API 后缀。
	path := strings.TrimRight(u.EscapedPath(), "/")
	if inherited {
		path = strings.TrimSuffix(path, "/v1")
	}
	u.Path, err = url.PathUnescape(path)
	if err != nil {
		return ""
	}
	u.RawPath = path
	return u.String()
}

// ModelCatalogVersion 不物化可选配置块，未指定版本时使用目录协议默认值。
func (p *Provider) ModelCatalogVersion() string {
	if p != nil && p.ModelCatalog != nil && p.ModelCatalog.ClientVersion != "" {
		return p.ModelCatalog.ClientVersion
	}
	return "0.167.0"
}

var catalogVersionPattern = regexp.MustCompile(`^[0-9]{1,6}\.[0-9]{1,6}\.[0-9]{1,6}$`)
var positiveIntegerPattern = regexp.MustCompile(`^[0-9]+$`)
var quotaUnitPattern = regexp.MustCompile(`^[0-9]+(\.[0-9]{1,18})?$`)

func validateProviderQueries(name string, p *Provider) error {
	label := "providers." + name
	if m := p.ModelCatalog; m != nil {
		if p.AuthType != "chatgptOAuth" {
			return fmt.Errorf("%s.modelCatalog 仅可用于 ChatGPT OAuth", label)
		}
		if m.ClientVersion != "" && (len(m.ClientVersion) > 32 || !catalogVersionPattern.MatchString(m.ClientVersion)) {
			return fmt.Errorf("%s.modelCatalog.clientVersion 须为 major.minor.patch，每段最多 6 位非负数字，最多 32 字节", label)
		}
	}
	b := p.Balance
	if b == nil {
		return nil
	}
	label += ".balance"
	if p.AuthType == "chatgptOAuth" {
		return fmt.Errorf("%s 不可用于 ChatGPT OAuth", label)
	}
	if b.Type != "newapi" && b.Type != "sub2api" {
		return fmt.Errorf("%s.type 须为 newapi 或 sub2api", label)
	}
	if b.Source != "" && b.Source != "auto" && b.Source != "account" && b.Source != "apiKey" {
		return fmt.Errorf("%s.source 须为 auto、account 或 apiKey", label)
	}
	if len(b.AccessToken) > 16*1024 || !utf8.ValidString(b.AccessToken) || hasControlCharacter(b.AccessToken) {
		return fmt.Errorf("%s.accessToken 须为不含控制字符的有效文本，最多 16 KiB", label)
	}
	if b.Source == "apiKey" && b.AccessToken != "" {
		return fmt.Errorf("%s.accessToken 不可与 source: apiKey 同时配置", label)
	}
	if b.EffectiveSource() == "account" && b.AccessToken == "" {
		return fmt.Errorf("%s.accessToken 在 account 模式下不能为空", label)
	}
	if b.EffectiveSource() == "account" && b.APIKeyID != "" {
		return fmt.Errorf("%s.apiKeyId 不可用于 account 模式", label)
	}
	if b.Type != "newapi" && (b.UserID != "" || b.QuotaPerUnit != "") {
		return fmt.Errorf("%s.userId 与 quotaPerUnit 仅可用于 New API", label)
	}
	if b.UserID != "" {
		id, err := strconv.ParseInt(b.UserID, 10, 64)
		if err != nil || id <= 0 || !positiveIntegerPattern.MatchString(b.UserID) {
			return fmt.Errorf("%s.userId 须为不超过有符号 64 位整数上限的正整数十进制字符串", label)
		}
	}
	if b.QuotaPerUnit != "" && !validQuotaPerUnit(b.QuotaPerUnit) {
		return fmt.Errorf("%s.quotaPerUnit 须为正十进制数，最多 64 字节、18 位小数且不大于 10^18", label)
	}
	interval := b.IntervalMinutes()
	if interval != 0 && (interval < 5 || interval > 1440) {
		return fmt.Errorf("%s.refreshIntervalMinutes 须为 0 或 5-1440 分钟", label)
	}
	queryURL, err := balanceURL(p.BalanceBaseURL())
	if err != nil {
		return fmt.Errorf("%s.baseUrl 须为无 userinfo、query、fragment 的有效 http/https 基址", label)
	}
	if b.EffectiveSource() == "apiKey" {
		providerURL, err := balanceURL(p.BaseURL)
		if err != nil || providerURL.Scheme != queryURL.Scheme || providerURL.Host != queryURL.Host {
			return fmt.Errorf("%s.baseUrl 使用模型密钥时须与提供商同 origin", label)
		}
		if p.UsesKeyList() {
			found := false
			for _, key := range p.APIKeys {
				if b.APIKeyID != "" && key.ID == b.APIKeyID {
					found = true
				}
			}
			if !found {
				return fmt.Errorf("%s.apiKeyId 须明确指定 apiKeys 中已有的 ID", label)
			}
		} else if b.APIKeyID != "" {
			return fmt.Errorf("%s.apiKeyId 仅可用于 apiKeys 列表模式", label)
		}
	}
	return nil
}

func validQuotaPerUnit(value string) bool {
	if len(value) > 64 || !quotaUnitPattern.MatchString(value) {
		return false
	}
	// 去掉整数部分前导零，避免 big.Rat 把纯整数的前导零当作八进制前缀。
	normalized := strings.TrimLeft(value, "0")
	if normalized == "" || strings.HasPrefix(normalized, ".") {
		normalized = "0" + normalized
	}
	n, ok := new(big.Rat).SetString(normalized)
	limit := new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil))
	return ok && n.Sign() > 0 && n.Cmp(limit) <= 0
}

// balanceURL 统一 origin 大小写和默认端口，同时保留同域子路径。
func balanceURL(raw string) (*url.URL, error) {
	u, err := url.ParseRequestURI(raw)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.ForceQuery || strings.Contains(raw, "#") || hasControlCharacter(raw) {
		return nil, fmt.Errorf("查询基址不合法")
	}
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return nil, fmt.Errorf("查询基址端口不合法")
		}
		port = strconv.Itoa(n)
	}
	if port == "80" && u.Scheme == "http" || port == "443" && u.Scheme == "https" {
		port = ""
	}
	u.Host = host
	if strings.Contains(host, ":") {
		u.Host = "[" + host + "]"
	}
	if port != "" {
		u.Host = net.JoinHostPort(host, port)
	}
	return u, nil
}

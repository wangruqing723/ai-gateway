// Package keypool 管理 Provider 密钥的有序选择、冷却与恢复事件。
package keypool

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"ai-gateway/internal/config"
)

const (
	StateDisabled   = "disabled"
	StateSelectable = "selectable"
	StateCooling    = "cooling"
)

// Fingerprint 返回密钥的短指纹，不包含密钥明文。
func Fingerprint(value string) string {
	if value == "" {
		return ""
	}
	sum := sha1.Sum([]byte(value))
	encoded := strings.ToUpper(hex.EncodeToString(sum[:]))
	return encoded[:8]
}

// Event 是密钥冷却或恢复事件。
type Event struct {
	Kind     string
	Provider string
	Detail   string
}

// Ticket 标识一次使用某个密钥的请求。Key.Value 仅供调用方构建上游请求。
type Ticket struct {
	Provider    string
	Key         config.ProviderKey
	Fingerprint string
	Sequence    uint64

	credHash   string
	connFP     string
	generation uint64
	cooldowns  map[string]time.Duration
}

// Snapshot 是 /health 对单个列表密钥公开的无机密状态。
type Snapshot struct {
	ID          string `json:"id"`
	Name        string `json:"name,omitempty"`
	Fingerprint string `json:"fingerprint"`
	Enabled     bool   `json:"enabled"`
	State       string `json:"state"`
	Reason      string `json:"reason,omitempty"`
	Status      int    `json:"status,omitempty"`
	RemainingMs int64  `json:"remainingMs,omitempty"`
	Until       string `json:"until,omitempty"`
}

type keyID struct {
	provider string
	id       string
}

type keyState struct {
	credHash    string
	connFP      string
	generation  uint64
	until       time.Time
	class       string
	status      int
	abnormalSeq uint64
	reported    map[string]bool
}

// Pool 保存跨请求的密钥冷却状态。
type Pool struct {
	mu        sync.Mutex
	states    map[keyID]*keyState
	providers map[string]string
	nextSeq   uint64
	nextGen   uint64
	now       func() time.Time
	onEvent   func(Event)
}

// New 创建密钥状态池；now 为 nil 时使用 time.Now。
func New(now func() time.Time) *Pool {
	if now == nil {
		now = time.Now
	}
	return &Pool{
		states:    make(map[keyID]*keyState),
		providers: make(map[string]string),
		now:       now,
	}
}

// SetEventHandler 设置事件回调。
func (p *Pool) SetEventHandler(handler func(Event)) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.onEvent = handler
	p.mu.Unlock()
}

// Reconcile 对齐运行时配置，清除删除或凭据版本已变化的状态。
func (p *Pool) Reconcile(cfg *config.Config) {
	if p == nil {
		return
	}
	desired := make(map[string]map[string]version)
	if cfg != nil {
		desired = make(map[string]map[string]version, len(cfg.Providers))
		for name, provider := range cfg.Providers {
			if provider == nil || !provider.UsesKeyList() {
				continue
			}
			connFP := connectionFingerprint(provider)
			keys := make(map[string]version, len(provider.APIKeys))
			for _, key := range provider.APIKeys {
				keys[key.ID] = version{credHash: credentialHash(key.Value), connFP: connFP}
			}
			desired[name] = keys
		}
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	for name, oldConnFP := range p.providers {
		newKeys, exists := desired[name]
		newConnFP := ""
		if exists && len(newKeys) > 0 {
			for _, next := range newKeys {
				newConnFP = next.connFP
				break
			}
		}
		if !exists || newConnFP != oldConnFP {
			for id := range p.states {
				if id.provider == name {
					delete(p.states, id)
				}
			}
		}
	}

	for id, state := range p.states {
		keys, exists := desired[id.provider]
		if !exists {
			delete(p.states, id)
			continue
		}
		current, exists := keys[id.id]
		if !exists || current.credHash != state.credHash || current.connFP != state.connFP {
			delete(p.states, id)
		}
	}

	p.providers = make(map[string]string, len(desired))
	for name, keys := range desired {
		for _, current := range keys {
			p.providers[name] = current.connFP
			break
		}
		for id, current := range keys {
			key := keyID{provider: name, id: id}
			if p.states[key] != nil {
				continue
			}
			p.nextGen++
			p.states[key] = &keyState{
				credHash:   current.credHash,
				connFP:     current.connFP,
				generation: p.nextGen,
				reported:   make(map[string]bool),
			}
		}
	}
}

type version struct {
	credHash string
	connFP   string
}

// Select 按配置顺序选择可用密钥。无可选密钥时返回最早冷却截止时间。
func (p *Pool) Select(providerName string, provider *config.Provider, excluded map[string]struct{}) (*Ticket, time.Time) {
	if p == nil || provider == nil || !provider.UsesKeyList() {
		return nil, time.Time{}
	}
	connFP := connectionFingerprint(provider)
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	var earliest time.Time
	for _, key := range provider.APIKeys {
		if !key.IsEnabled() {
			continue
		}
		if _, skip := excluded[key.ID]; skip {
			continue
		}
		state := p.matchingStateLocked(providerName, key, connFP)
		if state != nil && now.Before(state.until) {
			if earliest.IsZero() || state.until.Before(earliest) {
				earliest = state.until
			}
			continue
		}
		p.nextSeq++
		ticket := &Ticket{
			Provider:    providerName,
			Key:         key,
			Fingerprint: Fingerprint(key.Value),
			Sequence:    p.nextSeq,
			credHash:    credentialHash(key.Value),
			connFP:      connFP,
			cooldowns: map[string]time.Duration{
				"auth":      time.Duration(provider.KeyCooldownFor("auth")) * time.Millisecond,
				"quota":     time.Duration(provider.KeyCooldownFor("quota")) * time.Millisecond,
				"rateLimit": time.Duration(provider.KeyCooldownFor("rateLimit")) * time.Millisecond,
			},
		}
		if state != nil {
			ticket.generation = state.generation
		}
		return ticket, earliest
	}
	return nil, earliest
}

// HasSelectable 判断排除集合之外是否还有可选密钥，不发放 Ticket。
func (p *Pool) HasSelectable(providerName string, provider *config.Provider, excluded map[string]struct{}) bool {
	if p == nil || provider == nil || !provider.UsesKeyList() {
		return false
	}
	connFP := connectionFingerprint(provider)
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	for _, key := range provider.APIKeys {
		if !key.IsEnabled() {
			continue
		}
		if _, skip := excluded[key.ID]; skip {
			continue
		}
		state := p.matchingStateLocked(providerName, key, connFP)
		if state == nil || !now.Before(state.until) {
			return true
		}
	}
	return false
}

// EarliestRecovery 返回当前已启用且冷却中的密钥最早恢复时间。
func (p *Pool) EarliestRecovery(providerName string, provider *config.Provider) time.Time {
	if p == nil || provider == nil || !provider.UsesKeyList() {
		return time.Time{}
	}
	connFP := connectionFingerprint(provider)
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	var earliest time.Time
	for _, key := range provider.APIKeys {
		if !key.IsEnabled() {
			continue
		}
		state := p.matchingStateLocked(providerName, key, connFP)
		if state == nil || !now.Before(state.until) {
			continue
		}
		if earliest.IsZero() || state.until.Before(earliest) {
			earliest = state.until
		}
	}
	return earliest
}

func (p *Pool) matchingStateLocked(providerName string, key config.ProviderKey, connFP string) *keyState {
	state := p.states[keyID{provider: providerName, id: key.ID}]
	if state == nil || state.credHash != credentialHash(key.Value) || state.connFP != connFP {
		return nil
	}
	return state
}

// Report 上报一次真实请求结果。成功只用于恢复判定，不清除冷却。
func (p *Pool) Report(ticket *Ticket, status int, retryAfter time.Duration) {
	if p == nil || ticket == nil {
		return
	}
	var event *Event
	p.mu.Lock()
	state := p.states[keyID{provider: ticket.Provider, id: ticket.Key.ID}]
	if state == nil || state.generation != ticket.generation || state.credHash != ticket.credHash || state.connFP != ticket.connFP {
		p.mu.Unlock()
		return
	}
	now := p.now()
	switch {
	case status >= 200 && status < 400:
		if state.abnormalSeq != 0 && ticket.Sequence > state.abnormalSeq {
			detail := keyDetail(ticket, fmt.Sprintf("后续请求成功，密钥异常已恢复"))
			event = &Event{Kind: "key_recovered", Provider: ticket.Provider, Detail: detail}
			state.abnormalSeq = 0
			state.reported = make(map[string]bool)
		}
	case status == 401 || status == 403 || status == 402 || status == 429:
		class, duration := cooldownFor(ticket, status, retryAfter)
		if duration > 0 {
			if class == "auth" || class == "quota" {
				state.abnormalSeq = ticket.Sequence
			}
			until := now.Add(duration)
			if until.After(state.until) {
				state.until = until
			}
			state.class = class
			state.status = status
			if class != "rateLimit" && !state.reported[class] {
				detail := keyDetail(ticket, fmt.Sprintf("返回 HTTP %d，冷却 %dms", status, duration.Milliseconds()))
				event = &Event{Kind: "key_cooldown", Provider: ticket.Provider, Detail: detail}
				state.reported[class] = true
			}
		}
	}
	handler := p.onEvent
	p.mu.Unlock()
	if event != nil && handler != nil {
		handler(*event)
	}
}

func cooldownFor(ticket *Ticket, status int, retryAfter time.Duration) (string, time.Duration) {
	var class string
	switch status {
	case 401, 403:
		class = "auth"
	case 402:
		class = "quota"
	case 429:
		class = "rateLimit"
	default:
		return "", 0
	}
	if class != "auth" && retryAfter > 0 {
		return class, retryAfter
	}
	return class, ticket.cooldowns[class]
}

// Release 清除指定密钥的冷却，返回调用前是否仍处于冷却中。
func (p *Pool) Release(providerName, keyIDValue string) bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	state := p.states[keyID{provider: providerName, id: keyIDValue}]
	if state == nil || !p.now().Before(state.until) {
		return false
	}
	state.until = time.Time{}
	state.class = ""
	state.status = 0
	return true
}

// Snapshot 返回列表模式密钥的健康状态快照。
func (p *Pool) Snapshot(cfg *config.Config) map[string][]Snapshot {
	out := make(map[string][]Snapshot)
	if cfg == nil {
		return out
	}
	if p != nil {
		p.mu.Lock()
		defer p.mu.Unlock()
		now := p.now()
		for name, provider := range cfg.Providers {
			if provider == nil || !provider.UsesKeyList() {
				continue
			}
			connFP := connectionFingerprint(provider)
			entries := make([]Snapshot, 0, len(provider.APIKeys))
			for _, key := range provider.APIKeys {
				entry := makeSnapshot(key)
				if state := p.matchingStateLocked(name, key, connFP); state != nil && now.Before(state.until) {
					entry.RemainingMs = state.until.Sub(now).Milliseconds()
					if entry.RemainingMs < 1 {
						entry.RemainingMs = 1
					}
					entry.Until = state.until.Format(time.RFC3339Nano)
					entry.Reason = state.class
					entry.Status = state.status
					if key.IsEnabled() {
						entry.State = StateCooling
					}
				}
				entries = append(entries, entry)
			}
			out[name] = entries
		}
		return out
	}
	for name, provider := range cfg.Providers {
		if provider == nil || !provider.UsesKeyList() {
			continue
		}
		entries := make([]Snapshot, 0, len(provider.APIKeys))
		for _, key := range provider.APIKeys {
			entries = append(entries, makeSnapshot(key))
		}
		out[name] = entries
	}
	return out
}

func makeSnapshot(key config.ProviderKey) Snapshot {
	state := StateSelectable
	if !key.IsEnabled() {
		state = StateDisabled
	}
	return Snapshot{ID: key.ID, Name: key.Name, Fingerprint: Fingerprint(key.Value), Enabled: key.IsEnabled(), State: state}
}

func keyDetail(ticket *Ticket, message string) string {
	identity := ticket.Key.ID
	if ticket.Key.Name != "" {
		identity = ticket.Key.Name + "（" + identity + "）"
	}
	return fmt.Sprintf("密钥 %s：%s", identity, message)
}

func credentialHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func connectionFingerprint(provider *config.Provider) string {
	keys := make([]string, 0, len(provider.ExtraHeaders))
	for key := range provider.ExtraHeaders {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var headers strings.Builder
	for _, key := range keys {
		fmt.Fprintf(&headers, "%s\x00%s\x00", key, provider.ExtraHeaders[key])
	}
	identity := strings.Join([]string{provider.BaseURL, provider.Format, provider.AuthType, provider.Proxy, provider.UserAgent, headers.String()}, "\x00")
	sum := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(sum[:])
}

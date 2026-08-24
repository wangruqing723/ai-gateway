package config

import (
	"os"
	"testing"
)

// TestExampleConfigPassesStrictDecode 确认 config.example.yaml 能过严格解码与校验，
// 防止新增配置块只改了 struct 却漏改模板（或模板字段名拼错）。
func TestExampleConfigPassesStrictDecode(t *testing.T) {
	data, err := os.ReadFile("../../config.example.yaml")
	if err != nil {
		t.Fatalf("读取 config.example.yaml 失败: %v", err)
	}
	cfg, err := DecodeAndValidate(data)
	if err != nil {
		t.Fatalf("config.example.yaml 未通过严格解码/校验: %v", err)
	}
	if got := cfg.Breaker.FailureThreshold(); got != 3 {
		t.Errorf("Breaker.ConsecutiveFailures = %d, 期望 3", got)
	}
	if got := cfg.Breaker.CooldownMs(); got != 30000 {
		t.Errorf("Breaker.OpenMs = %d, 期望 30000", got)
	}
	if got := cfg.Breaker.ProbeLimit(); got != 1 {
		t.Errorf("Breaker.HalfOpenProbes = %d, 期望 1", got)
	}
	if cfg.Breaker.Enabled {
		t.Error("Breaker.Enabled = true, 期望模板里默认关闭")
	}
}

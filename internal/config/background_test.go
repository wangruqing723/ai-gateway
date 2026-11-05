package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// 第六阶段：后台刷新配置的默认值、边界、严格解码与落盘形状。

func TestChatGPTBackgroundDefaultsAndBounds(t *testing.T) {
	usageErr := "chatgptBackground.usageIntervalMinutes 应在 30-1440 之间；使用默认值请删除该项"
	activeErr := "chatgptBackground.activeWindowMinutes 应在 5-1440 之间；使用默认值请删除该项"
	cases := []struct {
		name    string
		section string
		wantErr string
		enabled bool
		usage   int
		active  int
	}{
		{"未配置", "", "", false, 30, 60},
		{"空配置", "chatgptBackground: {}\n", "", false, 30, 60},
		{"只开令牌刷新", "chatgptBackground:\n  tokenRefresh: true\n", "", true, 30, 60},
		{"只开用量刷新", "chatgptBackground:\n  usageRefresh: true\n", "", true, 30, 60},
		{"同时开启", "chatgptBackground:\n  tokenRefresh: true\n  usageRefresh: true\n", "", true, 30, 60},
		{"显式关闭", "chatgptBackground:\n  tokenRefresh: false\n  usageRefresh: false\n", "", false, 30, 60},
		{"周期下界", "chatgptBackground:\n  usageIntervalMinutes: 30\n  activeWindowMinutes: 5\n", "", false, 30, 5},
		{"周期上界", "chatgptBackground:\n  usageIntervalMinutes: 1440\n  activeWindowMinutes: 1440\n", "", false, 1440, 1440},
		{"用量周期为零", "chatgptBackground:\n  usageIntervalMinutes: 0\n", usageErr, false, 0, 0},
		{"用量周期低于下界", "chatgptBackground:\n  usageIntervalMinutes: 29\n", usageErr, false, 0, 0},
		{"用量周期高于上界", "chatgptBackground:\n  usageIntervalMinutes: 1441\n", usageErr, false, 0, 0},
		{"用量周期为负", "chatgptBackground:\n  usageIntervalMinutes: -1\n", usageErr, false, 0, 0},
		{"活跃窗口为零", "chatgptBackground:\n  activeWindowMinutes: 0\n", activeErr, false, 0, 0},
		{"活跃窗口低于下界", "chatgptBackground:\n  activeWindowMinutes: 4\n", activeErr, false, 0, 0},
		{"活跃窗口高于上界", "chatgptBackground:\n  activeWindowMinutes: 1441\n", activeErr, false, 0, 0},
		{"活跃窗口为负", "chatgptBackground:\n  activeWindowMinutes: -1\n", activeErr, false, 0, 0},
		{"拒绝未知字段", "chatgptBackground:\n  unknownRefresh: true\n", "unknownRefresh", false, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := DecodeAndValidate([]byte(tc.section + validConfigYAML()))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v，期望包含 %s", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			b := cfg.ChatGPTBackground
			if b.Enabled() != tc.enabled || b.UsageInterval() != tc.usage || b.ActiveWindow() != tc.active {
				t.Fatalf("后台配置 = %#v，启用=%v，用量周期=%d，活跃窗口=%d", b, b.Enabled(), b.UsageInterval(), b.ActiveWindow())
			}
			// 未写的周期必须保持 nil，默认值只由 accessor 提供。
			if !strings.Contains(tc.section, "usageIntervalMinutes:") && b.UsageIntervalMinutes != nil {
				t.Fatal("未配置的用量周期应保持 nil")
			}
			if !strings.Contains(tc.section, "activeWindowMinutes:") && b.ActiveWindowMinutes != nil {
				t.Fatal("未配置的活跃窗口应保持 nil")
			}
		})
	}
}

// 零值整段省略落盘；写了的项（含与默认值相同的显式周期）原样保留并可重新加载。
func TestChatGPTBackgroundRoundTrip(t *testing.T) {
	cases := []struct {
		name    string
		section string
		present []string
		absent  []string
	}{
		{"未配置省略整段", "", nil, []string{"chatgptBackground"}},
		{"零值省略整段", "chatgptBackground:\n  tokenRefresh: false\n  usageRefresh: false\n", nil, []string{"chatgptBackground"}},
		{"仅令牌刷新", "chatgptBackground:\n  tokenRefresh: true\n", []string{"tokenRefresh: true"}, []string{"usageRefresh", "usageIntervalMinutes", "activeWindowMinutes"}},
		{"仅用量刷新", "chatgptBackground:\n  usageRefresh: true\n", []string{"usageRefresh: true"}, []string{"tokenRefresh", "usageIntervalMinutes", "activeWindowMinutes"}},
		{"显式默认周期不省略", "chatgptBackground:\n  usageIntervalMinutes: 30\n  activeWindowMinutes: 60\n", []string{"usageIntervalMinutes: 30", "activeWindowMinutes: 60"}, []string{"tokenRefresh", "usageRefresh"}},
		{"显式自定义值保留", "chatgptBackground:\n  tokenRefresh: true\n  usageRefresh: true\n  usageIntervalMinutes: 1440\n  activeWindowMinutes: 5\n", []string{"tokenRefresh: true", "usageRefresh: true", "usageIntervalMinutes: 1440", "activeWindowMinutes: 5"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := DecodeAndValidate([]byte(tc.section + validConfigYAML()))
			if err != nil {
				t.Fatal(err)
			}
			out, err := yaml.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			for _, field := range tc.present {
				if !strings.Contains(string(out), field) {
					t.Fatalf("落盘应保留 %s：\n%s", field, out)
				}
			}
			for _, field := range tc.absent {
				if strings.Contains(string(out), field) {
					t.Fatalf("落盘应省略 %s：\n%s", field, out)
				}
			}
			again, err := DecodeAndValidate(out)
			if err != nil {
				t.Fatalf("落盘结果应能重新加载：%v\n%s", err, out)
			}
			before, after := cfg.ChatGPTBackground, again.ChatGPTBackground
			if before.TokenRefresh != after.TokenRefresh || before.UsageRefresh != after.UsageRefresh ||
				!sameOptionalInt(before.UsageIntervalMinutes, after.UsageIntervalMinutes) ||
				!sameOptionalInt(before.ActiveWindowMinutes, after.ActiveWindowMinutes) {
				t.Fatalf("往返前=%#v，往返后=%#v", before, after)
			}
		})
	}
}

func sameOptionalInt(a, b *int) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

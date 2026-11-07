package providerbalance

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

func successfulNewAPI(body map[string]any) bool {
	valid := false
	for _, key := range []string{"success", "code"} {
		if v, exists := body[key]; exists {
			flag, ok := v.(bool)
			if !ok || !flag {
				return false
			}
			valid = true
		}
	}
	return valid
}
func parseNewAPI(body map[string]any, q Query) ([]Item, *QueryError) {
	if !successfulNewAPI(body) {
		if value, ok := body["success"].(bool); ok && !value {
			return nil, failure("credential_rejected", 401)
		}
		if value, ok := body["code"].(bool); ok && !value {
			return nil, failure("credential_rejected", 401)
		}
		return nil, invalid()
	}
	data, ok := object(body["data"])
	if !ok {
		return nil, invalid()
	}
	if q.Source == "account" {
		if id, exists := data["id"]; exists && q.UserID != "" {
			var raw string
			switch value := id.(type) {
			case json.Number:
				raw = string(value)
			case string:
				raw = value
			default:
				return nil, invalid()
			}
			actual, e := strconv.ParseInt(raw, 10, 64)
			expected, _ := strconv.ParseInt(q.UserID, 10, 64)
			if e != nil || actual <= 0 || strings.Trim(raw, "0123456789") != "" || actual != expected {
				return nil, invalid()
			}
		}
		item := newItem("account_balance", "newapi.user_self", "quota", q)
		if !readAmounts(&item, data, "quota", "used_quota", "") || item.Remaining == nil {
			return nil, invalid()
		}
		preserveRaw(&item)
		return []Item{item}, nil
	}
	item := newItem("key_quota", "newapi.token_usage", "quota", q)
	unlimited, ok := data["unlimited_quota"].(bool)
	if !ok {
		return nil, invalid()
	}
	item.Unlimited = unlimited
	if !readAmounts(&item, data, "total_available", "total_used", "total_granted") || (!unlimited && item.Remaining == nil) {
		return nil, invalid()
	}
	preserveRaw(&item)
	if unlimited {
		item.Remaining = nil
		item.Limit = nil
	}
	items := []Item{item}
	if value, exists := data["user_quota"]; exists && value != nil {
		user, ok := object(value)
		if !ok {
			return nil, invalid()
		}
		account := newItem("account_balance", "newapi.token_usage.user_quota", "quota", q)
		if !readAmounts(&account, user, "total_available", "total_used", "") || account.Remaining == nil {
			return nil, invalid()
		}
		preserveRaw(&account)
		items = append(items, account)
	}
	return items, nil
}
func preserveRaw(item *Item) {
	item.RawRemaining = item.Remaining
	item.RawUsed = item.Used
	item.RawUnit = ptr("quota")
}
func readAmounts(item *Item, data map[string]any, remaining, used, limit string) bool {
	var ok bool
	item.Remaining, ok = amount(data, remaining, false)
	if !ok {
		return false
	}
	item.Used, ok = amount(data, used, true)
	if !ok {
		return false
	}
	item.Limit, ok = amount(data, limit, true)
	return ok
}

func parseSub2API(body map[string]any, q Query) ([]Item, *QueryError) {
	if q.Source == "account" {
		code, ok := body["code"].(json.Number)
		if !ok {
			return nil, invalid()
		}
		n, ok := decimal(code)
		if !ok {
			return nil, invalid()
		}
		if n != "0" {
			switch n {
			case "401":
				return nil, failure("credential_rejected", 401)
			case "403":
				return nil, failure("permission_denied", 403)
			default:
				return nil, invalid()
			}
		}
		data, ok := object(body["data"])
		if !ok {
			return nil, invalid()
		}
		item := newItem("account_balance", "sub2api.auth_me", "USD", q)
		item.Remaining, ok = amount(data, "balance", false)
		if !ok || item.Remaining == nil {
			return nil, invalid()
		}
		return []Item{item}, nil
	}
	valid, ok := body["isValid"].(bool)
	if !ok {
		return nil, invalid()
	}
	if !valid {
		return nil, failure("credential_rejected", 401)
	}
	mode, ok := body["mode"].(string)
	if !ok {
		return nil, invalid()
	}
	switch mode {
	case "quota_limited":
		if _, exists := body["balance"]; exists {
			return nil, invalid()
		}
		if _, exists := body["subscription"]; exists {
			return nil, invalid()
		}
		item := newItem("key_quota", "sub2api.usage.quota", "USD", q)
		if value, exists := body["quota"]; exists && value != nil {
			quota, ok := object(value)
			if !ok || quota["unit"] != "USD" || !readAmounts(&item, quota, "remaining", "used", "limit") || item.Limit == nil || item.Remaining == nil || strings.HasPrefix(*item.Remaining, "-") {
				return nil, invalid()
			}
		}
		windows, err := rateWindows(body)
		if err != nil {
			return nil, err
		}
		item.Windows = windows
		if item.Limit == nil && len(windows) == 0 {
			usage, ok := usageItem(body, q)
			if !ok {
				return nil, invalid()
			}
			return []Item{usage}, nil
		}
		return []Item{item}, nil
	case "unrestricted":
		if _, exists := body["quota"]; exists {
			return nil, invalid()
		}
		if _, exists := body["rate_limits"]; exists {
			return nil, invalid()
		}
		if value, exists := body["subscription"]; exists && value != nil {
			if _, exists := body["balance"]; exists {
				return nil, invalid()
			}
			subscription, ok := object(value)
			if !ok || body["unit"] != "USD" {
				return nil, invalid()
			}
			item := newItem("subscription_quota", "sub2api.usage.subscription", "USD", q)
			item.Remaining, ok = amount(body, "remaining", false)
			if !ok {
				return nil, invalid()
			}
			windows, err := subscriptionWindows(subscription)
			if err != nil {
				return nil, err
			}
			item.Windows = windows
			if item.Remaining != nil && *item.Remaining == "-1" {
				for _, w := range windows {
					if w.Limit != nil && positive(*w.Limit) {
						return nil, invalid()
					}
				}
				item.Unlimited = true
				item.Remaining = nil
			} else if item.Remaining != nil && strings.HasPrefix(*item.Remaining, "-") {
				return nil, invalid()
			}
			if item.Remaining == nil && !item.Unlimited && len(windows) == 0 {
				return nil, invalid()
			}
			return []Item{item}, nil
		}
		if value, exists := body["balance"]; exists && value != nil {
			if body["unit"] != "USD" {
				return nil, invalid()
			}
			item := newItem("account_balance", "sub2api.usage.wallet", "USD", q)
			item.Remaining, ok = amount(body, "balance", false)
			if !ok || item.Remaining == nil {
				return nil, invalid()
			}
			// 钱包以 balance 为准，不把 remaining 的特殊值解释成不限额。
			return []Item{item}, nil
		}
		if item, ok := usageItem(body, q); ok {
			return []Item{item}, nil
		}
		return nil, invalid()
	default:
		return nil, invalid()
	}
}
func usageItem(body map[string]any, q Query) (Item, bool) {
	item := newItem("usage_only", "sub2api.usage", "USD", q)
	usage, ok := object(body["usage"])
	if !ok {
		return item, false
	}
	total, ok := object(usage["total"])
	if !ok {
		return item, false
	}
	item.Used, ok = amount(total, "actual_cost", true)
	return item, ok && item.Used != nil
}
func rateWindows(body map[string]any) ([]Window, *QueryError) {
	result := []Window{}
	value, exists := body["rate_limits"]
	if !exists || value == nil {
		return result, nil
	}
	values, ok := value.([]any)
	if !ok || len(values) > 32 {
		return nil, invalid()
	}
	seen := map[string]bool{}
	for _, value := range values {
		entry, ok := object(value)
		if !ok {
			return nil, invalid()
		}
		name, ok := entry["window"].(string)
		if !ok || (name != "5h" && name != "1d" && name != "7d") || seen[name] {
			return nil, invalid()
		}
		seen[name] = true
		window := Window{Name: name, Unit: "USD"}
		window.Limit, ok = amount(entry, "limit", true)
		if !ok || window.Limit == nil {
			return nil, invalid()
		}
		window.Used, ok = amount(entry, "used", true)
		if !ok {
			return nil, invalid()
		}
		window.Remaining, ok = amount(entry, "remaining", true)
		if !ok {
			return nil, invalid()
		}
		window.ResetAt, ok = safeTime(entry["reset_at"])
		if !ok {
			return nil, invalid()
		}
		window.ExpiresAt, ok = safeTime(entry["expires_at"])
		if !ok {
			return nil, invalid()
		}
		result = append(result, window)
	}
	return result, nil
}
func subscriptionWindows(data map[string]any) ([]Window, *QueryError) {
	result := []Window{}
	expires, ok := safeTime(data["expires_at"])
	if !ok {
		return nil, invalid()
	}
	for _, name := range []string{"daily", "weekly", "monthly"} {
		limit, ok := amount(data, name+"_limit_usd", true)
		if !ok {
			return nil, invalid()
		}
		used, ok := amount(data, name+"_usage_usd", true)
		if !ok {
			return nil, invalid()
		}
		if limit == nil && used == nil {
			continue
		}
		window := Window{Name: name, Unit: "USD", Limit: limit, Used: used, ExpiresAt: expires}
		// 仅核验字段提供明确重置时间时展示，不拿 window_start 猜周期终点。
		window.ResetAt, ok = safeTime(data[name+"_reset_at"])
		if !ok {
			return nil, invalid()
		}
		if limit != nil && used != nil && positive(*limit) {
			window.Remaining = ptr(subtractFloorZero(*limit, *used))
		}
		result = append(result, window)
	}
	return result, nil
}
func safeTime(value any) (*string, bool) {
	if value == nil {
		return nil, true
	}
	s, ok := value.(string)
	if !ok || len(s) > 64 {
		return nil, false
	}
	parsed, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return nil, false
	}
	return ptr(parsed.UTC().Format(time.RFC3339Nano)), true
}

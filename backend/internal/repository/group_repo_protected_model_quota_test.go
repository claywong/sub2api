// 私有扩展（不属于 upstream sub2api）。
//
// 文件作用：toRawQuota / fromRawQuota 转换工具的单元测试。
// merge 策略：全新文件，无 upstream 冲突。

package repository

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

func ptrF(v float64) *float64 { return &v }

func TestToRawQuota_Nil_ReturnsEmptyMap(t *testing.T) {
	out := toRawQuota(nil)
	if out == nil {
		t.Fatal("expected non-nil empty map, got nil")
	}
	if len(out) != 0 {
		t.Fatalf("expected empty map, got %v", out)
	}
}

func TestToRawQuota_Value_WrapsUnderSharedKey(t *testing.T) {
	q := &service.ProtectedModelQuota{DailyLimitUSD: ptrF(10), WeeklyLimitUSD: ptrF(50)}
	out := toRawQuota(q)
	if len(out) != 1 {
		t.Fatalf("expected single-entry map, got %v", out)
	}
	inner, ok := out[sharedQuotaKey]
	if !ok {
		t.Fatalf("expected entry under key %q, got %v", sharedQuotaKey, out)
	}
	m, ok := inner.(map[string]interface{})
	if !ok {
		t.Fatalf("expected inner map[string]interface{}, got %T", inner)
	}
	if v, _ := m["daily_limit_usd"].(float64); v != 10 {
		t.Fatalf("expected daily=10, got %v", m["daily_limit_usd"])
	}
	if v, _ := m["weekly_limit_usd"].(float64); v != 50 {
		t.Fatalf("expected weekly=50, got %v", m["weekly_limit_usd"])
	}
}

func TestFromRawQuota_Empty_ReturnsNil(t *testing.T) {
	if q := fromRawQuota(nil); q != nil {
		t.Fatalf("expected nil, got %+v", q)
	}
	if q := fromRawQuota(map[string]interface{}{}); q != nil {
		t.Fatalf("expected nil, got %+v", q)
	}
}

func TestFromRawQuota_SharedKey_ReturnsValue(t *testing.T) {
	raw := map[string]interface{}{
		sharedQuotaKey: map[string]interface{}{
			"daily_limit_usd":  10.0,
			"weekly_limit_usd": 50.0,
		},
	}
	q := fromRawQuota(raw)
	if q == nil {
		t.Fatal("expected non-nil quota")
	}
	if q.DailyLimitUSD == nil || *q.DailyLimitUSD != 10 {
		t.Fatalf("expected daily=10, got %+v", q.DailyLimitUSD)
	}
	if q.WeeklyLimitUSD == nil || *q.WeeklyLimitUSD != 50 {
		t.Fatalf("expected weekly=50, got %+v", q.WeeklyLimitUSD)
	}
}

// TestFromRawQuota_LegacyKey_FallsBack 兼容老数据：旧版按 model name 作为 key，
// 改造后没有 sharedQuotaKey，需要取首个条目作为共享额度（避免丢配置）。
func TestFromRawQuota_LegacyKey_FallsBack(t *testing.T) {
	raw := map[string]interface{}{
		"claude-opus-4.7": map[string]interface{}{
			"daily_limit_usd": 8.5,
		},
	}
	q := fromRawQuota(raw)
	if q == nil {
		t.Fatal("expected non-nil quota for legacy data")
	}
	if q.DailyLimitUSD == nil || *q.DailyLimitUSD != 8.5 {
		t.Fatalf("expected daily=8.5, got %+v", q.DailyLimitUSD)
	}
	if q.WeeklyLimitUSD != nil {
		t.Fatalf("expected weekly=nil, got %v", *q.WeeklyLimitUSD)
	}
}

func TestRoundTrip_Preserves(t *testing.T) {
	orig := &service.ProtectedModelQuota{DailyLimitUSD: ptrF(3.14), WeeklyLimitUSD: ptrF(15)}
	back := fromRawQuota(toRawQuota(orig))
	if back == nil {
		t.Fatal("expected non-nil after round-trip")
	}
	if back.DailyLimitUSD == nil || *back.DailyLimitUSD != 3.14 {
		t.Fatalf("expected daily=3.14, got %+v", back.DailyLimitUSD)
	}
	if back.WeeklyLimitUSD == nil || *back.WeeklyLimitUSD != 15 {
		t.Fatalf("expected weekly=15, got %+v", back.WeeklyLimitUSD)
	}
}

package service

import (
	"math"
	"testing"
)

// 私有扩展单测（不属于 upstream sub2api）
// 验证粘性会话抢并发槽位时的「计数但不限流」哨兵换算逻辑。
// @author wangzhong

func TestStickySlotConcurrency(t *testing.T) {
	cases := []struct {
		name string
		in   int
		want int
	}{
		{"limited_account", 5, stickyBypassConcurrency},
		{"limited_account_one", 1, stickyBypassConcurrency},
		{"unlimited_zero", 0, 0},
		{"unlimited_negative", -1, -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := stickySlotConcurrency(tc.in); got != tc.want {
				t.Fatalf("stickySlotConcurrency(%d) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}

	// 哨兵值应足够大以等同无上限，且远离 int32 溢出边界
	if stickyBypassConcurrency <= 0 || stickyBypassConcurrency >= math.MaxInt32 {
		t.Fatalf("stickyBypassConcurrency=%d 不在合理范围 (0, MaxInt32)", stickyBypassConcurrency)
	}
}

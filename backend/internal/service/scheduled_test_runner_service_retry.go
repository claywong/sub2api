// 私有扩展（不属于 upstream sub2api）
// 功能：定时测试失败后的指数退避补测逻辑
// 包含方法：launchRetryIfNeeded, runRetryTest, retryDelay
// merge 策略：companion 文件，不修改 upstream 方法签名

package service

import (
	"context"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

// retryMaxAttempts 补测最大次数（不含原始测试）。
const retryMaxAttempts = 10

// retryDelay 返回第 attempt 次补测前的等待时长（0-indexed）。
// 序列：5s, 15s, 30s, 30s, ...（最多 retryMaxAttempts 次）
func retryDelay(attempt int) time.Duration {
	switch attempt {
	case 0:
		return 5 * time.Second
	case 1:
		return 15 * time.Second
	default:
		return 30 * time.Second
	}
}

// planRetryState 记录单个 plan 的补测进度，仅存于 runner 内存，不写入 AccountTestHealth。
type planRetryState struct {
	attempt int
}

// retryStateMap 管理所有 plan 的补测状态，线程安全。
type retryStateMap struct {
	mu     sync.Mutex
	states map[int64]*planRetryState
}

func newRetryStateMap() *retryStateMap {
	return &retryStateMap{states: make(map[int64]*planRetryState)}
}

func (m *retryStateMap) clear(planID int64) {
	m.mu.Lock()
	delete(m.states, planID)
	m.mu.Unlock()
}

// nextAttempt 返回下一次补测的 attempt 编号（0-indexed）；已达上限时返回 -1。
func (m *retryStateMap) nextAttempt(planID int64) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	st, ok := m.states[planID]
	if !ok {
		st = &planRetryState{}
		m.states[planID] = st
	}
	if st.attempt >= retryMaxAttempts {
		return -1
	}
	n := st.attempt
	st.attempt++
	return n
}

// launchRetryIfNeeded 在测试失败后安排下一次补测。
// 成功时清除状态；失败时按退避序列安排，直到 retryMaxAttempts 次或恢复健康。
// 状态仅存于 runner 内存，不写入 AccountTestHealth，避免旧版死锁问题。
func (s *ScheduledTestRunnerService) launchRetryIfNeeded(plan *ScheduledTestPlan, result *ScheduledTestResult) {
	if result.Status == "success" {
		s.retryStates.clear(plan.ID)
		return
	}
	attempt := s.retryStates.nextAttempt(plan.ID)
	if attempt < 0 {
		logger.LegacyPrintf("service.scheduled_test_runner",
			"[RetryTest] plan=%d account=%d reached max retries (%d), giving up until next scheduled run",
			plan.ID, plan.AccountID, retryMaxAttempts)
		s.retryStates.clear(plan.ID)
		return
	}
	go s.runRetryTest(plan, attempt)
}

// runRetryTest 等待退避时长后执行一次补测，结果写入滑动窗口以积累 MinSamples。
func (s *ScheduledTestRunnerService) runRetryTest(plan *ScheduledTestPlan, attempt int) {
	time.Sleep(retryDelay(attempt))

	retryTimeout := 10 * time.Second
	if s.cfg != nil && s.cfg.Gateway.ResponseHeaderTimeout > 0 {
		retryTimeout = time.Duration(s.cfg.Gateway.ResponseHeaderTimeout) * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), retryTimeout)
	defer cancel()

	result, err := s.accountTestSvc.RunTestBackground(ctx, plan.AccountID, plan.ModelID)
	if err != nil {
		logger.LegacyPrintf("service.scheduled_test_runner",
			"[RetryTest] plan=%d attempt=%d RunTestBackground error: %v", plan.ID, attempt+1, err)
		return
	}

	if err := s.scheduledSvc.SaveResult(ctx, plan.ID, plan.MaxResults, result); err != nil {
		logger.LegacyPrintf("service.scheduled_test_runner",
			"[RetryTest] plan=%d attempt=%d SaveResult error: %v", plan.ID, attempt+1, err)
	}

	if result.Status == "success" {
		logger.LegacyPrintf("service.scheduled_test_runner",
			"[RetryTest] plan=%d account=%d account_name=%s attempt=%d recovered",
			plan.ID, plan.AccountID, plan.AccountName, attempt+1)
	} else {
		logger.LegacyPrintf("service.scheduled_test_runner",
			"[WARN] [RetryTest] plan=%d account=%d account_name=%s attempt=%d failed: %s",
			plan.ID, plan.AccountID, plan.AccountName, attempt+1, result.ErrorMessage)
	}

	if s.healthCache != nil {
		s.healthCache.UpdateFromTest(plan.AccountID, result)
	}

	s.launchRetryIfNeeded(plan, result)
}

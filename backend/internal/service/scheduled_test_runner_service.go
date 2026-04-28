package service

import (
	"context"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/robfig/cron/v3"
)

const scheduledTestDefaultMaxWorkers = 10

// ScheduledTestRunnerService periodically scans due test plans and executes them.
type ScheduledTestRunnerService struct {
	planRepo       ScheduledTestPlanRepository
	scheduledSvc   *ScheduledTestService
	accountTestSvc *AccountTestService
	rateLimitSvc   *RateLimitService
	healthCache    *AccountTestHealthCache
	cfg            *config.Config

	cron      *cron.Cron
	startOnce sync.Once
	stopOnce  sync.Once
}

// NewScheduledTestRunnerService creates a new runner.
func NewScheduledTestRunnerService(
	planRepo ScheduledTestPlanRepository,
	scheduledSvc *ScheduledTestService,
	accountTestSvc *AccountTestService,
	rateLimitSvc *RateLimitService,
	cfg *config.Config,
	healthCache *AccountTestHealthCache,
) *ScheduledTestRunnerService {
	return &ScheduledTestRunnerService{
		planRepo:       planRepo,
		scheduledSvc:   scheduledSvc,
		accountTestSvc: accountTestSvc,
		rateLimitSvc:   rateLimitSvc,
		healthCache:    healthCache,
		cfg:            cfg,
	}
}

// Start begins the cron ticker (every minute).
func (s *ScheduledTestRunnerService) Start() {
	if s == nil {
		return
	}
	s.startOnce.Do(func() {
		loc := time.Local
		if s.cfg != nil {
			if parsed, err := time.LoadLocation(s.cfg.Timezone); err == nil && parsed != nil {
				loc = parsed
			}
		}

		c := cron.New(cron.WithParser(scheduledTestCronParser), cron.WithLocation(loc))
		_, err := c.AddFunc("* * * * *", func() { s.runScheduled() })
		if err != nil {
			logger.LegacyPrintf("service.scheduled_test_runner", "[ScheduledTestRunner] not started (invalid schedule): %v", err)
			return
		}
		s.cron = c
		s.cron.Start()
		logger.LegacyPrintf("service.scheduled_test_runner", "[ScheduledTestRunner] started (tick=every minute)")
	})
}

// Stop gracefully shuts down the cron scheduler.
func (s *ScheduledTestRunnerService) Stop() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() {
		if s.cron != nil {
			ctx := s.cron.Stop()
			select {
			case <-ctx.Done():
			case <-time.After(3 * time.Second):
				logger.LegacyPrintf("service.scheduled_test_runner", "[ScheduledTestRunner] cron stop timed out")
			}
		}
	})
}

func (s *ScheduledTestRunnerService) runScheduled() {
	// Delay 10s so execution lands at ~:10 of each minute instead of :00.
	time.Sleep(10 * time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	now := time.Now()
	plans, err := s.planRepo.ListDue(ctx, now)
	if err != nil {
		logger.LegacyPrintf("service.scheduled_test_runner", "[ScheduledTestRunner] ListDue error: %v", err)
		return
	}
	if len(plans) == 0 {
		return
	}

	logger.LegacyPrintf("service.scheduled_test_runner", "[ScheduledTestRunner] found %d due plans", len(plans))

	sem := make(chan struct{}, scheduledTestDefaultMaxWorkers)
	var wg sync.WaitGroup

	for _, plan := range plans {
		sem <- struct{}{}
		wg.Add(1)
		go func(p *ScheduledTestPlan) {
			defer wg.Done()
			defer func() { <-sem }()
			s.runOnePlan(ctx, p)
		}(plan)
	}

	wg.Wait()
}

func (s *ScheduledTestRunnerService) runOnePlan(ctx context.Context, plan *ScheduledTestPlan) {
	result, err := s.accountTestSvc.RunTestBackground(ctx, plan.AccountID, plan.ModelID)
	if err != nil {
		logger.LegacyPrintf("service.scheduled_test_runner", "[ScheduledTestRunner] plan=%d RunTestBackground error: %v", plan.ID, err)
		return
	}

	if err := s.scheduledSvc.SaveResult(ctx, plan.ID, plan.MaxResults, result); err != nil {
		logger.LegacyPrintf("service.scheduled_test_runner", "[ScheduledTestRunner] plan=%d SaveResult error: %v", plan.ID, err)
	}

	// 更新健康缓存，并根据健康状态决定是否补测或隔离
	if s.healthCache != nil {
		s.healthCache.UpdateFromTest(plan.AccountID, result)
		s.launchRetryIfNeeded(plan)
	}

	// Auto-recover account if test succeeded and auto_recover is enabled.
	if result.Status == "success" && plan.AutoRecover {
		s.tryRecoverAccount(ctx, plan.AccountID, plan.ID)
	}

	nextRun, err := computeNextRun(plan.CronExpression, time.Now())
	if err != nil {
		logger.LegacyPrintf("service.scheduled_test_runner", "[ScheduledTestRunner] plan=%d computeNextRun error: %v", plan.ID, err)
		return
	}

	if err := s.planRepo.UpdateAfterRun(ctx, plan.ID, time.Now(), nextRun); err != nil {
		logger.LegacyPrintf("service.scheduled_test_runner", "[ScheduledTestRunner] plan=%d UpdateAfterRun error: %v", plan.ID, err)
	}
}

// launchRetryIfNeeded 根据当前 ConsecFails 决定是否启动补测 goroutine 或触发 TempUnschedulable。
func (s *ScheduledTestRunnerService) launchRetryIfNeeded(plan *ScheduledTestPlan) {
	h := s.healthCache.GetOrCreate(plan.AccountID)
	h.mu.Lock()
	consecFails := h.ConsecFails
	retryInterval := h.RetryInterval
	h.mu.Unlock()

	cfg := s.healthCache.Cfg()
	switch {
	case consecFails == 1:
		go s.runRetryTest(plan, 0)
	case consecFails >= 2 && consecFails < cfg.tempUnschedThreshold:
		go s.runRetryTest(plan, retryInterval)
	case consecFails >= cfg.tempUnschedThreshold:
		s.triggerTempUnschedForScheduledTest(plan.AccountID)
	}
}

// runRetryTest 在独立 goroutine 中执行一次补测；delay>0 时先等待。
// 补测不修改 nextRun，不影响原 cron 节奏。
func (s *ScheduledTestRunnerService) runRetryTest(plan *ScheduledTestPlan, delay time.Duration) {
	if delay > 0 {
		time.Sleep(delay)
	}

	retryTimeout := 10 * time.Second
	if s.cfg != nil && s.cfg.Gateway.ResponseHeaderTimeout > 0 {
		retryTimeout = time.Duration(s.cfg.Gateway.ResponseHeaderTimeout) * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), retryTimeout)
	defer cancel()

	result, err := s.accountTestSvc.RunTestBackground(ctx, plan.AccountID, plan.ModelID)
	if err != nil {
		logger.LegacyPrintf("service.scheduled_test_runner", "[ScheduledTestRunner] retry plan=%d RunTestBackground error: %v", plan.ID, err)
		return
	}

	if err := s.scheduledSvc.SaveResult(ctx, plan.ID, plan.MaxResults, result); err != nil {
		logger.LegacyPrintf("service.scheduled_test_runner", "[ScheduledTestRunner] retry plan=%d SaveResult error: %v", plan.ID, err)
	}

	s.healthCache.UpdateFromTest(plan.AccountID, result)
	s.launchRetryIfNeeded(plan)
}

// triggerTempUnschedForScheduledTest 计算退避时长并持久化 TempUnschedulable。
func (s *ScheduledTestRunnerService) triggerTempUnschedForScheduledTest(accountID int64) {
	h := s.healthCache.GetOrCreate(accountID)
	cfg := s.healthCache.Cfg()
	h.mu.Lock()
	if h.TempUnschedDuration == 0 {
		h.TempUnschedDuration = cfg.tempUnschedInit
	} else {
		h.TempUnschedDuration *= 2
		if h.TempUnschedDuration > cfg.tempUnschedMax {
			h.TempUnschedDuration = cfg.tempUnschedMax
		}
	}
	duration := h.TempUnschedDuration
	h.mu.Unlock()

	until := time.Now().Add(duration)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second) // DB 写操作，固定 10s
	defer cancel()

	if s.rateLimitSvc == nil {
		return
	}
	if err := s.rateLimitSvc.SetTempUnschedulableForScheduledTest(ctx, accountID, until); err != nil {
		logger.LegacyPrintf("service.scheduled_test_runner", "[ScheduledTestRunner] SetTempUnschedulable account=%d error: %v", accountID, err)
	} else {
		logger.LegacyPrintf("service.scheduled_test_runner", "[ScheduledTestRunner] account=%d temp-unschedulable until=%s (duration=%s)", accountID, until.Format(time.RFC3339), duration)
	}
}

// tryRecoverAccount attempts to recover an account from recoverable runtime state.
func (s *ScheduledTestRunnerService) tryRecoverAccount(ctx context.Context, accountID int64, planID int64) {
	if s.rateLimitSvc == nil {
		return
	}

	recovery, err := s.rateLimitSvc.RecoverAccountAfterSuccessfulTest(ctx, accountID)
	if err != nil {
		logger.LegacyPrintf("service.scheduled_test_runner", "[ScheduledTestRunner] plan=%d auto-recover failed: %v", planID, err)
		return
	}
	if recovery == nil {
		return
	}

	if recovery.ClearedError {
		logger.LegacyPrintf("service.scheduled_test_runner", "[ScheduledTestRunner] plan=%d auto-recover: account=%d recovered from error status", planID, accountID)
	}
	if recovery.ClearedRateLimit {
		logger.LegacyPrintf("service.scheduled_test_runner", "[ScheduledTestRunner] plan=%d auto-recover: account=%d cleared rate-limit/runtime state", planID, accountID)
	}
}

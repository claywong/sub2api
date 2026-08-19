// 私有扩展（不属于 upstream sub2api）。
//
// 本文件：「飞书离职自动禁用」主服务——cron 调度、leader lock、编排。
// 所含内容：FeishuOffboardService 及其 Start/Stop/Reload/RunOnce 等方法。
// merge 策略：纯新增文件，与 upstream 无交集，merge 时保留即可。
//
// 调度骨架（cron 动态重建 + Redis leader lock + DB advisory lock 兜底 + 心跳）
// 沿用 ops_cleanup_service.go 的成熟做法，不另造一套。
//
// 判定在 feishu_offboard_decide.go，禁用在 feishu_offboard_execute.go，
// 本文件只做编排：取用户 → 判定 → 熔断 → 执行 → 落库 → 通知。
//
// @author wangzhong
package service

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/robfig/cron/v3"
)

const (
	feishuOffboardLeaderLockKey = "feishu:offboard:leader"
	feishuOffboardLeaderLockTTL = 30 * time.Minute
	// 全量核查要跑数百次飞书调用，给足超时；超过说明飞书侧异常，该失败。
	feishuOffboardRunTimeout       = 30 * time.Minute
	feishuOffboardHeartbeatTimeout = 10 * time.Second
	// 拉活跃用户的分页大小。列表接口会 join 订阅和分组，页开太大容易超时。
	feishuOffboardUserPageSize = 200
)

// cron parser 复用 feishu_offboard_config.go 里已声明的 feishuOffboardCronParser，
// 保证「校验配置时接受的表达式」与「调度时实际解析的表达式」是同一套语法，
// 否则会出现配置页校验通过但 cron 起不来的情况。

var feishuOffboardReleaseScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
  return redis.call("DEL", KEYS[1])
end
return 0
`)

// FeishuOffboardNotifier 发送执行结果通知。
type FeishuOffboardNotifier interface {
	NotifyOffboardResult(ctx context.Context, run *FeishuOffboardRun, to []string)
}

// FeishuOffboardService 每天按 cron 核查飞书在职状态并禁用已离职账号。
type FeishuOffboardService struct {
	configStore *FeishuOffboardConfigStore
	runRepo     FeishuOffboardRepository
	userRepo    UserRepository
	opsRepo     OpsRepository
	notifier    FeishuOffboardNotifier
	invalidator APIKeyAuthCacheInvalidator

	db          *sql.DB
	redisClient *redis.Client
	cfg         *config.Config

	instanceID string

	// clientFactory 便于单测注入假客户端；生产用真实 SDK。
	clientFactory func(appID, appSecret string) (FeishuContactClient, error)

	// mu 守护 cron 实例切换。不用 sync.Once 是因为 Reload 需要
	// "停旧 cron 再起新 cron"，而 Once 一旦触发就无法复用。
	mu      sync.Mutex
	cron    *cron.Cron
	started bool
	stopped bool

	// runMu 保证同一进程内不会有两次执行重叠（cron 与手动触发并发时）。
	// leader lock 管的是多实例，这个管的是单实例内部。
	runMu sync.Mutex

	warnNoRedisOnce sync.Once
}

func NewFeishuOffboardService(
	configStore *FeishuOffboardConfigStore,
	runRepo FeishuOffboardRepository,
	userRepo UserRepository,
	opsRepo OpsRepository,
	notifier FeishuOffboardNotifier,
	invalidator APIKeyAuthCacheInvalidator,
	db *sql.DB,
	redisClient *redis.Client,
	cfg *config.Config,
) *FeishuOffboardService {
	return &FeishuOffboardService{
		configStore:   configStore,
		runRepo:       runRepo,
		userRepo:      userRepo,
		opsRepo:       opsRepo,
		notifier:      notifier,
		invalidator:   invalidator,
		db:            db,
		redisClient:   redisClient,
		cfg:           cfg,
		instanceID:    uuid.NewString(),
		clientFactory: NewFeishuContactClient,
	}
}

// Start 首次启动 cron。未配置或未启用时静默不跑。重复调用幂等。
func (s *FeishuOffboardService) Start() {
	if s == nil || s.configStore == nil || s.userRepo == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started || s.stopped {
		return
	}
	s.started = true
	if err := s.applyScheduleLocked(context.Background()); err != nil {
		logger.LegacyPrintf("service.feishu_offboard",
			"[FeishuOffboard] not started: %v", err)
	}
}

// Stop 关闭 cron。幂等。
func (s *FeishuOffboardService) Stop() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return
	}
	s.stopped = true
	s.stopCronLocked()
}

// Reload 在管理员改动配置后重建 cron，使 enabled / schedule 立即生效。
func (s *FeishuOffboardService) Reload(ctx context.Context) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started || s.stopped {
		return nil
	}
	return s.applyScheduleLocked(ctx)
}

func (s *FeishuOffboardService) stopCronLocked() {
	if s.cron == nil {
		return
	}
	ctx := s.cron.Stop()
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
	}
	s.cron = nil
}

func (s *FeishuOffboardService) applyScheduleLocked(ctx context.Context) error {
	s.stopCronLocked()

	cfg, err := s.configStore.LoadConfig(ctx)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if !cfg.Enabled {
		logger.LegacyPrintf("service.feishu_offboard",
			"[FeishuOffboard] disabled by settings")
		return nil
	}

	schedule := strings.TrimSpace(cfg.Schedule)
	if schedule == "" {
		schedule = FeishuOffboardDefaultSchedule
	}

	loc := time.Local
	if s.cfg != nil && strings.TrimSpace(s.cfg.Timezone) != "" {
		if parsed, err := time.LoadLocation(strings.TrimSpace(s.cfg.Timezone)); err == nil && parsed != nil {
			loc = parsed
		}
	}

	c := cron.New(cron.WithParser(feishuOffboardCronParser), cron.WithLocation(loc))
	if _, err := c.AddFunc(schedule, func() { s.runScheduled() }); err != nil {
		return fmt.Errorf("invalid schedule %q: %w", schedule, err)
	}
	c.Start()
	s.cron = c
	logger.LegacyPrintf("service.feishu_offboard",
		"[FeishuOffboard] scheduled (schedule=%q tz=%s dry_run=%v threshold=%d)",
		schedule, loc.String(), cfg.DryRun, cfg.Threshold)
	return nil
}

func (s *FeishuOffboardService) runScheduled() {
	ctx, cancel := context.WithTimeout(context.Background(), feishuOffboardRunTimeout)
	defer cancel()

	release, ok := s.tryAcquireLeaderLock(ctx)
	if !ok {
		// 别的实例在跑，正常退出。
		return
	}
	if release != nil {
		defer release()
	}

	run, err := s.execute(ctx, OffboardTriggerCron, nil)
	if err != nil {
		logger.LegacyPrintf("service.feishu_offboard",
			"[FeishuOffboard] scheduled run failed: %v", err)
		return
	}
	logger.LegacyPrintf("service.feishu_offboard",
		"[FeishuOffboard] run done: checked=%d resigned=%d disabled=%d unverifiable=%d broken=%v",
		run.CheckedCount, run.ResignedCount, run.DisabledCount,
		run.UnverifiableCount, run.CircuitBroken)
}

// TriggerManual 由管理员在页面手动触发一次。
//
// dryRun 用指针而非 bool：nil 表示「沿用配置里的 dry_run」，非 nil 才覆盖。
// 这个区别很重要——如果用 bool，调用方不传时会得到零值 false，
// 等于把管理员配置的空跑模式静默改成真执行，凌晨的安全设置在手动触发时失效。
// 想强制空跑传 &true，想强制真跑传 &false。
//
// 手动触发同样受熔断保护。
func (s *FeishuOffboardService) TriggerManual(
	ctx context.Context, dryRun *bool,
) (*FeishuOffboardRun, error) {
	if s == nil {
		return nil, fmt.Errorf("service not initialized")
	}
	return s.execute(ctx, OffboardTriggerManual, dryRun)
}

// execute 跑完整流程并落库。
func (s *FeishuOffboardService) execute(
	ctx context.Context, trigger string, dryRunOverride *bool,
) (*FeishuOffboardRun, error) {
	// 单实例内串行：cron 与手动触发同时发生时，后者等前者跑完，
	// 避免两轮判定交叉导致重复禁用或统计错乱。
	s.runMu.Lock()
	defer s.runMu.Unlock()

	startedAt := time.Now()
	run := &FeishuOffboardRun{
		RunAt:         startedAt,
		TriggerSource: trigger,
	}

	cfg, err := s.configStore.LoadConfig(ctx)
	if err != nil {
		return s.finishWithError(ctx, run, startedAt, fmt.Errorf("load config: %w", err))
	}
	dryRun := cfg.DryRun
	if dryRunOverride != nil {
		dryRun = *dryRunOverride
	}
	run.DryRun = dryRun

	client, err := s.buildClient(cfg)
	if err != nil {
		return s.finishWithError(ctx, run, startedAt, err)
	}

	users, err := s.listActiveUsers(ctx)
	if err != nil {
		return s.finishWithError(ctx, run, startedAt,
			fmt.Errorf("list active users: %w", err))
	}
	run.CheckedCount = len(users)

	decider := &offboardDecider{client: client}
	decisions, err := decider.DecideOffboard(ctx, users)
	if err != nil {
		return s.finishWithError(ctx, run, startedAt, err)
	}

	resigned, unverifiable, skipped, _ := SummarizeDecisions(decisions)
	run.ResignedCount = resigned
	run.UnverifiableCount = unverifiable
	run.SkippedCount = skipped

	// 熔断在执行之前判断：宁可漏一天，也不要批量误禁。
	breaker := checkCircuitBreaker(decisions, cfg.Threshold)
	run.CircuitBroken = breaker.Broken
	if breaker.Broken {
		run.ErrorMessage = fmt.Sprintf(
			"命中 %d 人超过熔断阈值 %d，已阻止自动禁用，请人工核对后手动处理",
			breaker.HitCount, breaker.Threshold)
		logger.LegacyPrintf("service.feishu_offboard",
			"[FeishuOffboard] circuit breaker tripped: %s", run.ErrorMessage)
	} else {
		executor := &offboardExecutor{
			userRepo:             s.userRepo,
			authCacheInvalidator: s.invalidator,
		}
		run.DisabledCount = executor.applyDecisions(ctx, decisions, dryRun)
	}

	run.Decisions = decisions
	run.DurationMs = time.Since(startedAt).Milliseconds()
	s.persist(ctx, run)
	s.recordHeartbeat(run, startedAt, nil)
	s.notify(ctx, run, cfg)
	return run, nil
}

// buildClient 构造飞书客户端。凭证缺失时给出明确错误——
// 开了开关却没凭证会让任务每天静默失败，不如直接说清楚。
func (s *FeishuOffboardService) buildClient(
	cfg FeishuOffboardConfig,
) (FeishuContactClient, error) {
	factory := s.clientFactory
	if factory == nil {
		factory = NewFeishuContactClient
	}
	client, err := factory(cfg.AppID, cfg.AppSecret)
	if err != nil {
		return nil, fmt.Errorf("飞书凭证未配置或无效：%w", err)
	}
	return client, nil
}

// listActiveUsers 拉取全部 status=active 的用户。
//
// 只查活跃用户：已禁用的账号无需再判定，能显著缩小飞书调用量。
// 不加载订阅（IncludeSubscriptions=false），列表接口 join 订阅很慢，
// 而判定只需要 id/email/username/role。
func (s *FeishuOffboardService) listActiveUsers(ctx context.Context) ([]User, error) {
	includeSubs := false
	out := make([]User, 0, 256)
	page := 1
	for {
		users, result, err := s.userRepo.ListWithFilters(ctx,
			pagination.PaginationParams{Page: page, PageSize: feishuOffboardUserPageSize},
			UserListFilters{
				Status:               StatusActive,
				IncludeSubscriptions: &includeSubs,
			})
		if err != nil {
			return nil, err
		}
		out = append(out, users...)
		if result == nil || page >= result.Pages || len(users) == 0 {
			break
		}
		page++
	}
	return out, nil
}

func (s *FeishuOffboardService) finishWithError(
	ctx context.Context, run *FeishuOffboardRun, startedAt time.Time, err error,
) (*FeishuOffboardRun, error) {
	run.ErrorMessage = err.Error()
	run.DurationMs = time.Since(startedAt).Milliseconds()
	s.persist(ctx, run)
	s.recordHeartbeat(run, startedAt, err)
	return run, err
}

// persist 落库执行记录。落库失败不应让整次执行算失败——
// 禁用动作已经发生了，日志里要留下痕迹。
func (s *FeishuOffboardService) persist(ctx context.Context, run *FeishuOffboardRun) {
	if s.runRepo == nil {
		return
	}
	if err := s.runRepo.Insert(ctx, run); err != nil {
		logger.LegacyPrintf("service.feishu_offboard",
			"[FeishuOffboard] persist run record failed: %v", err)
	}
}

func (s *FeishuOffboardService) notify(
	ctx context.Context, run *FeishuOffboardRun, cfg FeishuOffboardConfig,
) {
	if s.notifier == nil {
		return
	}
	// 无事发生就不打扰：没命中、没熔断、没出错时不发信，
	// 否则每天一封空报告会让人很快开始忽略它。
	if run.ResignedCount == 0 && !run.CircuitBroken && run.ErrorMessage == "" {
		return
	}
	s.notifier.NotifyOffboardResult(ctx, run, cfg.NotifyTo)
}

func (s *FeishuOffboardService) recordHeartbeat(
	run *FeishuOffboardRun, startedAt time.Time, runErr error,
) {
	if s.opsRepo == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), feishuOffboardHeartbeatTimeout)
	defer cancel()

	runAt := startedAt.UTC()
	durMs := time.Since(startedAt).Milliseconds()
	input := &OpsUpsertJobHeartbeatInput{
		JobName:        FeishuOffboardJobName,
		LastRunAt:      &runAt,
		LastDurationMs: &durMs,
	}
	if runErr != nil {
		msg := truncateString(runErr.Error(), 2048)
		input.LastError = &msg
	} else {
		now := time.Now().UTC()
		result := truncateString(fmt.Sprintf(
			"checked=%d resigned=%d disabled=%d unverifiable=%d skipped=%d broken=%v dry_run=%v",
			run.CheckedCount, run.ResignedCount, run.DisabledCount,
			run.UnverifiableCount, run.SkippedCount, run.CircuitBroken, run.DryRun), 2048)
		input.LastSuccessAt = &now
		input.LastResult = &result
	}
	_ = s.opsRepo.UpsertJobHeartbeat(ctx, input)
}

func (s *FeishuOffboardService) tryAcquireLeaderLock(ctx context.Context) (func(), bool) {
	if s == nil {
		return nil, false
	}
	if s.cfg != nil && s.cfg.RunMode == config.RunModeSimple {
		return nil, true
	}

	if s.redisClient != nil {
		ok, err := s.redisClient.SetNX(ctx, feishuOffboardLeaderLockKey,
			s.instanceID, feishuOffboardLeaderLockTTL).Result()
		if err == nil {
			if !ok {
				return nil, false
			}
			return func() {
				_, _ = feishuOffboardReleaseScript.Run(ctx, s.redisClient,
					[]string{feishuOffboardLeaderLockKey}, s.instanceID).Result()
			}, true
		}
		s.warnNoRedisOnce.Do(func() {
			logger.LegacyPrintf("service.feishu_offboard",
				"[FeishuOffboard] leader lock SetNX failed; falling back to DB advisory lock: %v", err)
		})
	} else {
		s.warnNoRedisOnce.Do(func() {
			logger.LegacyPrintf("service.feishu_offboard",
				"[FeishuOffboard] redis not configured; using DB advisory lock")
		})
	}

	if s.db == nil {
		return nil, false
	}
	return tryAcquireDBAdvisoryLock(ctx, s.db,
		hashAdvisoryLockID(feishuOffboardLeaderLockKey))
}

// ── handler 依赖的读接口 ───────────────────────────────────────────────

func (s *FeishuOffboardService) LoadConfigView(
	ctx context.Context,
) (FeishuOffboardConfigView, error) {
	return s.configStore.LoadView(ctx)
}

// SaveConfig 保存配置并立即重建 cron，让 enabled / schedule 改动当场生效。
func (s *FeishuOffboardService) SaveConfig(
	ctx context.Context, input FeishuOffboardConfigInput,
) error {
	if err := s.configStore.SaveConfig(ctx, input); err != nil {
		return err
	}
	if err := s.Reload(ctx); err != nil {
		logger.LegacyPrintf("service.feishu_offboard",
			"[FeishuOffboard] reload after save failed: %v", err)
	}
	return nil
}

// TestConnection 用当前配置试调一次飞书，校验凭证可用。
//
// 用 batch_get_id 传一个不存在的邮箱来试：能正常返回说明 token 拿到了、
// 权限也够，而查一个不存在的人不会碰到任何真实用户数据。
func (s *FeishuOffboardService) TestConnection(ctx context.Context) error {
	cfg, err := s.configStore.LoadConfig(ctx)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	client, err := s.buildClient(cfg)
	if err != nil {
		return err
	}
	if _, err := client.BatchGetUsersByEmails(ctx,
		[]string{"connection-probe-not-exist@invalid.local"}); err != nil {
		return fmt.Errorf("飞书接口调用失败：%w", err)
	}
	return nil
}

func (s *FeishuOffboardService) ListRuns(
	ctx context.Context, filter FeishuOffboardRunListFilter,
) (*FeishuOffboardRunList, error) {
	if s.runRepo == nil {
		return &FeishuOffboardRunList{Items: []FeishuOffboardRun{}}, nil
	}
	return s.runRepo.List(ctx, filter)
}

func (s *FeishuOffboardService) GetRun(
	ctx context.Context, id int64,
) (*FeishuOffboardRun, error) {
	if s.runRepo == nil {
		return nil, fmt.Errorf("run repository not configured")
	}
	return s.runRepo.GetByID(ctx, id)
}

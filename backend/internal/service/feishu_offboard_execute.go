// 私有扩展（不属于 upstream sub2api）。
//
// 本文件：「飞书离职自动禁用」的执行层——熔断判断与禁用动作。
// 所含内容：applyDecisions、disableOne、checkCircuitBreaker。
// merge 策略：纯新增文件，与 upstream 无交集，merge 时保留即可。
//
// 判定在 feishu_offboard_decide.go（只读），本文件负责唯一的写动作。
// 分开是为了让"会禁用账号的代码"集中在一处、便于审查，
// 而判定逻辑可以被穷举单测而不触碰任何数据。
//
// @author wangzhong
package service

import (
	"context"
	"fmt"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

// offboardExecutor 执行禁用所需的依赖。
type offboardExecutor struct {
	userRepo             UserRepository
	authCacheInvalidator APIKeyAuthCacheInvalidator
}

// circuitBreakerResult 熔断判断结果。
type circuitBreakerResult struct {
	Broken    bool
	HitCount  int
	Threshold int
}

// checkCircuitBreaker 判断本次命中数是否超过阈值。
//
// 熔断存在的理由：正常情况下一天离职不会有很多人。一次命中十几个以上，
// 更可能是飞书数据异常、接口语义变化或判定逻辑有 bug，而不是真的集体离职。
// 这种时候宁可漏一天（第二天人工确认后再跑），也不要在凌晨批量误禁在职员工——
// sub2api 侧没有操作审计可以一键回滚，误禁会直接打断对方工作。
func checkCircuitBreaker(decisions []OffboardDecision, threshold int) circuitBreakerResult {
	if threshold <= 0 {
		threshold = FeishuOffboardDefaultThreshold
	}
	hit := 0
	for _, d := range decisions {
		if d.Verdict == OffboardVerdictResigned {
			hit++
		}
	}
	return circuitBreakerResult{
		Broken:    hit > threshold,
		HitCount:  hit,
		Threshold: threshold,
	}
}

// applyDecisions 对判定为离职的用户执行禁用，返回成功禁用的人数。
//
// dryRun 为 true 时只标记不写库，用于上线前核对判定结果。
// decisions 会被原地更新（写入 Disabled / DisableError），以便完整落库追溯。
func (e *offboardExecutor) applyDecisions(
	ctx context.Context, decisions []OffboardDecision, dryRun bool,
) int {
	if e == nil || e.userRepo == nil {
		return 0
	}
	disabled := 0
	for i := range decisions {
		d := &decisions[i]
		if d.Verdict != OffboardVerdictResigned {
			continue
		}
		if dryRun {
			// 空跑：明确不写库。Disabled 保持 false，报告里能看出"本来会禁谁"。
			continue
		}
		if err := e.disableOne(ctx, d.UserID); err != nil {
			d.DisableError = err.Error()
			logger.LegacyPrintf("service.feishu_offboard",
				"[FeishuOffboard] disable user %d failed: %v", d.UserID, err)
			continue
		}
		d.Disabled = true
		disabled++
		logger.LegacyPrintf("service.feishu_offboard",
			"[FeishuOffboard] disabled user %d (%s) reason=%s",
			d.UserID, d.Email, d.Reason)
	}
	return disabled
}

// disableOne 禁用单个用户。
//
// 只写 status 一列（UserUpdateFields{Status: true}），余额、分组权限、订阅
// 一律不动，这样人员回归时改回 active 即可恢复，不需要重新配置。
// 这与 content_moderation 的自动封禁走同一套范式。
//
// 禁用后必须失效鉴权缓存：API Key 鉴权走 Redis 缓存快照，
// 不失效的话已签发的 Key 在缓存 TTL 内仍然可用，禁用就不是即时生效的。
func (e *offboardExecutor) disableOne(ctx context.Context, userID int64) error {
	user, err := e.userRepo.GetByID(ctx, userID)
	if err != nil {
		return fmt.Errorf("get user: %w", err)
	}
	if user == nil {
		return fmt.Errorf("user %d not found", userID)
	}
	// 再挡一层 admin：判定阶段已经跳过，这里防的是判定与执行之间
	// 用户被提权、或调用方绕过判定直接传入 decision 的情况。
	// 服务端 UpdateUser 也会拒绝，但那是 HTTP 层，这里走的是 repo。
	if user.IsAdmin() {
		return fmt.Errorf("refuse to disable admin user %d", userID)
	}
	if user.Status == StatusDisabled {
		// 已经是禁用状态，幂等返回成功。
		return nil
	}

	user.Status = StatusDisabled
	if err := e.userRepo.Update(ctx, user, UserUpdateFields{Status: true}); err != nil {
		return fmt.Errorf("update status: %w", err)
	}
	if e.authCacheInvalidator != nil {
		e.authCacheInvalidator.InvalidateAuthCacheByUserID(ctx, userID)
	}
	return nil
}

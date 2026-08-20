// 私有扩展（不属于 upstream sub2api）。
//
// 本文件：「飞书离职自动禁用」的 wire provider。
// 所含内容：ProvideFeishuOffboardConfigStore、ProvideFeishuOffboardEmailNotifier、
//
//	ProvideFeishuOffboardService。
//
// merge 策略：纯新增文件。provider 单独放这里而不是塞进 upstream 的 wire.go，
//
//	是为了把 provider set 的改动压到一行引用，减少 merge 冲突面。
//
// @author wangzhong
package service

import (
	"database/sql"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/redis/go-redis/v9"
)

func ProvideFeishuOffboardConfigStore(settingRepo SettingRepository) *FeishuOffboardConfigStore {
	return NewFeishuOffboardConfigStore(settingRepo)
}

func ProvideFeishuOffboardEmailNotifier(
	emailService *EmailService, settingRepo SettingRepository,
) FeishuOffboardNotifier {
	return NewFeishuOffboardEmailNotifier(emailService, settingRepo)
}

// ProvideFeishuOffboardService 构造并启动定时任务。
//
// Start() 内部会读配置，未启用时静默不起 cron，所以这里无条件调用是安全的：
// 管理员在页面打开开关后走 SaveConfig → Reload 即可生效，无需重启进程。
func ProvideFeishuOffboardService(
	configStore *FeishuOffboardConfigStore,
	runRepo FeishuOffboardRepository,
	userRepo UserRepository,
	opsRepo OpsRepository,
	notifier FeishuOffboardNotifier,
	invalidator *APIKeyService,
	db *sql.DB,
	redisClient *redis.Client,
	cfg *config.Config,
) *FeishuOffboardService {
	svc := NewFeishuOffboardService(
		configStore, runRepo, userRepo, opsRepo, notifier,
		invalidator, db, redisClient, cfg,
	)
	svc.Start()
	return svc
}

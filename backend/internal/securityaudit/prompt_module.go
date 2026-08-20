package securityaudit

import "github.com/google/wire"

var ProviderSet = wire.NewSet(
	NewPostgreSQLRepository,
	wire.Bind(new(JobRepository), new(*PostgreSQLRepository)),
	wire.Bind(new(EventRepository), new(*PostgreSQLRepository)),
	NewRedisPayloadStore,
	wire.Bind(new(PayloadStore), new(*RedisPayloadStore)),
	NewOpenAICompatibleScanner,
	wire.Bind(new(PromptScanner), new(*OpenAICompatibleScanner)),
	NewAtomicMetrics,
	wire.Bind(new(Metrics), new(*AtomicMetrics)),
	NewConfigManager,
	wire.Bind(new(ConfigStore), new(*ConfigManager)),
	NewPromptService,
	wire.Bind(new(PromptEngine), new(*PromptService)),
	wire.Bind(new(PromptAdminService), new(*PromptService)),
	NewLegacyModerationAdapter,
	// 私有扩展：用 ProvideCoordinator 替代 NewCoordinator，把 DLP 引擎一并装上，
	// 保证重跑 wire 后 DLP 仍然生效。实现见 coordinator_dlp.go。
	ProvideCoordinator,
	wire.Bind(new(DLPEngine), new(*PromptService)),
	NewPromptAdminHandler,
)

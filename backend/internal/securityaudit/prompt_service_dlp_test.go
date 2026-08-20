package securityaudit

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// dlpServiceRequest 构造一条带敏感信息的请求。
// 身份证号是 high 严重度，在 BlockOnHighSeverity 打开时应被拦截。
func dlpServiceRequest(groupID *int64) Request {
	return Request{
		RequestID: "req-1", Protocol: "openai_chat", Stage: "http", GroupID: groupID,
		Body: []byte(`{"messages":[{"role":"user","content":"身份证号 110101199003072316 已核验"}]}`),
	}
}

// newDLPService 组装一个只带 DLP 能力的 PromptService。
// scanner 传 nil：本测试只关心 DLP，不触碰 qwen3guard 链路。
func newDLPService(cfg ActiveConfig) *PromptService {
	return &PromptService{
		config:    &fakeConfigStore{cfg: cfg, active: true},
		evaluator: newDLPTestEvaluator(&dlpStubScanner{}, &dlpNoopRepo{}),
		metrics:   NewAtomicMetrics(),
		clock:     realClock{},
	}
}

// dlpServiceConfig 构造一份 DLP 已启用、qwen3guard 处于指定模式的配置。
func dlpServiceConfig(t *testing.T, mode Mode) ActiveConfig {
	t.Helper()
	confirmServer, _ := newDLPConfirmStub(t, true, http.StatusOK)
	cfg := dlpTestConfig(confirmServer.URL, true)
	cfg.DLP.AllGroups = true
	switch mode {
	case ModeOff:
		// upstream 的 EffectiveMode：Enabled=false 即 ModeOff。
		cfg.Enabled, cfg.BlockingEnabled = false, false
	case ModeAsync:
		cfg.Enabled, cfg.BlockingEnabled = true, false
	case ModeBlocking:
		cfg.Enabled, cfg.BlockingEnabled = true, true
	}
	require.Equal(t, mode, cfg.EffectiveMode(), "配置构造错误")
	return cfg
}

// 核心回归：DLP 的执行不依赖 qwen3guard 的审计模式。
func TestServiceEvaluateDLPIgnoresAuditMode(t *testing.T) {
	for _, mode := range []Mode{ModeOff, ModeAsync, ModeBlocking} {
		t.Run(string(mode), func(t *testing.T) {
			service := newDLPService(dlpServiceConfig(t, mode))

			decision := service.EvaluateDLP(context.Background(), dlpServiceRequest(nil))

			require.NotNil(t, decision, "DLP 应不受审计模式影响")
			require.Equal(t, DecisionBlock, decision.Kind)
			require.Equal(t, ErrorCodeDLPBlocked, decision.ErrorCode)
		})
	}
}

// 升级路径：分组字段是后加的，旧配置里没有 all_groups，不能因此让 DLP 静默停摆。
func TestServiceEvaluateDLPLegacyConfigWithoutGroupScopeStillWorks(t *testing.T) {
	confirmServer, _ := newDLPConfirmStub(t, true, http.StatusOK)
	// 模拟旧配置反序列化的结果：Enabled=true，但分组字段全是零值。
	stored := DLPConfig{
		Enabled: true, ConfirmEnabled: true, BlockOnHighSeverity: true,
		ConfirmTimeoutMS: 5000,
		Endpoints: []StorageEndpoint{{
			ID: "dlp-1", BaseURL: confirmServer.URL, Model: DefaultDLPConfirmModel,
			TimeoutMS: 5000, Enabled: true,
		}},
	}
	require.False(t, stored.AllGroups, "前提：旧配置没有 all_groups 字段")

	active := stored.ToActiveDLPConfig(nil)
	require.True(t, active.AllGroups, "旧配置应被解释为全部分组，而不是不对任何分组生效")

	cfg := dlpServiceConfig(t, ModeBlocking)
	cfg.DLP = active
	decision := newDLPService(cfg).EvaluateDLP(context.Background(), dlpServiceRequest(nil))

	require.NotNil(t, decision, "升级后旧配置的 DLP 必须继续工作")
	require.Equal(t, DecisionBlock, decision.Kind)
}

// DLP 关闭时必须完全不工作，即便 qwen3guard 开着。
func TestServiceEvaluateDLPRespectsOwnEnabledSwitch(t *testing.T) {
	cfg := dlpServiceConfig(t, ModeBlocking)
	cfg.DLP.Enabled = false
	service := newDLPService(cfg)

	decision := service.EvaluateDLP(context.Background(), dlpServiceRequest(nil))

	require.Nil(t, decision, "DLP 关闭时不应产生决策")
}

// DLP 的分组范围独立于 qwen3guard 的分组设置。
func TestServiceEvaluateDLPUsesOwnGroupScope(t *testing.T) {
	inScope, outOfScope := int64(7), int64(9)
	tests := []struct {
		name       string
		dlpGroups  []int64
		dlpAll     bool
		guardAll   bool
		guardGroup []int64
		requestID  *int64
		wantBlock  bool
	}{
		{
			name:   "DLP 全部分组时不受 qwen3guard 的窄范围影响",
			dlpAll: true, guardAll: false, guardGroup: []int64{outOfScope},
			requestID: &inScope, wantBlock: true,
		},
		{
			name:      "请求分组在 DLP 范围内则检测",
			dlpGroups: []int64{inScope}, guardAll: true,
			requestID: &inScope, wantBlock: true,
		},
		{
			name:      "请求分组不在 DLP 范围内则跳过，即便 qwen3guard 覆盖全部分组",
			dlpGroups: []int64{inScope}, guardAll: true,
			requestID: &outOfScope, wantBlock: false,
		},
		{
			name:      "指定分组模式下无分组的请求跳过",
			dlpGroups: []int64{inScope}, guardAll: true,
			requestID: nil, wantBlock: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := dlpServiceConfig(t, ModeBlocking)
			cfg.DLP.AllGroups, cfg.DLP.GroupIDs = tt.dlpAll, tt.dlpGroups
			cfg.AllGroups, cfg.GroupIDs = tt.guardAll, tt.guardGroup
			service := newDLPService(cfg)

			decision := service.EvaluateDLP(context.Background(), dlpServiceRequest(tt.requestID))

			if tt.wantBlock {
				require.NotNil(t, decision)
				require.Equal(t, DecisionBlock, decision.Kind)
				return
			}
			require.Nil(t, decision)
		})
	}
}

// qwen3guard 的「只扫最后一轮」不得收窄 DLP 的扫描范围。
func TestServiceEvaluateDLPScansFullConversationDespiteLatestTurnOnly(t *testing.T) {
	cfg := dlpServiceConfig(t, ModeBlocking)
	cfg.BlockingLatestTurnOnly = true
	service := newDLPService(cfg)

	// 敏感信息在靠前的历史轮次里，最后一轮是无害内容。
	req := Request{
		RequestID: "req-history", Protocol: "openai_chat", Stage: "http",
		Body: []byte(`{"messages":[` +
			`{"role":"user","content":"身份证号 110101199003072316"},` +
			`{"role":"assistant","content":"已记录"},` +
			`{"role":"user","content":"帮我写个快速排序"}]}`),
	}

	decision := service.EvaluateDLP(context.Background(), req)

	require.NotNil(t, decision, "历史轮次里的敏感信息必须被检出")
	require.Equal(t, DecisionBlock, decision.Kind)
}

// 配置不可用时 fail-open，不能把网关拖挂。
func TestServiceEvaluateDLPFailsOpenWithoutConfig(t *testing.T) {
	service := &PromptService{
		config:    &fakeConfigStore{active: false},
		evaluator: newDLPTestEvaluator(&dlpStubScanner{}, &dlpNoopRepo{}),
		metrics:   NewAtomicMetrics(),
		clock:     realClock{},
	}

	decision := service.EvaluateDLP(context.Background(), dlpServiceRequest(nil))

	require.Nil(t, decision, "配置读不到时应放行")
}

// 请求体不是合法 JSON 时放行，交给 upstream 流程报错。
func TestServiceEvaluateDLPFailsOpenOnUnparsableBody(t *testing.T) {
	service := newDLPService(dlpServiceConfig(t, ModeBlocking))

	decision := service.EvaluateDLP(context.Background(), Request{
		RequestID: "req-bad", Protocol: "openai_chat", Body: []byte(`not json`),
	})

	require.Nil(t, decision)
}

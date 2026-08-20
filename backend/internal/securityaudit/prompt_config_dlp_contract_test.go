// prompt_config_dlp_contract_test.go
// =============================================================================
// 私有扩展（不属于 upstream sub2api）：DLP 配置接口的前后端契约测试。
//
// 为什么需要这个文件：
//
//	后端有 ToPublicDLPConfig 的单测，前端有 dlpConfigToDraft 的单测，两边都绿——
//	但两边各用各的手写 fixture，没有任何测试验证「后端真实序列化出来的 JSON
//	能被前端解析成非空规则表」。字段名对不上、层级挪了位、tag 写错，两边测试
//	依然全绿，只有跑起来才看得见（表现为界面显示 0/0 条规则）。
//
// 做法：
//
//	本测试把真实的 PublicDLPConfig 序列化后写入 frontend 的 fixture 文件，
//	前端测试直接读这份字节。fixture 已提交进 git，所以跑前端测试不需要装 Go；
//	一旦后端结构变化导致 fixture 过期，本测试会失败并提示重新生成。
//
// 与 upstream 合并策略：
//   - 纯新增文件，merge 不会冲突。
//
// =============================================================================
package securityaudit

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

// updateFixtures 用于重新生成前端契约 fixture：
//
//	go test ./internal/securityaudit/ -run TestDLPConfigFixture -update-fixtures
var updateFixtures = flag.Bool("update-fixtures", false, "重新生成前端契约 fixture")

// dlpContractFixturePath 是前端测试读取的 fixture 路径。
const dlpContractFixturePath = "../../../frontend/src/features/dlp/__tests__/fixtures/dlpConfig.backend.json"

// buildDLPContractFixture 构造一份覆盖面尽量宽的配置：
// 带管理员覆盖（改严重度 + 关规则），以便前端断言这些字段真的过得来。
func buildDLPContractFixture(t *testing.T) PublicDLPConfig {
	t.Helper()

	credentialRules := DLPRuleIDsByScanner(DLPScannerCredential)
	if len(credentialRules) < 2 {
		t.Fatalf("凭证检测器至少应有 2 条规则，实际 %d 条", len(credentialRules))
	}

	stored := &DLPConfig{
		Enabled:                true,
		Scanners:               []string{DLPScannerCredential, DLPScannerPII, DLPScannerSensitive},
		ConfirmEnabled:         true,
		ConfirmTimeoutMS:       5000,
		CacheEnabled:           true,
		CacheSensitiveTTLHours: 6,
		CacheBenignTTLHours:    24,
		BlockOnHighSeverity:    true,
		AllGroups:              true,
		RuleOverrides: DLPRuleOverrides{
			credentialRules[0]: {Severity: RiskHigh},
			credentialRules[1]: {Disabled: true},
		},
	}
	return publicDLPFromStorage(stored, nil)
}

// TestDLPConfigFixtureMatchesBackend 校验已提交的 fixture 与当前后端结构一致。
//
// 失败说明后端 DTO 变了但 fixture 没重新生成，此时前端测试仍在按旧结构验证，
// 等于契约检查失效。用 -run TestDLPConfigFixture -update 重新生成。
func TestDLPConfigFixtureMatchesBackend(t *testing.T) {
	current, err := json.MarshalIndent(buildDLPContractFixture(t), "", "  ")
	if err != nil {
		t.Fatalf("序列化配置失败: %v", err)
	}
	current = append(current, '\n')

	if *updateFixtures {
		if err := os.MkdirAll(filepath.Dir(dlpContractFixturePath), 0o755); err != nil {
			t.Fatalf("创建 fixture 目录失败: %v", err)
		}
		if err := os.WriteFile(dlpContractFixturePath, current, 0o644); err != nil {
			t.Fatalf("写入 fixture 失败: %v", err)
		}
		t.Logf("已更新 fixture: %s", dlpContractFixturePath)
		return
	}

	committed, err := os.ReadFile(dlpContractFixturePath)
	if err != nil {
		t.Fatalf("读取 fixture 失败（用 go test ./internal/securityaudit/ -run TestDLPConfigFixture -update-fixtures 生成）: %v", err)
	}
	if string(committed) != string(current) {
		t.Errorf("fixture 与后端结构不一致，前端契约测试已失效。\n"+
			"用以下命令重新生成后一并提交：\n"+
			"  go test ./internal/securityaudit/ -run TestDLPConfigFixture -update-fixtures\n"+
			"当前后端输出:\n%s", current)
	}
}

// TestDLPContractFixtureCarriesRules 兜住「fixture 本身是空表」这种退化：
// 若 fixture 里规则表为空，前端契约测试断言的就是空数组，等于什么都没验证。
func TestDLPContractFixtureCarriesRules(t *testing.T) {
	fixture := buildDLPContractFixture(t)

	if len(fixture.Rules) != len(dlpRules) {
		t.Fatalf("fixture 应下发全部 %d 条规则，实际 %d 条", len(dlpRules), len(fixture.Rules))
	}
	if len(fixture.AvailableSeverities) == 0 || len(fixture.BlockingSeverities) == 0 {
		t.Fatal("fixture 缺少严重度取值或拦截阈值，前端渲染不出选择器与「会拦/仅记录」")
	}

	// 每条规则都必须挂在一个已注册的检测器下，否则前端按 scanner_id 分组时
	// 会把它归到没有任何界面入口的分组里，规则就此消失。
	registered := map[string]bool{}
	for _, definition := range dlpScannerDefinitionList() {
		registered[definition.ID] = true
	}
	var overriddenSeverity, disabled int
	for _, rule := range fixture.Rules {
		if !registered[rule.ScannerID] {
			t.Errorf("规则 %s 的 scanner_id=%q 未注册，界面上无处可挂", rule.ID, rule.ScannerID)
		}
		if rule.Title == "" {
			t.Errorf("规则 %s 缺少标题，界面只能显示空行", rule.ID)
		}
		if rule.Severity != rule.DefaultSeverity {
			overriddenSeverity++
		}
		if rule.Disabled {
			disabled++
		}
	}

	// fixture 必须真的带上管理员覆盖，否则前端无法验证覆盖字段是否传得过来。
	if overriddenSeverity == 0 {
		t.Error("fixture 未包含改过严重度的规则，前端验证不到覆盖生效")
	}
	if disabled == 0 {
		t.Error("fixture 未包含被关掉的规则，前端验证不到 disabled 字段")
	}
}

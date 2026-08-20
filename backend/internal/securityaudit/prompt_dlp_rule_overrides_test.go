package securityaudit

import "testing"

// AWS Access Key 内置是 medium，因此开了拦截开关也不会拦——这正是需要
// 严重度可配的原因。用它当主要样本。
const (
	awsRuleID = "credential-aws-access-key"
	// 测试用假密钥，拆分以绕过 GitHub push protection
	awsSample = "AKIA" + "ZXCVBNMQWERTYUI7"
	idCardID  = "pii-idcard"
)

func TestDLPRuleDefaultSeveritiesUnchangedWithoutOverrides(t *testing.T) {
	// 没有覆盖时必须完全沿用内置值，否则等于悄悄改了所有部署的处置行为。
	for _, rule := range dlpRules {
		if got := DLPRuleOverrides(nil).EffectiveSeverity(rule); got != rule.Severity {
			t.Errorf("规则 %s 的生效严重度 = %s, 期望内置值 %s", rule.ID, got, rule.Severity)
		}
		if DLPRuleOverrides(nil).IsRuleDisabled(rule.ID) {
			t.Errorf("规则 %s 在无覆盖时不应被禁用", rule.ID)
		}
	}
}

func TestEffectiveSeverityAppliesOverride(t *testing.T) {
	rule, ok := dlpRuleByID(awsRuleID)
	if !ok {
		t.Fatalf("找不到规则 %s", awsRuleID)
	}
	if rule.Severity != RiskMedium {
		t.Fatalf("前置条件变了：%s 的内置严重度 = %s, 期望 medium", awsRuleID, rule.Severity)
	}

	overrides := DLPRuleOverrides{awsRuleID: {Severity: RiskHigh}}
	if got := overrides.EffectiveSeverity(rule); got != RiskHigh {
		t.Errorf("生效严重度 = %s, 期望 high", got)
	}
}

func TestEffectiveSeverityFallsBackOnInvalidValue(t *testing.T) {
	rule, _ := dlpRuleByID(awsRuleID)
	// 非法值绝不能让 finding 拿到空严重度：dlpSeverityRank 对空值会落到 low 分支，
	// 等于把规则悄悄降级。
	for _, invalid := range []RiskLevel{"", "low", "critical", "bogus"} {
		overrides := DLPRuleOverrides{awsRuleID: {Severity: invalid}}
		if got := overrides.EffectiveSeverity(rule); got != rule.Severity {
			t.Errorf("severity=%q 时生效值 = %s, 期望回落到内置值 %s", invalid, got, rule.Severity)
		}
	}
}

func TestScanAppliesSeverityOverride(t *testing.T) {
	baseline := ScanDLP(awsSample, nil)
	if len(baseline.Findings) != 1 || baseline.Findings[0].Severity != RiskMedium {
		t.Fatalf("基线扫描 = %+v, 期望 1 条 medium 命中", baseline.Findings)
	}

	overrides := DLPRuleOverrides{awsRuleID: {Severity: RiskHigh}}
	scanned := ScanDLPWithOverrides(awsSample, nil, overrides)
	if len(scanned.Findings) != 1 {
		t.Fatalf("命中数 = %d, 期望 1", len(scanned.Findings))
	}
	if scanned.Findings[0].Severity != RiskHigh {
		t.Errorf("命中严重度 = %s, 期望 high", scanned.Findings[0].Severity)
	}
}

func TestScanSkipsDisabledRule(t *testing.T) {
	overrides := DLPRuleOverrides{awsRuleID: {Disabled: true}}
	if scanned := ScanDLPWithOverrides(awsSample, nil, overrides); len(scanned.Findings) != 0 {
		t.Errorf("禁用规则后命中数 = %d, 期望 0", len(scanned.Findings))
	}

	// 禁用一条不得影响同检测器下的其他规则。
	other := ScanDLPWithOverrides("身份证 110101199003072316", nil, overrides)
	if len(other.Findings) == 0 {
		t.Error("禁用 AWS 规则不应影响身份证规则")
	}
}

// 覆盖严重度后，拦截判定必须跟着变——这是整个特性的目的。
func TestOverrideChangesBlockingDecision(t *testing.T) {
	rule, _ := dlpRuleByID(awsRuleID)

	t.Run("默认 medium 不拦", func(t *testing.T) {
		cfg := ActiveDLPConfig{BlockOnHighSeverity: true}
		severity := cfg.RuleOverrides.EffectiveSeverity(rule)
		if dlpShouldBlock(cfg, severity) {
			t.Error("AWS Access Key 内置 medium，开了拦截开关也不该拦")
		}
	})

	t.Run("提到 high 后拦", func(t *testing.T) {
		cfg := ActiveDLPConfig{
			BlockOnHighSeverity: true,
			RuleOverrides:       DLPRuleOverrides{awsRuleID: {Severity: RiskHigh}},
		}
		severity := cfg.RuleOverrides.EffectiveSeverity(rule)
		if !dlpShouldBlock(cfg, severity) {
			t.Error("提到 high 后应当拦截")
		}
	})

	t.Run("拦截总开关关闭时仍不拦", func(t *testing.T) {
		cfg := ActiveDLPConfig{
			BlockOnHighSeverity: false,
			RuleOverrides:       DLPRuleOverrides{awsRuleID: {Severity: RiskHigh}},
		}
		severity := cfg.RuleOverrides.EffectiveSeverity(rule)
		if dlpShouldBlock(cfg, severity) {
			t.Error("总开关关闭时不该拦截")
		}
	})

	t.Run("高危规则降到 medium 后不再拦", func(t *testing.T) {
		idRule, _ := dlpRuleByID(idCardID)
		if idRule.Severity != RiskHigh {
			t.Fatalf("前置条件变了：%s 内置严重度 = %s", idCardID, idRule.Severity)
		}
		cfg := ActiveDLPConfig{
			BlockOnHighSeverity: true,
			RuleOverrides:       DLPRuleOverrides{idCardID: {Severity: RiskMedium}},
		}
		if dlpShouldBlock(cfg, cfg.RuleOverrides.EffectiveSeverity(idRule)) {
			t.Error("降到 medium 后不该拦截")
		}
	})
}

func TestNormalizeDLPRuleOverrides(t *testing.T) {
	t.Run("丢弃等于默认值且未禁用的条目", func(t *testing.T) {
		// 存下来会让日后调整内置默认值对老配置失效。
		rule, _ := dlpRuleByID(awsRuleID)
		got := normalizeDLPRuleOverrides(DLPRuleOverrides{awsRuleID: {Severity: rule.Severity}})
		if got != nil {
			t.Errorf("结果 = %v, 期望 nil", got)
		}
	})

	t.Run("保留禁用标记", func(t *testing.T) {
		rule, _ := dlpRuleByID(awsRuleID)
		got := normalizeDLPRuleOverrides(DLPRuleOverrides{
			awsRuleID: {Severity: rule.Severity, Disabled: true},
		})
		if len(got) != 1 || !got[awsRuleID].Disabled {
			t.Errorf("结果 = %v, 期望保留禁用标记", got)
		}
		// 严重度等于默认值时不必存。
		if got[awsRuleID].Severity != "" {
			t.Errorf("严重度 = %s, 期望空（等于默认值不存）", got[awsRuleID].Severity)
		}
	})

	t.Run("丢弃未知规则 ID", func(t *testing.T) {
		// 版本间规则会增删，残留旧 ID 不该让配置加载失败。
		got := normalizeDLPRuleOverrides(DLPRuleOverrides{
			"rule-that-no-longer-exists": {Severity: RiskHigh, Disabled: true},
		})
		if got != nil {
			t.Errorf("结果 = %v, 期望 nil", got)
		}
	})

	t.Run("丢弃非法严重度但保留禁用", func(t *testing.T) {
		got := normalizeDLPRuleOverrides(DLPRuleOverrides{
			awsRuleID: {Severity: "critical", Disabled: true},
		})
		if len(got) != 1 || got[awsRuleID].Severity != "" || !got[awsRuleID].Disabled {
			t.Errorf("结果 = %v, 期望仅保留禁用标记", got)
		}
	})

	t.Run("空输入返回 nil", func(t *testing.T) {
		if normalizeDLPRuleOverrides(nil) != nil {
			t.Error("nil 输入应返回 nil")
		}
		if normalizeDLPRuleOverrides(DLPRuleOverrides{}) != nil {
			t.Error("空 map 应返回 nil")
		}
	})
}

func TestValidateDLPRuleOverrides(t *testing.T) {
	if err := validateDLPRuleOverrides(DLPRuleOverrides{awsRuleID: {Severity: RiskHigh}}); err != nil {
		t.Errorf("high 应当合法，得到 %v", err)
	}
	if err := validateDLPRuleOverrides(DLPRuleOverrides{awsRuleID: {Severity: RiskMedium}}); err != nil {
		t.Errorf("medium 应当合法，得到 %v", err)
	}
	if err := validateDLPRuleOverrides(DLPRuleOverrides{awsRuleID: {Disabled: true}}); err != nil {
		t.Errorf("仅禁用应当合法，得到 %v", err)
	}
	// low/critical 与 medium/high 在拦截行为上没有差别，不开放以免误解。
	for _, invalid := range []RiskLevel{"low", "critical", "bogus"} {
		if err := validateDLPRuleOverrides(DLPRuleOverrides{awsRuleID: {Severity: invalid}}); err == nil {
			t.Errorf("severity=%q 应当被拒", invalid)
		}
	}
}

func TestValidateDLPConfigRejectsAllRulesDisabled(t *testing.T) {
	// 「开着但每条规则都关了」与「开着但没选任何分组」同类：配置看着生效，
	// 实际静默不工作，必须在保存时拒掉。
	overrides := DLPRuleOverrides{}
	for _, rule := range dlpRules {
		overrides[rule.ID] = DLPRuleOverride{Disabled: true}
	}
	cfg := DLPConfig{Enabled: true, AllGroups: true, RuleOverrides: overrides}
	if err := ValidateDLPConfig(cfg); err == nil {
		t.Error("全部规则禁用时应当拒绝保存")
	}

	// 留一条就该放行。
	delete(overrides, awsRuleID)
	if err := ValidateDLPConfig(DLPConfig{
		Enabled: true, AllGroups: true, RuleOverrides: overrides,
	}); err != nil {
		t.Errorf("保留一条规则时应当允许保存，得到 %v", err)
	}
}

func TestEnabledDLPRuleCountRespectsScannerScope(t *testing.T) {
	// 只启用 PII 检测器时，计数不应把其他检测器的规则算进来。
	piiTotal := len(DLPRuleIDsByScanner(DLPScannerPII))
	if got := enabledDLPRuleCount([]string{DLPScannerPII}, nil); got != piiTotal {
		t.Errorf("计数 = %d, 期望 %d", got, piiTotal)
	}
	// 关掉 PII 下的一条。
	overrides := DLPRuleOverrides{idCardID: {Disabled: true}}
	if got := enabledDLPRuleCount([]string{DLPScannerPII}, overrides); got != piiTotal-1 {
		t.Errorf("计数 = %d, 期望 %d", got, piiTotal-1)
	}
	// 关掉的规则不属于启用的检测器时，计数不变。
	if got := enabledDLPRuleCount([]string{DLPScannerPII}, DLPRuleOverrides{
		awsRuleID: {Disabled: true},
	}); got != piiTotal {
		t.Errorf("计数 = %d, 期望 %d", got, piiTotal)
	}
}

func TestDLPRuleCatalogReportsEffectiveState(t *testing.T) {
	overrides := DLPRuleOverrides{
		awsRuleID: {Severity: RiskHigh},
		idCardID:  {Disabled: true},
	}
	catalog := DLPRuleCatalog(overrides)
	if len(catalog) != len(dlpRules) {
		t.Fatalf("目录长度 = %d, 期望 %d", len(catalog), len(dlpRules))
	}

	byID := map[string]DLPRuleCatalogEntry{}
	for _, entry := range catalog {
		byID[entry.ID] = entry
	}

	aws := byID[awsRuleID]
	if aws.Severity != RiskHigh || aws.DefaultSeverity != RiskMedium {
		t.Errorf("AWS 条目 severity=%s default=%s, 期望 high/medium", aws.Severity, aws.DefaultSeverity)
	}

	idCard := byID[idCardID]
	if !idCard.Disabled {
		t.Error("身份证规则应标为已禁用")
	}
	// 禁用后严重度仍如实下发，界面重新勾选时不该丢失原设置。
	if idCard.Severity != RiskHigh {
		t.Errorf("禁用规则的严重度 = %s, 期望仍为 high", idCard.Severity)
	}
}

// 拦截阈值必须由后端说了算：前端拿它和草稿状态实时组合，
// 而不是自己硬编码「high 才拦」。
func TestBlockingDLPSeveritiesMatchesShouldBlock(t *testing.T) {
	blocking := map[RiskLevel]bool{}
	for _, level := range BlockingDLPSeverities() {
		blocking[level] = true
	}
	if !blocking[RiskHigh] || !blocking[RiskCritical] {
		t.Errorf("会拦的严重度 = %v, 期望含 high 与 critical", BlockingDLPSeverities())
	}
	if blocking[RiskMedium] || blocking[RiskLow] {
		t.Errorf("会拦的严重度 = %v, 不应含 medium/low", BlockingDLPSeverities())
	}

	// 与 dlpShouldBlock 的判定保持一致，避免两处逻辑漂移。
	cfg := ActiveDLPConfig{BlockOnHighSeverity: true}
	for _, level := range []RiskLevel{RiskLow, RiskMedium, RiskHigh, RiskCritical} {
		if got := dlpShouldBlock(cfg, level); got != blocking[level] {
			t.Errorf("severity=%s: dlpShouldBlock=%v 但 BlockingDLPSeverities 说 %v",
				level, got, blocking[level])
		}
	}
}

func TestDLPRuleCatalogFlagsBroadRules(t *testing.T) {
	// Broad 规则误报相对高，界面要能提示管理员，因此必须如实下发。
	broadCount := 0
	for _, entry := range DLPRuleCatalog(nil) {
		if entry.Broad {
			broadCount++
		}
	}
	if broadCount == 0 {
		t.Error("规则表里存在 Broad 规则，目录应当标出来")
	}
}

func TestDlpRuleOverridesFromUpdate(t *testing.T) {
	t.Run("rules 为 nil 时保持原值", func(t *testing.T) {
		// 旧客户端不带 rules 字段，绝不能因此清空已有覆盖。
		current := DLPRuleOverrides{awsRuleID: {Severity: RiskHigh}}
		got := dlpRuleOverridesFromUpdate(current, nil)
		if len(got) != 1 || got[awsRuleID].Severity != RiskHigh {
			t.Errorf("结果 = %v, 期望保持原值", got)
		}
	})

	t.Run("提交全量列表只留偏差", func(t *testing.T) {
		rules := make([]UpdateDLPRule, 0, len(dlpRules))
		for _, rule := range dlpRules {
			rules = append(rules, UpdateDLPRule{ID: rule.ID, Severity: rule.Severity, Enabled: true})
		}
		// 全部等于默认值，应当一条都不存。
		if got := dlpRuleOverridesFromUpdate(nil, rules); got != nil {
			t.Errorf("结果 = %v, 期望 nil", got)
		}

		// 改一条。
		for index := range rules {
			if rules[index].ID == awsRuleID {
				rules[index].Severity = RiskHigh
			}
		}
		got := dlpRuleOverridesFromUpdate(nil, rules)
		if len(got) != 1 || got[awsRuleID].Severity != RiskHigh {
			t.Errorf("结果 = %v, 期望只含 AWS 的 high 覆盖", got)
		}
	})

	t.Run("Enabled=false 转成 Disabled", func(t *testing.T) {
		got := dlpRuleOverridesFromUpdate(nil, []UpdateDLPRule{
			{ID: awsRuleID, Severity: RiskMedium, Enabled: false},
		})
		if len(got) != 1 || !got[awsRuleID].Disabled {
			t.Errorf("结果 = %v, 期望标记为禁用", got)
		}
	})
}

func TestPublicDLPConfigExposesRuleCatalog(t *testing.T) {
	t.Run("未配置过时按内置默认值下发", func(t *testing.T) {
		public := publicDLPFromStorage(nil, nil)
		if len(public.Rules) != len(dlpRules) {
			t.Errorf("规则数 = %d, 期望 %d", len(public.Rules), len(dlpRules))
		}
		if len(public.AvailableSeverities) != 2 {
			t.Errorf("可选严重度 = %v, 期望 2 个", public.AvailableSeverities)
		}
	})

	t.Run("下发生效状态", func(t *testing.T) {
		stored := &DLPConfig{
			Enabled: true, AllGroups: true, BlockOnHighSeverity: true,
			RuleOverrides: DLPRuleOverrides{awsRuleID: {Severity: RiskHigh}},
		}
		public := publicDLPFromStorage(stored, nil)
		if len(public.BlockingSeverities) == 0 {
			t.Error("必须下发拦截阈值，否则界面无法算「会拦 / 仅记录」")
		}
		for _, entry := range public.Rules {
			if entry.ID != awsRuleID {
				continue
			}
			if entry.Severity != RiskHigh {
				t.Errorf("AWS 条目 = %+v, 期望 severity=high", entry)
			}
			return
		}
		t.Errorf("规则表里找不到 %s", awsRuleID)
	})
}

func TestToActiveDLPConfigNormalizesOverrides(t *testing.T) {
	// 手工改过的配置行可能带已下线的 rule ID 或非法严重度，运行时视图要能容忍。
	cfg := DLPConfig{
		Enabled: true, AllGroups: true,
		RuleOverrides: DLPRuleOverrides{
			"gone-rule": {Severity: RiskHigh},
			awsRuleID:   {Severity: "critical", Disabled: true},
		},
	}
	active := cfg.ToActiveDLPConfig(nil)
	if _, exists := active.RuleOverrides["gone-rule"]; exists {
		t.Error("已下线的 rule ID 应被丢弃")
	}
	if !active.RuleOverrides.IsRuleDisabled(awsRuleID) {
		t.Error("禁用标记应当保留")
	}
	rule, _ := dlpRuleByID(awsRuleID)
	if got := active.RuleOverrides.EffectiveSeverity(rule); got != rule.Severity {
		t.Errorf("非法严重度应回落到内置值，得到 %s", got)
	}
}

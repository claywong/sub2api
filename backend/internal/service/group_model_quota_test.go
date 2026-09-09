//go:build unit

package service

import (
	"strings"
	"testing"
)

func f64(v float64) *float64 { return &v }

func TestGroupModelQuotasMatchRule(t *testing.T) {
	tests := []struct {
		name  string
		cfg   GroupModelQuotas
		model string
		want  string // 期望命中的规则 match，空串表示未命中
	}{
		{
			name:  "disabled config never matches",
			cfg:   GroupModelQuotas{Enabled: false, Rules: []GroupModelQuotaRule{{Match: "claude-opus*", Daily: f64(50)}}},
			model: "claude-opus-4-6",
			want:  "",
		},
		{
			name:  "empty rules never match",
			cfg:   GroupModelQuotas{Enabled: true},
			model: "claude-opus-4-6",
			want:  "",
		},
		{
			name:  "empty model never matches",
			cfg:   GroupModelQuotas{Enabled: true, Rules: []GroupModelQuotaRule{{Match: "claude-opus*", Daily: f64(50)}}},
			model: "   ",
			want:  "",
		},
		{
			name:  "exact match",
			cfg:   GroupModelQuotas{Enabled: true, Rules: []GroupModelQuotaRule{{Match: "gpt-6-astra", Daily: f64(10)}}},
			model: "gpt-6-astra",
			want:  "gpt-6-astra",
		},
		{
			name:  "prefix match",
			cfg:   GroupModelQuotas{Enabled: true, Rules: []GroupModelQuotaRule{{Match: "claude-opus*", Daily: f64(50)}}},
			model: "claude-opus-4-6",
			want:  "claude-opus*",
		},
		{
			name: "exact wins over prefix",
			cfg: GroupModelQuotas{Enabled: true, Rules: []GroupModelQuotaRule{
				{Match: "claude-opus*", Daily: f64(50)},
				{Match: "claude-opus-4-6", Daily: f64(10)},
			}},
			model: "claude-opus-4-6",
			want:  "claude-opus-4-6",
		},
		{
			name: "exact wins over prefix regardless of declaration order",
			cfg: GroupModelQuotas{Enabled: true, Rules: []GroupModelQuotaRule{
				{Match: "claude-opus-4-6", Daily: f64(10)},
				{Match: "claude-opus*", Daily: f64(50)},
			}},
			model: "claude-opus-4-6",
			want:  "claude-opus-4-6",
		},
		{
			name: "longest prefix wins",
			cfg: GroupModelQuotas{Enabled: true, Rules: []GroupModelQuotaRule{
				{Match: "claude-opus*", Daily: f64(50)},
				{Match: "claude-opus-4*", Daily: f64(20)},
				{Match: "claude*", Daily: f64(100)},
			}},
			model: "claude-opus-4-6",
			want:  "claude-opus-4*",
		},
		{
			name: "sibling model falls back to broader prefix",
			cfg: GroupModelQuotas{Enabled: true, Rules: []GroupModelQuotaRule{
				{Match: "claude-opus*", Daily: f64(50)},
				{Match: "claude-opus-4-6", Daily: f64(10)},
			}},
			model: "claude-opus-4-5",
			want:  "claude-opus*",
		},
		{
			name: "rule without any limit is skipped so broader rule still applies",
			cfg: GroupModelQuotas{Enabled: true, Rules: []GroupModelQuotaRule{
				{Match: "claude-opus*", Daily: f64(50)},
				{Match: "claude-opus-4-6"}, // 三窗口全空
			}},
			model: "claude-opus-4-6",
			want:  "claude-opus*",
		},
		{
			name:  "match is case insensitive",
			cfg:   GroupModelQuotas{Enabled: true, Rules: []GroupModelQuotaRule{{Match: "Claude-Opus*", Daily: f64(50)}}},
			model: "CLAUDE-OPUS-4-6",
			want:  "Claude-Opus*",
		},
		{
			name:  "gemini models/ prefix is normalized",
			cfg:   GroupModelQuotas{Enabled: true, Rules: []GroupModelQuotaRule{{Match: "gemini-3-pro", Daily: f64(5)}}},
			model: "models/gemini-3-pro",
			want:  "gemini-3-pro",
		},
		{
			name:  "no rule matches unrelated model",
			cfg:   GroupModelQuotas{Enabled: true, Rules: []GroupModelQuotaRule{{Match: "claude-opus*", Daily: f64(50)}}},
			model: "gpt-6-astra",
			want:  "",
		},
		{
			name:  "zero limit still matches (explicit deny)",
			cfg:   GroupModelQuotas{Enabled: true, Rules: []GroupModelQuotaRule{{Match: "gpt-6-astra", Daily: f64(0)}}},
			model: "gpt-6-astra",
			want:  "gpt-6-astra",
		},
		{
			name: "equal length prefixes keep first declared",
			cfg: GroupModelQuotas{Enabled: true, Rules: []GroupModelQuotaRule{
				{Match: "claude-opu*", Daily: f64(1)},
				{Match: "claude-op*", Daily: f64(2)},
			}},
			model: "claude-opus-4-6",
			want:  "claude-opu*",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.cfg.MatchRule(tt.model)
			if tt.want == "" {
				if got != nil {
					t.Fatalf("expected no match, got rule %q", got.Match)
				}
				return
			}
			if got == nil {
				t.Fatalf("expected rule %q, got no match", tt.want)
			}
			if got.Match != tt.want {
				t.Fatalf("expected rule %q, got %q", tt.want, got.Match)
			}
		})
	}
}

func TestModelQuotaRuleKey(t *testing.T) {
	cases := map[string]string{
		"  Claude-Opus*  ": "claude-opus*",
		"gpt-6-astra":      "gpt-6-astra",
		"":                 "",
	}
	for in, want := range cases {
		if got := ModelQuotaRuleKey(in); got != want {
			t.Errorf("ModelQuotaRuleKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizeGroupModelQuotas(t *testing.T) {
	t.Run("enabled with empty rules is rejected", func(t *testing.T) {
		if _, err := normalizeGroupModelQuotas(GroupModelQuotas{Enabled: true}); err == nil {
			t.Fatal("expected error for enabled config with no rules")
		}
	})

	t.Run("disabled with empty rules is accepted", func(t *testing.T) {
		out, err := normalizeGroupModelQuotas(GroupModelQuotas{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if out.Enabled || len(out.Rules) != 0 {
			t.Fatalf("unexpected output: %+v", out)
		}
	})

	t.Run("mid-string wildcard is rejected", func(t *testing.T) {
		cfg := GroupModelQuotas{Enabled: true, Rules: []GroupModelQuotaRule{{Match: "claude-*-opus", Daily: f64(1)}}}
		if _, err := normalizeGroupModelQuotas(cfg); err == nil {
			t.Fatal("expected error for mid-string wildcard")
		}
	})

	t.Run("bare wildcard is rejected", func(t *testing.T) {
		cfg := GroupModelQuotas{Enabled: true, Rules: []GroupModelQuotaRule{{Match: "*", Daily: f64(1)}}}
		if _, err := normalizeGroupModelQuotas(cfg); err == nil {
			t.Fatal("expected error for bare wildcard")
		}
	})

	t.Run("empty match is rejected", func(t *testing.T) {
		cfg := GroupModelQuotas{Enabled: true, Rules: []GroupModelQuotaRule{{Match: "   ", Daily: f64(1)}}}
		if _, err := normalizeGroupModelQuotas(cfg); err == nil {
			t.Fatal("expected error for empty match")
		}
	})

	t.Run("negative limit is rejected", func(t *testing.T) {
		cfg := GroupModelQuotas{Enabled: true, Rules: []GroupModelQuotaRule{{Match: "gpt-6-astra", Daily: f64(-1)}}}
		if _, err := normalizeGroupModelQuotas(cfg); err == nil {
			t.Fatal("expected error for negative limit")
		}
	})

	t.Run("oversized match is rejected", func(t *testing.T) {
		long := strings.Repeat("a", 201)
		cfg := GroupModelQuotas{Enabled: true, Rules: []GroupModelQuotaRule{{Match: long, Daily: f64(1)}}}
		if _, err := normalizeGroupModelQuotas(cfg); err == nil {
			t.Fatal("expected error for match longer than rule_key column")
		}
		// 恰好 200（含末尾通配符）应通过
		ok := strings.Repeat("a", 199) + "*"
		cfg = GroupModelQuotas{Enabled: true, Rules: []GroupModelQuotaRule{{Match: ok, Daily: f64(1)}}}
		if _, err := normalizeGroupModelQuotas(cfg); err != nil {
			t.Fatalf("unexpected error at exactly 200 chars: %v", err)
		}
	})

	t.Run("duplicate match is rejected", func(t *testing.T) {
		cfg := GroupModelQuotas{Enabled: true, Rules: []GroupModelQuotaRule{
			{Match: "claude-opus*", Daily: f64(1)},
			{Match: "  Claude-Opus*  ", Daily: f64(2)},
		}}
		if _, err := normalizeGroupModelQuotas(cfg); err == nil {
			t.Fatal("expected error for duplicate rule keys")
		}
	})

	t.Run("match is trimmed but case preserved", func(t *testing.T) {
		cfg := GroupModelQuotas{Enabled: true, Rules: []GroupModelQuotaRule{{Match: "  Claude-Opus*  ", Daily: f64(1)}}}
		out, err := normalizeGroupModelQuotas(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if out.Rules[0].Match != "Claude-Opus*" {
			t.Fatalf("expected trimmed match, got %q", out.Rules[0].Match)
		}
	})

	t.Run("zero limit is accepted as explicit deny", func(t *testing.T) {
		cfg := GroupModelQuotas{Enabled: true, Rules: []GroupModelQuotaRule{{Match: "gpt-6-astra", Daily: f64(0)}}}
		out, err := normalizeGroupModelQuotas(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if out.Rules[0].Daily == nil || *out.Rules[0].Daily != 0 {
			t.Fatalf("expected zero daily limit preserved, got %+v", out.Rules[0].Daily)
		}
	})
}

func TestGroupModelQuotasEnabled(t *testing.T) {
	var nilGroup *Group
	if nilGroup.ModelQuotasEnabled() {
		t.Error("nil group should report quotas disabled")
	}

	g := &Group{ModelQuotas: GroupModelQuotas{Enabled: true}}
	if g.ModelQuotasEnabled() {
		t.Error("enabled config with no rules should report disabled")
	}

	g = &Group{ModelQuotas: GroupModelQuotas{Enabled: true, Rules: []GroupModelQuotaRule{{Match: "a*", Daily: f64(1)}}}}
	if !g.ModelQuotasEnabled() {
		t.Error("enabled config with rules should report enabled")
	}
}

func TestGroupModelQuotasDomainRoundTrip(t *testing.T) {
	src := GroupModelQuotas{Enabled: true, Rules: []GroupModelQuotaRule{
		{Match: "claude-opus*", Daily: f64(50), Weekly: f64(200)},
		{Match: "gpt-6-astra", Monthly: f64(30)},
	}}
	out := GroupModelQuotasFromDomain(DomainGroupModelQuotas(src))
	if out.Enabled != src.Enabled || len(out.Rules) != len(src.Rules) {
		t.Fatalf("round trip mismatch: %+v vs %+v", out, src)
	}
	for i := range src.Rules {
		if out.Rules[i].Match != src.Rules[i].Match {
			t.Errorf("rule %d match mismatch: %q vs %q", i, out.Rules[i].Match, src.Rules[i].Match)
		}
	}
	if out.Rules[0].Daily == nil || *out.Rules[0].Daily != 50 {
		t.Error("daily limit lost in round trip")
	}
	if out.Rules[1].Monthly == nil || *out.Rules[1].Monthly != 30 {
		t.Error("monthly limit lost in round trip")
	}
}

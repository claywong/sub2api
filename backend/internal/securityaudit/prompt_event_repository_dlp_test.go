package securityaudit

import (
	"strings"
	"testing"
)

func TestResolveEventScannerBackends(t *testing.T) {
	cases := []struct {
		name       string
		source     string
		wantList   []string
		wantNegate bool
	}{
		{name: "dlp 只看 DLP 事件", source: "dlp", wantList: []string{DLPScannerBackend}, wantNegate: false},
		{name: "guard 排除 DLP 事件", source: "guard", wantList: []string{DLPScannerBackend}, wantNegate: true},
		{name: "大小写与空格归一", source: "  DLP  ", wantList: []string{DLPScannerBackend}, wantNegate: false},
		{name: "空来源不过滤", source: "", wantList: nil, wantNegate: false},
		{name: "无法识别的来源退化为不过滤", source: "nonsense", wantList: nil, wantNegate: false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			backends, negate := ResolveEventScannerBackends(testCase.source)
			if len(backends) != len(testCase.wantList) {
				t.Fatalf("backends = %v, 期望 %v", backends, testCase.wantList)
			}
			for index := range backends {
				if backends[index] != testCase.wantList[index] {
					t.Errorf("backends[%d] = %s, 期望 %s", index, backends[index], testCase.wantList[index])
				}
			}
			if negate != testCase.wantNegate {
				t.Errorf("negate = %v, 期望 %v", negate, testCase.wantNegate)
			}
		})
	}
}

func TestCanonicalScannerBackends(t *testing.T) {
	got := canonicalScannerBackends([]string{" b ", "a", "", "b", "  "})
	want := []string{"a", "b"}
	if len(got) != len(want) {
		t.Fatalf("结果 = %v, 期望 %v", got, want)
	}
	for index := range got {
		if got[index] != want[index] {
			t.Errorf("结果[%d] = %s, 期望 %s", index, got[index], want[index])
		}
	}
	if canonicalScannerBackends(nil) != nil {
		t.Error("nil 输入应返回 nil")
	}
	if canonicalScannerBackends([]string{"", "  "}) != nil {
		t.Error("全空输入应返回 nil")
	}
}

func TestScannerBackendClause(t *testing.T) {
	t.Run("IN 条件", func(t *testing.T) {
		clause, args := scannerBackendClause([]string{DLPScannerBackend}, false, 3)
		if !strings.Contains(clause, "e.scanner_backend IN ($3)") {
			t.Errorf("clause = %q, 期望包含 IN ($3)", clause)
		}
		if strings.Contains(clause, "IS NULL") {
			t.Error("IN 条件不应包含 IS NULL 分支")
		}
		if len(args) != 1 || args[0] != DLPScannerBackend {
			t.Errorf("args = %v, 期望 [%s]", args, DLPScannerBackend)
		}
	})

	t.Run("NOT IN 条件补 NULL 分支", func(t *testing.T) {
		clause, args := scannerBackendClause([]string{DLPScannerBackend}, true, 1)
		if !strings.Contains(clause, "NOT IN ($1)") {
			t.Errorf("clause = %q, 期望包含 NOT IN ($1)", clause)
		}
		// 历史事件的 scanner_backend 可能为 NULL，NOT IN 会把它们整体判 NULL 而漏掉。
		if !strings.Contains(clause, "e.scanner_backend IS NULL") {
			t.Errorf("clause = %q, 期望补 IS NULL 分支", clause)
		}
		if len(args) != 1 {
			t.Errorf("args = %v, 期望 1 个参数", args)
		}
	})

	t.Run("空列表不生成条件", func(t *testing.T) {
		clause, args := scannerBackendClause(nil, false, 1)
		if clause != "" || args != nil {
			t.Errorf("clause = %q args = %v, 期望空", clause, args)
		}
	})
}

// buildEventWhere 里来源条件的占位符必须接在既有参数之后，
// 否则会和前面的筛选条件抢同一个 $N，导致查询参数错位。
func TestBuildEventWhereScannerBackendPlaceholderOrder(t *testing.T) {
	filter := EventFilter{
		Decision:        "critical",
		RiskLevel:       "high",
		ScannerBackends: []string{DLPScannerBackend},
	}
	clause, args := buildEventWhere(filter, 1)
	if len(args) != 3 {
		t.Fatalf("args = %v, 期望 3 个参数", args)
	}
	if args[2] != DLPScannerBackend {
		t.Errorf("args[2] = %v, 期望 %s", args[2], DLPScannerBackend)
	}
	// decision 占 $1、risk_level 占 $2，来源应落在 $3。
	if !strings.Contains(clause, "e.scanner_backend IN ($3)") {
		t.Errorf("clause = %q, 期望来源条件用 $3", clause)
	}
}

// 不传来源时 FilterHash 必须与改动前一致，否则已发出的删除确认 token 会失效。
func TestFilterHashUnchangedWithoutScannerBackends(t *testing.T) {
	filter := EventFilter{Decision: "critical", RequestID: "req-1"}
	baseline := FilterHash(filter, 100)

	// 显式传空列表应与不传等价（canonicalScannerBackends 会归一成 nil）。
	withEmpty := EventFilter{Decision: "critical", RequestID: "req-1", ScannerBackends: []string{}}
	if FilterHash(withEmpty, 100) != baseline {
		t.Error("空来源列表不应改变 FilterHash")
	}

	// 传了来源就应该算出不同的 hash，避免跨来源复用确认 token。
	withSource := EventFilter{Decision: "critical", RequestID: "req-1", ScannerBackends: []string{DLPScannerBackend}}
	if FilterHash(withSource, 100) == baseline {
		t.Error("指定来源后 FilterHash 应当变化")
	}

	// 顺序不同但内容相同的列表必须得到同一个 hash。
	first := EventFilter{ScannerBackends: []string{"a", "b"}}
	second := EventFilter{ScannerBackends: []string{"b", "a"}}
	if FilterHash(first, 1) != FilterHash(second, 1) {
		t.Error("来源列表顺序不应影响 FilterHash")
	}
}

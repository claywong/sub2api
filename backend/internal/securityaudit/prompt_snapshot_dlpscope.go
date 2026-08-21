// prompt_snapshot_dlpscope.go
// =============================================================================
// 私有扩展（不属于 upstream sub2api）：DLP 专用的扫描范围收窄。
//
// 为什么 DLP 的范围要和 qwen3guard 不同：
//
//	两者防的是不同的事。qwen3guard 防「客户端发出恶意内容」，攻击者可以伪造
//	assistant/tool 轮次来夹带 jailbreak，所以它必须扫全部角色——这是 upstream
//	clientInstructionRoles 含 assistant 的理由。
//
//	DLP 防的是「本地环境的敏感数据流出去」。数据只可能从两个口子进入请求：
//	  1. 用户自己敲进去的（role=user）
//	  2. 工具从本地读出来的（文件内容、shell 输出、数据库查询结果）
//	模型自己生成的 assistant 文本、上游服务商下发的 system prompt，都不是本地
//	数据源。把它们纳入扫描只会带来三个代价，没有收益：
//	  - 量级失控：实测 Claude Code 单请求 190 万 rune，其中绝大部分是历史轮次
//	  - 误报：190 万字符的代码上下文里必然撞上形似密钥的哈希、时间戳
//	  - 合规风险：上游的 system prompt 会明文落进 prompt_audit_events
//
// 明确不扫工具「入参」：
//
//	入参是模型生成的（要写什么文件、执行什么命令），不是本地数据源。入参里真出现
//	凭证，来源必然是用户输入或此前的工具输出，那两处已经覆盖，重复扫只增误报。
//	这也是 promptSegment.toolInput 标记存在的唯一理由。
//
// 与 upstream 合并策略：
//   - 本文件纯新增。upstream 侧仅 promptSegment 加一个字段 + 三处入参提取点各加
//     一个 `toolInput: true`，均为单行改动。
//
// =============================================================================
package securityaudit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"
)

// TrimRunesLeft 保留字符串的**尾部** limit 个 rune，超长时前面加省略号。
//
// upstream 的 TrimRunes 保留头部，用于截断过长文本；这里需要相反的方向：
// 证据的上下文窗口要的是「命中值之前紧邻的那段」，即原文的尾部。
func TrimRunesLeft(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return "…" + string(runes[len(runes)-limit:])
}

// dlpScopedSegments 过滤出 DLP 该看的片段：用户输入 + 工具输出。
//
// 保留规则：
//   - role=user：用户输入。Anthropic 的 tool_result 嵌在 user 消息里，一并保留。
//   - role=tool/function 且非 toolInput：工具输出。
//
// 丢弃 system / developer / assistant / model，以及全部工具入参。
func dlpScopedSegments(values []promptSegment) []promptSegment {
	result := make([]promptSegment, 0, len(values))
	for _, segment := range values {
		if segment.toolInput {
			continue
		}
		role := strings.ToLower(strings.TrimSpace(segment.role))
		switch {
		case role == "user" || segment.user:
			result = append(result, segment)
		case role == "tool" || role == "function":
			result = append(result, segment)
		}
	}
	return result
}

// ExtractDLPSnapshot 构建 DLP 专用快照，扫描与留存范围都收窄到
// 「用户输入 + 工具输出」。
//
// 刻意不复用 ExtractBlockingPromptSnapshot：那条路径的范围由 upstream 的角色
// 白名单决定，改它会连带影响 qwen3guard。这里直接从 extractProtocolSegments
// 取原始片段再过滤，两条路径互不干扰。
//
// 返回的 ScanText 与 FullPrompt 同源，差别仅在 FullPrompt 受 maxRunes 截断——
// 收窄后正常请求远低于上限，命中片段基本都能在留存文本里定位到。
func ExtractDLPSnapshot(req Request) (PromptSnapshot, error) {
	var document any
	if err := json.Unmarshal(req.Body, &document); err != nil {
		return PromptSnapshot{}, errors.New("prompt audit request JSON is invalid")
	}
	scoped := dlpScopedSegments(extractProtocolSegments(req.Protocol, document))
	segments := normalizeSegmentsLatestUserFirst(scoped)
	if len(segments) == 0 {
		return PromptSnapshot{}, ErrNoPromptText
	}
	scanText, metadataText := buildPrioritizedScanText(segments)
	digest := sha256.Sum256([]byte(metadataText))
	stage := strings.TrimSpace(req.Stage)
	if stage == "" {
		stage = "http"
	}
	return PromptSnapshot{
		RequestID: req.RequestID, UserID: req.UserID, UsernameSnapshot: req.Username,
		UserEmailSnapshot: req.UserEmail, APIKeyID: req.APIKeyID, APIKeyNameSnapshot: req.APIKeyName,
		GroupID: cloneInt64Ptr(req.GroupID), GroupName: req.GroupName, Provider: req.Provider,
		Endpoint: req.Endpoint, Protocol: req.Protocol, Model: req.Model,
		PromptHash:      hex.EncodeToString(digest[:]),
		RedactedPreview: BuildPromptPreview(metadataText, DefaultPromptPreviewMaxRunes),
		FullPrompt:      BuildFullPrompt(metadataText, DefaultFullPromptMaxRunes),
		PromptLength:    utf8.RuneCountInString(metadataText),
		MessageCount:    len(segments), Stage: stage,
		ScanText: scanText,
	}, nil
}

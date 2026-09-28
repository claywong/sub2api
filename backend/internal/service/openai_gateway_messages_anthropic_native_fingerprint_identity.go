// 私有扩展（不属于 upstream sub2api）。
//
// 智谱账号指纹归一化的「客户端身份头」：
//   - claudecode：x-stainless-* 规范值。package-version / runtime-version 随 CLI 版本变化，
//     从 UA 版本等于当前生效版本的真实 claude-cli 入站请求中学习配对值，未学到时回退内置抓包值；
//     lang / os / arch / runtime 是设备属性，固定为账号级常量。
//   - zcode：官方 ZCode 客户端的 X-ZCode-* 身份头。出站白名单会丢弃这组头，
//     归一化时按官方抓包补齐，否则上游看到「UA 是 ZCode 但没有任何 ZCode 身份头」。
//
// @author wangzhong
package service

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"

	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// Claude Code x-stainless 规范值。版本类的兜底值取自真实 claude-cli 2.1.270 抓包
// （pi-cc-compat 逐字节比对，2026-09）；升级兜底值时同步更新本注释。
const (
	claudeCodeStainlessLang                 = "js"
	claudeCodeStainlessOS                   = "Linux"
	claudeCodeStainlessArch                 = "arm64"
	claudeCodeStainlessRuntime              = "node"
	claudeCodeStainlessFallbackPackageVer   = "0.112.1"
	claudeCodeStainlessFallbackRuntimeVer   = "v26.3.0"
	claudeCodeStainlessPackageVersionHeader = "x-stainless-package-version"
	claudeCodeStainlessRuntimeVersionHeader = "x-stainless-runtime-version"
)

var (
	// stainlessPackageVersionRe / stainlessRuntimeVersionRe 限定学习值的形态，
	// 防止入站伪造的畸形值被学成规范值。
	stainlessPackageVersionRe = regexp.MustCompile(`^\d+\.\d+\.\d+$`)
	stainlessRuntimeVersionRe = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)
)

// claudeCodeStainlessVersions 与某个 CLI 版本配对的 SDK / 运行时版本。
type claudeCodeStainlessVersions struct {
	cliVersion     string
	packageVersion string
	runtimeVersion string
}

var (
	claudeCodeStainlessLearnedMu sync.RWMutex
	claudeCodeStainlessLearned   claudeCodeStainlessVersions
)

// learnClaudeCodeStainlessVersions 从真实 claude-cli 入站请求学习与生效 CLI 版本配对的
// package / runtime 版本。只接受 UA 版本严格等于生效版本、且两个值都形态合法的请求。
func learnClaudeCodeStainlessVersions(inboundUA string, h http.Header) {
	effective := claude.EffectiveCLIVersion()
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(inboundUA)), claudeCLIUserAgentPrefix+effective+" ") {
		return
	}
	pkg := strings.TrimSpace(getHeaderRaw(h, claudeCodeStainlessPackageVersionHeader))
	runtime := strings.TrimSpace(getHeaderRaw(h, claudeCodeStainlessRuntimeVersionHeader))
	if !stainlessPackageVersionRe.MatchString(pkg) || !stainlessRuntimeVersionRe.MatchString(runtime) {
		return
	}
	next := claudeCodeStainlessVersions{cliVersion: effective, packageVersion: pkg, runtimeVersion: runtime}
	claudeCodeStainlessLearnedMu.RLock()
	same := claudeCodeStainlessLearned == next
	claudeCodeStainlessLearnedMu.RUnlock()
	if same {
		return
	}
	claudeCodeStainlessLearnedMu.Lock()
	claudeCodeStainlessLearned = next
	claudeCodeStainlessLearnedMu.Unlock()
}

// claudeCodeCanonicalStainlessHeaders 返回当前生效的 x-stainless-* 规范值（小写键）。
// 学习值只在其配对的 CLI 版本仍是生效版本时使用，版本切换后自动回退兜底值直到重新学到。
func claudeCodeCanonicalStainlessHeaders() map[string]string {
	pkg, runtime := claudeCodeStainlessFallbackPackageVer, claudeCodeStainlessFallbackRuntimeVer
	claudeCodeStainlessLearnedMu.RLock()
	learned := claudeCodeStainlessLearned
	claudeCodeStainlessLearnedMu.RUnlock()
	if learned.cliVersion != "" && learned.cliVersion == claude.EffectiveCLIVersion() {
		pkg, runtime = learned.packageVersion, learned.runtimeVersion
	}
	return map[string]string{
		"x-stainless-lang":                      claudeCodeStainlessLang,
		claudeCodeStainlessPackageVersionHeader: pkg,
		"x-stainless-os":                        claudeCodeStainlessOS,
		"x-stainless-arch":                      claudeCodeStainlessArch,
		"x-stainless-runtime":                   claudeCodeStainlessRuntime,
		claudeCodeStainlessRuntimeVersionHeader: runtime,
	}
}

// ZCode 身份头规范值，取自官方 ZCode 抓包（pi-zcode-headers，桌面版 3.10.2，2026-09）。
// 设备 / 地域类取账号级固定值，使拼车用户在供应商侧收敛为同一台设备。
const (
	zcodeReferer             = "https://zcode.z.ai"
	zcodeTitleElectron       = "Z Code@electron"
	zcodeTitleCLI            = "Z Code@cli"
	zcodeAgent               = "glm"
	zcodeSessionType         = "main"
	zcodeReleaseChannel      = "production"
	zcodeCanonicalPlatform   = "darwin-arm64"
	zcodeCanonicalOSCategory = "macos"
	zcodeCanonicalOSVersion  = "24.6.0"
	zcodeCanonicalLanguage   = "zh-CN"
	zcodeCanonicalTimezone   = "Asia/Shanghai"
)

// zcodeRequestIDHeaders 每次请求一个新值的 ID 头；入站带了就沿用（与客户端自身链路一致）。
var zcodeRequestIDHeaders = []string{"X-Query-Id", "X-Request-Id", "X-ZCode-Trace-Id"}

// zcodeCanonicalIdentityHeaders 返回 ZCode 规范身份头（不含会话 / 请求级 ID）。
// X-Title 保留入站的桌面 / CLI 身份（与客户端请求体形态自洽），缺失或非法时取桌面身份。
func zcodeCanonicalIdentityHeaders(inbound http.Header) map[string]string {
	title := zcodeTitleElectron
	if inbound != nil && strings.TrimSpace(inbound.Get("X-Title")) == zcodeTitleCLI {
		title = zcodeTitleCLI
	}
	return map[string]string{
		"HTTP-Referer":         zcodeReferer,
		"X-Title":              title,
		"X-ZCode-App-Version":  zcodeCanonicalVersion,
		"X-ZCode-Agent":        zcodeAgent,
		"X-ZCode-Session-Type": zcodeSessionType,
		"X-Platform":           zcodeCanonicalPlatform,
		"X-Os-Category":        zcodeCanonicalOSCategory,
		"X-Os-Version":         zcodeCanonicalOSVersion,
		"X-Client-Language":    zcodeCanonicalLanguage,
		"X-Client-Timezone":    zcodeCanonicalTimezone,
		"X-Release-Channel":    zcodeReleaseChannel,
	}
}

// zcodeSessionIDFor 返回出站 X-Session-Id：入站带了就沿用（会话是自然行为，保留），
// 否则由账号 ID 确定性派生，避免每个请求都像一个新会话。
func zcodeSessionIDFor(account *Account, inbound http.Header) string {
	if inbound != nil {
		if v := strings.TrimSpace(inbound.Get("X-Session-Id")); v != "" {
			return v
		}
	}
	return deriveStableUUIDv4(fmt.Sprintf("sub2api:zcode-session:v1:%d", account.ID))
}

// applyZCodeIdentityHeaders 在 zcode 目标下补齐 ZCode 身份头；其他目标 no-op。
// inbound 为客户端原始请求头（出站白名单已丢弃这组头，需从入站取会话 / 请求 ID）。
// 账号级显式覆写的键保留管理员值。
func applyZCodeIdentityHeaders(account *Account, h http.Header, mode anthropicFingerprintNormalizeMode, inbound http.Header) {
	if account == nil || h == nil || mode != anthropicFingerprintZCode {
		return
	}
	set := func(name, value string) {
		if _, overridden := account.HeaderOverrideValue(strings.ToLower(name)); overridden || value == "" {
			return
		}
		deleteHeaderAllForms(h, name)
		h.Set(name, value)
	}
	for name, value := range zcodeCanonicalIdentityHeaders(inbound) {
		set(name, value)
	}
	set("X-Session-Id", zcodeSessionIDFor(account, inbound))
	for _, name := range zcodeRequestIDHeaders {
		value := ""
		if inbound != nil {
			value = strings.TrimSpace(inbound.Get(name))
		}
		if value == "" {
			value = uuid.NewString()
		}
		set(name, value)
	}
}

// inboundRequestHeaders 返回入站请求头；c 为空时返回 nil。
func inboundRequestHeaders(c *gin.Context) http.Header {
	if c == nil || c.Request == nil {
		return nil
	}
	return c.Request.Header
}

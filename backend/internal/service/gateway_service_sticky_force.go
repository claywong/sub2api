package service

// 私有扩展（不属于 upstream sub2api）
//
// 所含内容：
//   - 常量 stickyBypassConcurrency
//   - 函数 stickySlotConcurrency
//
// 背景：粘性会话命中某账号后，upstream 仍要抢该账号的并发槽位（Concurrency 上限），
// 槽位满会排队甚至降级换号，破坏会话亲和（prompt cache 失效）。本 fork 让粘性命中
// 豁免并发上限——「计数但不限流」。
//
// 实现：不改并发服务底层。AcquireAccountSlot 的 Lua 逻辑是 `count < maxConcurrency 才放行`，
// 且放行时本就 ZADD 计数。因此粘性抢槽位时把 maxConcurrency 传一个极大值即可：
//   - count < (1<<30) 永远成立 → 永远放行；
//   - 仍 ZADD → 并发计数 / LoadRate 准确（LoadRate 用账号真实 Concurrency 计算，与此处无关）；
//   - ReleaseFunc 照常释放。
//
// 会话数量控制（max_sessions / checkAndRegisterSession）不受影响，仍然生效。
//
// merge 策略：纯增量。调用点改动见 gateway_service.go 中三处粘性抢槽位处
// （搜索 stickySlotConcurrency）。回退只需把这三处改回传 account.Concurrency。
//
// @author wangzhong

// stickyBypassConcurrency 是粘性会话抢并发槽位时使用的「无上限」哨兵值。
// 取 1<<30（约 10.7 亿）：实际等同无上限，又远离 int 溢出边界。
const stickyBypassConcurrency = 1 << 30

// stickySlotConcurrency 返回粘性会话抢并发槽位时应传给 AcquireAccountSlot 的上限。
//   - 账号有并发限制（c > 0）：返回极大值，使其永远放行但仍计数（计数但不限流）；
//   - 账号无并发限制（c <= 0）：保持原值，沿用 upstream 的 no-op 不计数语义。
func stickySlotConcurrency(c int) int {
	if c > 0 {
		return stickyBypassConcurrency
	}
	return c
}

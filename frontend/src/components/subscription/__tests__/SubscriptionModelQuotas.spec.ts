import { flushPromises, mount } from "@vue/test-utils";
import { beforeEach, describe, expect, it, vi } from "vitest";

import SubscriptionModelQuotas from "../SubscriptionModelQuotas.vue";
import type { ModelQuotaUsageProgress } from "@/types";

const getSubscriptionModelQuotaUsage = vi.fn();

vi.mock("@/api/subscriptions", () => ({
  default: {
    get getSubscriptionModelQuotaUsage() {
      return getSubscriptionModelQuotaUsage;
    },
  },
}));

vi.mock("vue-i18n", async () => {
  const actual = await vi.importActual<typeof import("vue-i18n")>("vue-i18n");
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string, params?: Record<string, unknown>) =>
        params ? `${key}:${JSON.stringify(params)}` : key,
    }),
  };
});

function window(overrides: Partial<{ limit: number; used: number; pct: number }> = {}) {
  const limit = overrides.limit ?? 100;
  const used = overrides.used ?? 25;
  return {
    limit_usd: limit,
    used_usd: used,
    remaining_usd: Math.max(limit - used, 0),
    percentage: overrides.pct ?? (limit > 0 ? (used / limit) * 100 : 100),
    // 固定在未来，避免用例受当前时间影响
    resets_at: new Date(Date.now() + 3 * 3600 * 1000).toISOString(),
    resets_in_seconds: 3 * 3600,
  };
}

async function mountWith(items: ModelQuotaUsageProgress[]) {
  getSubscriptionModelQuotaUsage.mockResolvedValueOnce(items);
  const wrapper = mount(SubscriptionModelQuotas, { props: { subscriptionId: 7 } });
  await flushPromises();
  return wrapper;
}

beforeEach(() => {
  getSubscriptionModelQuotaUsage.mockReset();
});

describe("SubscriptionModelQuotas", () => {
  it("分组未配置按模型额度时整块不渲染", async () => {
    const wrapper = await mountWith([]);
    expect(wrapper.text()).toBe("");
  });

  it("加载后 emit 规则数，供父级抑制无限制徽章", async () => {
    const wrapper = await mountWith([
      { match: "claude-opus*", rule_key: "claude-opus*", daily: window() },
    ]);
    expect(wrapper.emitted("loaded")?.[0]).toEqual([1]);
  });

  it("加载失败时按无规则处理，不把错误抛给父级", async () => {
    getSubscriptionModelQuotaUsage.mockRejectedValueOnce(new Error("boom"));
    const spy = vi.spyOn(console, "error").mockImplementation(() => {});
    const wrapper = mount(SubscriptionModelQuotas, { props: { subscriptionId: 7 } });
    await flushPromises();

    expect(wrapper.text()).toBe("");
    expect(wrapper.emitted("loaded")?.[0]).toEqual([0]);
    spy.mockRestore();
  });

  it("展开后按规则列出各窗口的已用/上限", async () => {
    const wrapper = await mountWith([
      {
        match: "gpt-6-astra",
        rule_key: "gpt-6-astra",
        daily: window({ limit: 20, used: 5 }),
        monthly: window({ limit: 200, used: 50 }),
      },
    ]);

    await wrapper.get("button").trigger("click");
    const text = wrapper.text();
    expect(text).toContain("gpt-6-astra");
    expect(text).toContain("$5.00");
    expect(text).toContain("$20.00");
    expect(text).toContain("$50.00");
    expect(text).toContain("$200.00");
  });

  it("上限全为 0 的规则展示为已禁用，不画进度条", async () => {
    const wrapper = await mountWith([
      { match: "claude-opus-4-5", rule_key: "claude-opus-4-5", daily: window({ limit: 0, used: 0 }) },
    ]);

    await wrapper.get("button").trigger("click");
    expect(wrapper.text()).toContain("userSubscriptions.modelQuota.disabled");
    expect(wrapper.text()).toContain("userSubscriptions.modelQuota.disabledHint");
  });

  it("日 0 月有额度的混配规则仍逐窗口展示，不整条按禁用处理", async () => {
    const wrapper = await mountWith([
      {
        match: "claude-opus*",
        rule_key: "claude-opus*",
        daily: window({ limit: 0, used: 0 }),
        monthly: window({ limit: 300, used: 12 }),
      },
    ]);

    await wrapper.get("button").trigger("click");
    expect(wrapper.text()).not.toContain("userSubscriptions.modelQuota.disabledHint");
    expect(wrapper.text()).toContain("$300.00");
  });

  it("前缀规则标注共享额度，精确规则不标注", async () => {
    const prefix = await mountWith([
      { match: "claude-opus*", rule_key: "claude-opus*", daily: window() },
    ]);
    await prefix.get("button").trigger("click");
    expect(prefix.text()).toContain("userSubscriptions.modelQuota.prefixHint");

    const exact = await mountWith([
      { match: "gpt-6-astra", rule_key: "gpt-6-astra", daily: window() },
    ]);
    await exact.get("button").trigger("click");
    expect(exact.text()).not.toContain("userSubscriptions.modelQuota.prefixHint");
  });

  it("额度用尽时提示已用尽", async () => {
    const wrapper = await mountWith([
      { match: "gpt-6-astra", rule_key: "gpt-6-astra", daily: window({ limit: 10, used: 10 }) },
    ]);

    await wrapper.get("button").trigger("click");
    expect(wrapper.text()).toContain("userSubscriptions.modelQuota.exhausted");
  });
});

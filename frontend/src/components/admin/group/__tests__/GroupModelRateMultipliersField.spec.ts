import { mount } from "@vue/test-utils";
import { defineComponent, h, ref } from "vue";
import { describe, expect, it, vi } from "vitest";

import GroupModelRateMultipliersField from "../GroupModelRateMultipliersField.vue";
import type { ModelRateMultipliers } from "@/types";

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

const stubs = {
  Icon: { template: "<span />" },
  Toggle: {
    props: ["modelValue"],
    emits: ["update:modelValue"],
    template:
      '<button data-testid="toggle" @click="$emit(\'update:modelValue\', !modelValue)" />',
  },
};

function mountField(modelValue?: ModelRateMultipliers | null, groupRate: number | null = 1.5) {
  return mount(GroupModelRateMultipliersField, {
    props: { modelValue: modelValue ?? null, groupRate },
    global: { stubs },
  });
}

function lastEmitted(wrapper: ReturnType<typeof mountField>): ModelRateMultipliers | undefined {
  const events = wrapper.emitted("update:modelValue");
  if (!events || events.length === 0) return undefined;
  return events[events.length - 1][0] as ModelRateMultipliers;
}

function errorText(wrapper: ReturnType<typeof mountField>): string {
  return wrapper.get('[data-testid="model-rate-error"]').text();
}

describe("GroupModelRateMultipliersField", () => {
  it("renders disabled state without the rules table", () => {
    const wrapper = mountField();
    expect(wrapper.find('[data-testid="model-rate-add"]').exists()).toBe(false);
  });

  it("hydrates rules and shows the effective rate as group rate × factor", () => {
    const wrapper = mountField({ enabled: true, rules: [{ match: "claude-opus*", multiplier: 2 }] });
    expect((wrapper.get('[data-testid="model-rate-match-0"]').element as HTMLInputElement).value).toBe("claude-opus*");
    expect((wrapper.get('[data-testid="model-rate-multiplier-0"]').element as HTMLInputElement).value).toBe("2");
    expect(wrapper.get('[data-testid="model-rate-effective-0"]').text()).toBe("1.50 × 2.00 = 3.00x");
  });

  it("updates the effective rate when the group rate changes", async () => {
    const wrapper = mountField({ enabled: true, rules: [{ match: "claude-opus*", multiplier: 2 }] });
    await wrapper.setProps({ groupRate: 0.8 });
    expect(wrapper.get('[data-testid="model-rate-effective-0"]').text()).toBe("0.80 × 2.00 = 1.60x");
  });

  it("shows a dash when the factor is missing", async () => {
    const wrapper = mountField({ enabled: true, rules: [{ match: "claude-opus*", multiplier: 2 }] });
    await wrapper.get('[data-testid="model-rate-multiplier-0"]').setValue("");
    expect(wrapper.get('[data-testid="model-rate-effective-0"]').text()).toBe("-");
  });

  it("emits trimmed match and numeric factor", async () => {
    const wrapper = mountField({ enabled: true, rules: [] });
    await wrapper.get('[data-testid="model-rate-add"]').trigger("click");
    await wrapper.get('[data-testid="model-rate-match-0"]').setValue("  gpt-6-astra  ");
    await wrapper.get('[data-testid="model-rate-multiplier-0"]').setValue("1.25");
    expect(lastEmitted(wrapper)).toEqual({
      enabled: true,
      rules: [{ match: "gpt-6-astra", multiplier: 1.25 }],
    });
  });

  it("submits 0 for a cleared factor so the backend rejects it", async () => {
    const wrapper = mountField({ enabled: true, rules: [{ match: "claude-opus*", multiplier: 2 }] });
    await wrapper.get('[data-testid="model-rate-multiplier-0"]').setValue("");
    expect(lastEmitted(wrapper)?.rules[0].multiplier).toBe(0);
    expect(errorText(wrapper)).toContain("invalidMultiplier");
  });

  it("removes rules", async () => {
    const wrapper = mountField({ enabled: true, rules: [{ match: "claude-opus*", multiplier: 2 }] });
    await wrapper.get('[data-testid="model-rate-remove-0"]').trigger("click");
    expect(wrapper.find('[data-testid="model-rate-match-0"]').exists()).toBe(false);
  });

  it.each([
    ["emptyMatch", { match: "  ", multiplier: 2 }],
    ["bareWildcard", { match: "*", multiplier: 2 }],
    ["wildcardPosition", { match: "claude-*-opus", multiplier: 2 }],
    ["invalidMultiplier", { match: "claude-opus*", multiplier: 0 }],
    ["invalidMultiplier", { match: "claude-opus*", multiplier: -1 }],
  ])("flags %s", async (expected, rule) => {
    const wrapper = mountField({ enabled: true, rules: [rule] });
    await wrapper.vm.$nextTick();
    expect(errorText(wrapper)).toContain(expected);
  });

  it("flags duplicate rules case-insensitively", async () => {
    const wrapper = mountField({
      enabled: true,
      rules: [
        { match: "claude-opus*", multiplier: 2 },
        { match: "Claude-Opus*", multiplier: 3 },
      ],
    });
    await wrapper.vm.$nextTick();
    expect(errorText(wrapper)).toContain("duplicate");
  });

  it("flags an enabled config with no rules", async () => {
    const wrapper = mountField({ enabled: true, rules: [] });
    await wrapper.vm.$nextTick();
    expect(errorText(wrapper)).toContain("enabledButEmpty");
  });

  it("accepts a valid configuration without error", async () => {
    const wrapper = mountField({
      enabled: true,
      rules: [
        { match: "claude-opus*", multiplier: 2 },
        { match: "gpt-6-astra", multiplier: 1.5 },
      ],
    });
    await wrapper.vm.$nextTick();
    expect(wrapper.find('[data-testid="model-rate-error"]').exists()).toBe(false);
  });

  // 回归：v-model 闭环回流时内容比较必须收敛，不能 emit → 回灌 → 再 emit 无限循环。
  it("does not loop when the parent echoes emitted values back (v-model)", async () => {
    const state = ref<ModelRateMultipliers>({
      enabled: true,
      rules: [{ match: "claude-opus*", multiplier: 2 }],
    });
    const host = mount(
      defineComponent({
        setup() {
          return () =>
            h(GroupModelRateMultipliersField, {
              modelValue: state.value,
              groupRate: 1.5,
              "onUpdate:modelValue": (v: ModelRateMultipliers) => {
                state.value = v;
              },
            });
        },
      }),
      { global: { stubs } },
    );

    await host.get('[data-testid="model-rate-multiplier-0"]').setValue("3");
    await host.vm.$nextTick();
    await host.vm.$nextTick();

    const before = host.emitted("update:modelValue")?.length ?? 0;
    await host.vm.$nextTick();
    await host.vm.$nextTick();
    expect(host.emitted("update:modelValue")?.length ?? 0).toBe(before);
    expect(state.value.rules[0].multiplier).toBe(3);
  });
});

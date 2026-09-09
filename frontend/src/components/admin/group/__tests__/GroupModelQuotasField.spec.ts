import { mount } from "@vue/test-utils";
import { defineComponent, h, ref } from "vue";
import { describe, expect, it, vi } from "vitest";

import GroupModelQuotasField from "../GroupModelQuotasField.vue";
import type { ModelQuotas } from "@/types";

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

function mountField(modelValue?: ModelQuotas | null) {
  return mount(GroupModelQuotasField, {
    props: { modelValue: modelValue ?? null },
    global: {
      stubs: {
        Icon: { template: "<span />" },
        Toggle: {
          props: ["modelValue"],
          emits: ["update:modelValue"],
          template:
            '<button data-testid="toggle" @click="$emit(\'update:modelValue\', !modelValue)" />',
        },
      },
    },
  });
}

function lastEmitted(wrapper: ReturnType<typeof mountField>): ModelQuotas | undefined {
  const events = wrapper.emitted("update:modelValue");
  if (!events || events.length === 0) return undefined;
  return events[events.length - 1][0] as ModelQuotas;
}

describe("GroupModelQuotasField", () => {
  it("renders disabled state without the rules table", () => {
    const wrapper = mountField();
    expect(wrapper.find('[data-testid="model-quota-add"]').exists()).toBe(false);
  });

  it("hydrates existing rules from the model value", () => {
    const wrapper = mountField({
      enabled: true,
      rules: [
        { match: "claude-opus*", daily: 50, weekly: 200, monthly: null },
        { match: "gpt-6-astra", daily: 10 },
      ],
    });
    const matchInputs = wrapper.findAll('input[type="text"]');
    expect(matchInputs).toHaveLength(2);
    expect((matchInputs[0].element as HTMLInputElement).value).toBe("claude-opus*");
    expect((matchInputs[1].element as HTMLInputElement).value).toBe("gpt-6-astra");
  });

  it("distinguishes an unset limit from an explicit zero", async () => {
    const wrapper = mountField({
      enabled: true,
      rules: [{ match: "gpt-6-astra", daily: 0, weekly: null }],
    });
    const numberInputs = wrapper.findAll('input[type="number"]');
    // daily=0 → 输入框显示 "0"；weekly=null → 空串（占位符提示不限制）
    expect((numberInputs[0].element as HTMLInputElement).value).toBe("0");
    expect((numberInputs[1].element as HTMLInputElement).value).toBe("");

    // 触发一次真实编辑后再断言回写：组件在挂载时不 emit，避免无用户操作就改脏表单
    await numberInputs[2].setValue("30");
    const emitted = lastEmitted(wrapper);
    expect(emitted?.rules[0].daily).toBe(0);
    expect(emitted?.rules[0].weekly).toBeNull();
    expect(emitted?.rules[0].monthly).toBe(30);
  });

  it("clearing a limit input turns it back into no-limit", async () => {
    const wrapper = mountField({
      enabled: true,
      rules: [{ match: "gpt-6-astra", daily: 10 }],
    });
    await wrapper.findAll('input[type="number"]')[0].setValue("");
    expect(lastEmitted(wrapper)?.rules[0].daily).toBeNull();
  });

  it("adds and removes rules", async () => {
    const wrapper = mountField({ enabled: true, rules: [] });
    await wrapper.get('[data-testid="model-quota-add"]').trigger("click");
    expect(wrapper.findAll('input[type="text"]')).toHaveLength(1);

    await wrapper.get('[data-testid="model-quota-remove-0"]').trigger("click");
    expect(wrapper.findAll('input[type="text"]')).toHaveLength(0);
  });

  it("emits trimmed match values", async () => {
    const wrapper = mountField({ enabled: true, rules: [{ match: "", daily: null }] });
    await wrapper.get('input[type="text"]').setValue("  claude-opus*  ");
    expect(lastEmitted(wrapper)?.rules[0].match).toBe("claude-opus*");
  });

  it("flags a bare wildcard", async () => {
    const wrapper = mountField({ enabled: true, rules: [{ match: "*", daily: 1 }] });
    await wrapper.vm.$nextTick();
    expect(wrapper.get('[data-testid="model-quota-error"]').text()).toContain("bareWildcard");
  });

  it("flags a wildcard that is not at the end", async () => {
    const wrapper = mountField({
      enabled: true,
      rules: [{ match: "claude-*-opus", daily: 1 }],
    });
    await wrapper.vm.$nextTick();
    expect(wrapper.get('[data-testid="model-quota-error"]').text()).toContain("wildcardPosition");
  });

  it("flags duplicate rules case-insensitively", async () => {
    const wrapper = mountField({
      enabled: true,
      rules: [
        { match: "claude-opus*", daily: 1 },
        { match: "Claude-Opus*", daily: 2 },
      ],
    });
    await wrapper.vm.$nextTick();
    expect(wrapper.get('[data-testid="model-quota-error"]').text()).toContain("duplicate");
  });

  it("flags an empty match", async () => {
    const wrapper = mountField({ enabled: true, rules: [{ match: "  ", daily: 1 }] });
    await wrapper.vm.$nextTick();
    expect(wrapper.get('[data-testid="model-quota-error"]').text()).toContain("emptyMatch");
  });

  it("flags a negative limit", async () => {
    const wrapper = mountField({ enabled: true, rules: [{ match: "gpt-6-astra", daily: 1 }] });
    await wrapper.findAll('input[type="number"]')[0].setValue("-5");
    await wrapper.vm.$nextTick();
    expect(wrapper.get('[data-testid="model-quota-error"]').text()).toContain("negativeLimit");
  });

  it("flags an enabled config with no rules", async () => {
    const wrapper = mountField({ enabled: true, rules: [] });
    await wrapper.vm.$nextTick();
    expect(wrapper.get('[data-testid="model-quota-error"]').text()).toContain("enabledButEmpty");
  });

  it("accepts a valid configuration without error", async () => {
    const wrapper = mountField({
      enabled: true,
      rules: [
        { match: "claude-opus*", daily: 50 },
        { match: "gpt-6-astra", daily: 10 },
      ],
    });
    await wrapper.vm.$nextTick();
    expect(wrapper.find('[data-testid="model-quota-error"]').exists()).toBe(false);
  });

  // 回归：真实使用是 v-model 闭环（父组件把 emit 的对象原样回流为 modelValue）。
  // 旧实现用引用相等判断外部回灌，回流对象引用必变 → syncFromProps 重置内部状态
  // → deep watch 再 emit → 无限循环。本测试模拟 v-model 回流，断言 emit 收敛。
  it("does not loop when the parent echoes emitted values back (v-model)", async () => {
    const state = ref<ModelQuotas>({
      enabled: true,
      rules: [{ match: "claude-opus*", daily: 50 }],
    });
    const host = mount(
      defineComponent({
        setup() {
          return () =>
            h(GroupModelQuotasField, {
              modelValue: state.value,
              "onUpdate:modelValue": (v: ModelQuotas) => {
                state.value = v;
              },
            });
        },
      }),
      {
        global: {
          stubs: {
            Icon: { template: "<span />" },
            Toggle: {
              props: ["modelValue"],
              emits: ["update:modelValue"],
              template: "<button data-testid='toggle' @click=\"$emit('update:modelValue', !modelValue)\" />",
            },
          },
        },
      },
    );

    // 用户编辑：触发 emit → 父回流 → 若误判为外部回灌会再 emit
    await host.findAll('input[type="number"]')[1].setValue("200");
    await host.vm.$nextTick();
    await host.vm.$nextTick();

    const before = host.emitted("update:modelValue")?.length ?? 0;
    await host.vm.$nextTick();
    await host.vm.$nextTick();
    await host.vm.$nextTick();
    const after = host.emitted("update:modelValue")?.length ?? 0;

    expect(after).toBe(before);
    expect(state.value.rules[0].weekly).toBe(200);
  });
});

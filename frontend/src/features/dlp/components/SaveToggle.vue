<!--
  SaveToggle.vue
  ============================================================================
  私有扩展（不属于 upstream sub2api）：底部保存栏里的开关控件。

  与 upstream PromptAuditView.vue 内联的 SaveToggle 视觉一致，但刻意复制一份而不是
  把那个内联组件抽出来共享：抽取要改 upstream 文件，而 features/prompt-audit 目前
  是 0 diff，为了省 30 行去换一处永久的 merge 冲突点不值得。

  与 upstream 合并策略：
    - 本文件是私有新增。upstream 若将其内联组件抽成公共组件，可改为直接引用并删除本文件。
  ============================================================================
-->
<template>
  <label
    class="flex items-center gap-2.5 text-sm"
    :class="disabled ? 'cursor-not-allowed opacity-50' : 'cursor-pointer'"
  >
    <button
      v-bind="$attrs"
      type="button"
      role="switch"
      :aria-checked="modelValue"
      :aria-label="label"
      :disabled="disabled"
      class="relative inline-flex h-6 w-11 shrink-0 items-center rounded-full border-2 border-transparent transition-colors duration-200 focus:outline-none focus-visible:ring-2 focus-visible:ring-primary-500 focus-visible:ring-offset-2"
      :class="[
        modelValue ? 'bg-primary-600' : 'bg-gray-300 dark:bg-dark-600',
        disabled ? 'cursor-not-allowed' : 'cursor-pointer',
      ]"
      @click.prevent="toggle"
    >
      <span
        class="pointer-events-none inline-block h-5 w-5 rounded-full bg-white shadow transition-transform duration-200 ease-in-out"
        :class="modelValue ? 'translate-x-5' : 'translate-x-0'"
      />
    </button>
    <span class="select-none text-gray-700 dark:text-dark-200">{{ label }}</span>
  </label>
</template>

<script setup lang="ts">
// 外部传入的 data-test / aria-* 要落在真正的开关元素上，
// 默认会落到根 label，用它测不到 disabled 与 aria-checked。
defineOptions({ inheritAttrs: false })

const props = withDefaults(
  defineProps<{ label: string; modelValue: boolean; disabled?: boolean }>(),
  { disabled: false },
)
const emit = defineEmits<{ (event: 'update:modelValue', value: boolean): void }>()

function toggle() {
  if (props.disabled) return
  emit('update:modelValue', !props.modelValue)
}
</script>

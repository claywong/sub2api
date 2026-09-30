<template>
  <div class="space-y-3">
    <div class="flex items-start justify-between gap-3">
      <div>
        <label class="block text-sm font-medium text-gray-700 dark:text-gray-300">
          {{ t('admin.groups.modelRateMultipliers.title') }}
        </label>
        <p class="mt-1 text-xs text-gray-500 dark:text-gray-400">
          {{ t('admin.groups.modelRateMultipliers.hint') }}
        </p>
      </div>
      <Toggle v-model="enabled" data-testid="model-rate-enabled" />
    </div>

    <div v-if="enabled" class="space-y-3">
      <div
        v-if="rules.length === 0"
        class="rounded-xl border border-dashed border-gray-300 px-4 py-6 text-center text-sm text-gray-500 dark:border-dark-600 dark:text-gray-400"
      >
        {{ t('admin.groups.modelRateMultipliers.empty') }}
      </div>

      <div v-else class="overflow-x-auto">
        <table class="min-w-full text-sm">
          <thead>
            <tr
              class="border-b border-gray-200 text-gray-700 dark:border-dark-700 dark:text-gray-300"
            >
              <th class="px-2 py-2 text-left font-medium">
                {{ t('admin.groups.modelRateMultipliers.columns.match') }}
              </th>
              <th class="px-2 py-2 text-left font-medium">
                {{ t('admin.groups.modelRateMultipliers.columns.multiplier') }}
              </th>
              <th class="px-2 py-2 text-left font-medium">
                {{ t('admin.groups.modelRateMultipliers.columns.effective') }}
              </th>
              <th class="w-10 px-2 py-2"></th>
            </tr>
          </thead>
          <tbody>
            <tr
              v-for="(rule, index) in rules"
              :key="index"
              class="border-b border-gray-100 dark:border-dark-800"
            >
              <td class="px-2 py-2">
                <input
                  v-model="rule.match"
                  type="text"
                  class="input w-full min-w-[12rem] font-mono"
                  :placeholder="t('admin.groups.modelRateMultipliers.matchPlaceholder')"
                  :aria-label="t('admin.groups.modelRateMultipliers.columns.match')"
                  :data-testid="`model-rate-match-${index}`"
                />
              </td>
              <td class="px-2 py-2">
                <input
                  v-model="multiplierInputs[index]"
                  type="number"
                  min="0"
                  step="0.001"
                  class="input w-24"
                  :aria-label="t('admin.groups.modelRateMultipliers.columns.multiplier')"
                  :data-testid="`model-rate-multiplier-${index}`"
                />
              </td>
              <td
                class="px-2 py-2 font-mono text-gray-700 dark:text-gray-300"
                :data-testid="`model-rate-effective-${index}`"
              >
                {{ effectiveLabel(index) }}
              </td>
              <td class="px-2 py-2 text-right">
                <button
                  type="button"
                  class="text-gray-400 transition-colors hover:text-red-500"
                  :title="t('common.delete')"
                  :aria-label="t('common.delete')"
                  :data-testid="`model-rate-remove-${index}`"
                  @click="removeRule(index)"
                >
                  <Icon name="trash" size="sm" />
                </button>
              </td>
            </tr>
          </tbody>
        </table>
      </div>

      <button
        type="button"
        class="btn-secondary text-sm"
        data-testid="model-rate-add"
        @click="addRule"
      >
        {{ t('admin.groups.modelRateMultipliers.addRule') }}
      </button>

      <p
        v-if="validationError"
        class="rounded-lg bg-red-50 px-3 py-2 text-xs text-red-600 dark:bg-red-500/10 dark:text-red-300"
        role="alert"
        data-testid="model-rate-error"
      >
        {{ validationError }}
      </p>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import Icon from '@/components/icons/Icon.vue'
import Toggle from '@/components/common/Toggle.vue'
import { formatMultiplier } from '@/utils/formatters'
import type { ModelRateMultiplierRule, ModelRateMultipliers } from '@/types'

const props = defineProps<{
  modelValue?: ModelRateMultipliers | null
  // 分组倍率，用于实时展示「生效倍率 = 分组倍率 × 系数」
  groupRate?: number | null
}>()

const emit = defineEmits<{
  'update:modelValue': [value: ModelRateMultipliers]
}>()

const { t } = useI18n()

const enabled = ref(props.modelValue?.enabled ?? false)
const rules = ref<ModelRateMultiplierRule[]>([])
// 系数用独立的字符串数组承接输入：number 输入框清空时得到空串，
// 直接写回 rule 会变成 0 并被后端拒绝，这里保留原始输入用于校验提示。
const multiplierInputs = ref<string[]>([])

// raw 声明为 unknown：number 输入框在程序化赋值 / 测试 setValue 时可能给到 number。
function inputToMultiplier(raw: unknown): number | null {
  if (raw === null || raw === undefined) return null
  const trimmed = String(raw).trim()
  if (trimmed === '') return null
  const parsed = Number(trimmed)
  return Number.isFinite(parsed) ? parsed : null
}

function syncFromProps(value: ModelRateMultipliers | null | undefined) {
  enabled.value = value?.enabled ?? false
  const incoming = value?.rules ?? []
  rules.value = incoming.map((rule) => ({ ...rule }))
  multiplierInputs.value = incoming.map((rule) => String(rule.multiplier))
}

syncFromProps(props.modelValue)

watch(
  () => props.modelValue,
  (value) => {
    // 只在外部整体替换（打开对话框、切换分组）时重灌；必须做内容比较，
    // 否则 v-model 回流的新引用会触发 emit → 回灌 → 再 emit 的无限循环。
    if (payloadEquals(value, buildPayload())) return
    syncFromProps(value)
  },
)

function effectiveLabel(index: number): string {
  const multiplier = inputToMultiplier(multiplierInputs.value[index])
  const groupRate = props.groupRate
  if (multiplier === null || multiplier <= 0 || groupRate === null || groupRate === undefined || !Number.isFinite(groupRate)) {
    return '-'
  }
  return `${formatMultiplier(groupRate)} × ${formatMultiplier(multiplier)} = ${formatMultiplier(groupRate * multiplier)}x`
}

function matchError(match: string, seen: Set<string>): string {
  if (match === '') return t('admin.groups.modelRateMultipliers.errors.emptyMatch')
  if (match === '*') return t('admin.groups.modelRateMultipliers.errors.bareWildcard')
  if (match.replace(/\*$/, '').includes('*')) {
    return t('admin.groups.modelRateMultipliers.errors.wildcardPosition', { match })
  }
  const key = match.toLowerCase()
  if (seen.has(key)) return t('admin.groups.modelRateMultipliers.errors.duplicate', { match })
  seen.add(key)
  return ''
}

const validationError = computed<string>(() => {
  const seen = new Set<string>()
  for (const [index, rule] of rules.value.entries()) {
    const match = rule.match.trim()
    const error = matchError(match, seen)
    if (error) return error
    const multiplier = inputToMultiplier(multiplierInputs.value[index])
    if (multiplier === null || multiplier <= 0) {
      return t('admin.groups.modelRateMultipliers.errors.invalidMultiplier', { match })
    }
  }
  if (enabled.value && rules.value.length === 0) {
    return t('admin.groups.modelRateMultipliers.errors.enabledButEmpty')
  }
  return ''
})

function buildPayload(): ModelRateMultipliers {
  return {
    enabled: enabled.value,
    rules: rules.value.map((rule, index) => ({
      match: rule.match.trim(),
      // 非法输入按 0 提交，由后端返回 400，避免静默按 1 倍计费
      multiplier: inputToMultiplier(multiplierInputs.value[index]) ?? 0,
    })),
  }
}

// payloadEquals 做内容比较。null 视为空配置（enabled=false、无规则）。
function payloadEquals(a: ModelRateMultipliers | null | undefined, b: ModelRateMultipliers): boolean {
  const left = a ?? { enabled: false, rules: [] }
  if (left.enabled !== b.enabled) return false
  if ((left.rules?.length ?? 0) !== b.rules.length) return false
  return b.rules.every((rule, i) => {
    const other = left.rules[i]
    return (other.match ?? '') === rule.match && other.multiplier === rule.multiplier
  })
}

function addRule() {
  rules.value.push({ match: '', multiplier: 1 })
  multiplierInputs.value.push('1')
}

function removeRule(index: number) {
  rules.value.splice(index, 1)
  multiplierInputs.value.splice(index, 1)
}

watch(
  [enabled, rules, multiplierInputs],
  () => {
    emit('update:modelValue', buildPayload())
  },
  { deep: true },
)

defineExpose({ validationError })
</script>

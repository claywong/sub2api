<template>
  <div class="space-y-3">
    <div class="flex items-start justify-between gap-3">
      <div>
        <label class="block text-sm font-medium text-gray-700 dark:text-gray-300">
          {{ t('admin.groups.modelQuotas.title') }}
        </label>
        <p class="mt-1 text-xs text-gray-500 dark:text-gray-400">
          {{ t('admin.groups.modelQuotas.hint') }}
        </p>
      </div>
      <Toggle v-model="enabled" data-testid="model-quotas-enabled" />
    </div>

    <div v-if="enabled" class="space-y-3">
      <div
        v-if="rules.length === 0"
        class="rounded-xl border border-dashed border-gray-300 px-4 py-6 text-center text-sm text-gray-500 dark:border-dark-600 dark:text-gray-400"
      >
        {{ t('admin.groups.modelQuotas.empty') }}
      </div>

      <div v-else class="overflow-x-auto">
        <table class="min-w-full text-sm">
          <thead>
            <tr
              class="border-b border-gray-200 text-gray-700 dark:border-dark-700 dark:text-gray-300"
            >
              <th class="px-2 py-2 text-left font-medium">
                {{ t('admin.groups.modelQuotas.columns.match') }}
              </th>
              <th class="px-2 py-2 text-left font-medium">
                {{ t('admin.groups.modelQuotas.columns.daily') }}
              </th>
              <th class="px-2 py-2 text-left font-medium">
                {{ t('admin.groups.modelQuotas.columns.weekly') }}
              </th>
              <th class="px-2 py-2 text-left font-medium">
                {{ t('admin.groups.modelQuotas.columns.monthly') }}
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
                  :placeholder="t('admin.groups.modelQuotas.matchPlaceholder')"
                  :data-testid="`model-quota-match-${index}`"
                />
              </td>
              <td class="px-2 py-2">
                <input
                  v-model="dailyInputs[index]"
                  type="number"
                  min="0"
                  step="0.01"
                  class="input w-24"
                  :placeholder="t('admin.groups.modelQuotas.unlimited')"
                />
              </td>
              <td class="px-2 py-2">
                <input
                  v-model="weeklyInputs[index]"
                  type="number"
                  min="0"
                  step="0.01"
                  class="input w-24"
                  :placeholder="t('admin.groups.modelQuotas.unlimited')"
                />
              </td>
              <td class="px-2 py-2">
                <input
                  v-model="monthlyInputs[index]"
                  type="number"
                  min="0"
                  step="0.01"
                  class="input w-24"
                  :placeholder="t('admin.groups.modelQuotas.unlimited')"
                />
              </td>
              <td class="px-2 py-2 text-right">
                <button
                  type="button"
                  class="text-gray-400 transition-colors hover:text-red-500"
                  :title="t('common.delete')"
                  :data-testid="`model-quota-remove-${index}`"
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
        data-testid="model-quota-add"
        @click="addRule"
      >
        {{ t('admin.groups.modelQuotas.addRule') }}
      </button>

      <p
        v-if="validationError"
        class="rounded-lg bg-red-50 px-3 py-2 text-xs text-red-600 dark:bg-red-500/10 dark:text-red-300"
        data-testid="model-quota-error"
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
import type { ModelQuotaRule, ModelQuotas } from '@/types'

const props = defineProps<{
  modelValue?: ModelQuotas | null
}>()

const emit = defineEmits<{
  'update:modelValue': [value: ModelQuotas]
}>()

const { t } = useI18n()

const enabled = ref(props.modelValue?.enabled ?? false)
const rules = ref<ModelQuotaRule[]>([])

// 三个上限用独立的字符串数组承接输入：number 类型的 v-model 在清空输入框时
// 会得到空串或 NaN，直接写回 rule 会把"不限制"（null）和"填了 0"（禁用）混淆。
const dailyInputs = ref<string[]>([])
const weeklyInputs = ref<string[]>([])
const monthlyInputs = ref<string[]>([])

function limitToInput(value: number | null | undefined): string {
  return value === null || value === undefined ? '' : String(value)
}

// raw 声明为 unknown 而非 string：number 输入框在部分场景（程序化赋值、
// 测试的 setValue）会给到 number，写死 string 会在 raw.trim() 处抛错。
function inputToLimit(raw: unknown): number | null {
  if (raw === null || raw === undefined) return null
  const trimmed = String(raw).trim()
  if (trimmed === '') return null
  const parsed = Number(trimmed)
  return Number.isFinite(parsed) ? parsed : null
}

function syncFromProps(value: ModelQuotas | null | undefined) {
  enabled.value = value?.enabled ?? false
  const incoming = value?.rules ?? []
  rules.value = incoming.map((rule) => ({ ...rule }))
  dailyInputs.value = incoming.map((rule) => limitToInput(rule.daily))
  weeklyInputs.value = incoming.map((rule) => limitToInput(rule.weekly))
  monthlyInputs.value = incoming.map((rule) => limitToInput(rule.monthly))
}

syncFromProps(props.modelValue)

watch(
  () => props.modelValue,
  (value) => {
    // 只在外部整体替换（打开对话框、切换分组）时重灌。
    // 必须做内容比较：本组件 emit 出去的对象经父组件 v-model 回流时引用必变，
    // 引用比较（===）恒不相等，会形成 emit → 回灌 → 重置 → 再 emit 的无限循环。
    if (payloadEquals(value, buildPayload())) return
    syncFromProps(value)
  },
)

const validationError = computed<string>(() => {
  const seen = new Set<string>()
  for (const [index, rule] of rules.value.entries()) {
    const match = rule.match.trim()
    if (match === '') {
      return t('admin.groups.modelQuotas.errors.emptyMatch')
    }
    if (match === '*') {
      return t('admin.groups.modelQuotas.errors.bareWildcard')
    }
    if (match.replace(/\*$/, '').includes('*')) {
      return t('admin.groups.modelQuotas.errors.wildcardPosition', { match })
    }
    const key = match.toLowerCase()
    if (seen.has(key)) {
      return t('admin.groups.modelQuotas.errors.duplicate', { match })
    }
    seen.add(key)

    for (const raw of [dailyInputs.value[index], weeklyInputs.value[index], monthlyInputs.value[index]]) {
      const limit = inputToLimit(raw ?? '')
      if (limit !== null && limit < 0) {
        return t('admin.groups.modelQuotas.errors.negativeLimit')
      }
    }
  }
  if (enabled.value && rules.value.length === 0) {
    return t('admin.groups.modelQuotas.errors.enabledButEmpty')
  }
  return ''
})

function buildPayload(): ModelQuotas {
  return {
    enabled: enabled.value,
    rules: rules.value.map((rule, index) => ({
      match: rule.match.trim(),
      daily: inputToLimit(dailyInputs.value[index] ?? ''),
      weekly: inputToLimit(weeklyInputs.value[index] ?? ''),
      monthly: inputToLimit(monthlyInputs.value[index] ?? ''),
    })),
  }
}

// payloadEquals 做内容比较。null 视为空配置（enabled=false、无规则）。
function payloadEquals(a: ModelQuotas | null | undefined, b: ModelQuotas): boolean {
  const left = a ?? { enabled: false, rules: [] }
  const right = b
  if (left.enabled !== right.enabled) return false
  if ((left.rules?.length ?? 0) !== (right.rules?.length ?? 0)) return false
  for (let i = 0; i < (right.rules?.length ?? 0); i++) {
    const lr = left.rules![i]
    const rr = right.rules[i]
    if ((lr.match ?? '') !== (rr.match ?? '')) return false
    if ((lr.daily ?? null) !== (rr.daily ?? null)) return false
    if ((lr.weekly ?? null) !== (rr.weekly ?? null)) return false
    if ((lr.monthly ?? null) !== (rr.monthly ?? null)) return false
  }
  return true
}

function addRule() {
  rules.value.push({ match: '', daily: null, weekly: null, monthly: null })
  dailyInputs.value.push('')
  weeklyInputs.value.push('')
  monthlyInputs.value.push('')
}

function removeRule(index: number) {
  rules.value.splice(index, 1)
  dailyInputs.value.splice(index, 1)
  weeklyInputs.value.splice(index, 1)
  monthlyInputs.value.splice(index, 1)
}

watch(
  [enabled, rules, dailyInputs, weeklyInputs, monthlyInputs],
  () => {
    emit('update:modelValue', buildPayload())
  },
  { deep: true },
)

defineExpose({ validationError })
</script>

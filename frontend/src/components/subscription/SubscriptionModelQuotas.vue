<template>
  <!-- 分组未配置按模型配额时整块不渲染，避免给多数用户增加噪音 -->
  <div v-if="loading" class="flex items-center gap-2 border-t border-gray-100 pt-4 dark:border-dark-700">
    <div class="h-3 w-3 animate-spin rounded-full border-2 border-primary-500 border-t-transparent"></div>
    <span class="text-xs text-gray-500 dark:text-dark-400">{{ t('common.loading') }}</span>
  </div>

  <div v-else-if="items.length > 0" class="border-t border-gray-100 pt-4 dark:border-dark-700">
    <button
      type="button"
      class="flex w-full items-center justify-between text-left"
      :aria-expanded="expanded"
      :aria-controls="panelId"
      @click="expanded = !expanded"
    >
      <span class="text-sm font-medium text-gray-700 dark:text-gray-300">
        {{ t('userSubscriptions.modelQuota.title') }}
        <span class="ml-1 text-xs font-normal text-gray-400 dark:text-gray-500">({{ items.length }})</span>
      </span>
      <span class="text-xs text-gray-400 dark:text-gray-500">
        {{ expanded ? t('userSubscriptions.modelQuota.hide') : t('userSubscriptions.modelQuota.show') }}
      </span>
    </button>

    <div v-show="expanded" :id="panelId" class="mt-3 space-y-4">
      <p class="text-xs text-gray-500 dark:text-dark-400">
        {{ t('userSubscriptions.modelQuota.description') }}
      </p>

      <div
        v-for="item in items"
        :key="item.rule_key"
        class="rounded-xl border border-gray-100 p-3 dark:border-dark-700"
      >
        <div class="flex items-baseline justify-between gap-2">
          <code class="break-all text-xs font-medium text-gray-800 dark:text-gray-200">{{
            item.match
          }}</code>
          <span
            v-if="isPrefixRule(item.match)"
            class="shrink-0 text-[11px] text-gray-400 dark:text-gray-500"
          >
            {{ t('userSubscriptions.modelQuota.prefixHint') }}
          </span>
        </div>

        <!-- 上限为 0 是显式禁用，画进度条会显示成“已满”，语义上更接近不可用 -->
        <p
          v-if="isFullyDisabled(item)"
          class="mt-2 text-xs font-medium text-red-600 dark:text-red-400"
        >
          {{ t('userSubscriptions.modelQuota.disabled') }} ·
          <span class="font-normal text-gray-500 dark:text-dark-400">
            {{ t('userSubscriptions.modelQuota.disabledHint') }}
          </span>
        </p>

        <div v-else class="mt-2 space-y-3">
          <div v-for="window in windowsOf(item)" :key="window.label" class="space-y-1.5">
            <div class="flex items-center justify-between text-xs">
              <span class="text-gray-600 dark:text-gray-400">{{ window.label }}</span>
              <span class="text-gray-500 dark:text-dark-400">
                {{ formatUSD(window.data.used_usd) }} /
                <template v-if="window.data.limit_usd > 0">{{
                  formatUSD(window.data.limit_usd)
                }}</template>
                <template v-else>{{ t('userSubscriptions.modelQuota.disabled') }}</template>
              </span>
            </div>
            <div class="relative h-1.5 overflow-hidden rounded-full bg-gray-200 dark:bg-dark-600">
              <div
                class="absolute inset-y-0 left-0 rounded-full transition-all duration-300"
                :class="barClass(window.data.percentage)"
                :style="{ width: barWidth(window.data.percentage) }"
              ></div>
            </div>
            <p class="text-[11px] text-gray-400 dark:text-gray-500">
              <template v-if="window.data.remaining_usd <= 0">
                {{ t('userSubscriptions.modelQuota.exhausted') }} ·
              </template>
              {{ t('userSubscriptions.resetIn', { time: formatResetsIn(window.data) }) }}
            </p>
          </div>
        </div>
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import subscriptionsAPI from '@/api/subscriptions'
import type { ModelQuotaUsageProgress, ModelQuotaUsageWindow } from '@/types'
import { getRemainingDurationParts } from '@/utils/subscriptionQuota'

const props = defineProps<{ subscriptionId: number }>()

// 父级用规则数判断该订阅是否真的“无限制”：分组总额度为空但配了按模型额度时，
// 订阅卡片不能再显示无限制徽章。
const emit = defineEmits<{ loaded: [ruleCount: number] }>()

const { t } = useI18n()

const items = ref<ModelQuotaUsageProgress[]>([])
const loading = ref(true)
const expanded = ref(false)
// 同页面会渲染多张订阅卡片，订阅 ID 保证 aria-controls 页内唯一
const panelId = `model-quota-panel-${props.subscriptionId}`

interface LabeledWindow {
  label: string
  data: ModelQuotaUsageWindow
}

function windowsOf(item: ModelQuotaUsageProgress): LabeledWindow[] {
  const out: LabeledWindow[] = []
  if (item.daily) out.push({ label: t('userSubscriptions.daily'), data: item.daily })
  if (item.weekly) out.push({ label: t('userSubscriptions.weekly'), data: item.weekly })
  if (item.monthly) out.push({ label: t('userSubscriptions.monthly'), data: item.monthly })
  return out
}

// 所有已配置窗口的上限都是 0 才算禁用；混配（如日 0、月 50）仍按窗口逐条展示
function isFullyDisabled(item: ModelQuotaUsageProgress): boolean {
  const windows = windowsOf(item)
  return windows.length > 0 && windows.every(w => w.data.limit_usd === 0)
}

function isPrefixRule(match: string): boolean {
  return match.endsWith('*')
}

function formatUSD(value: number): string {
  return `$${(value || 0).toFixed(2)}`
}

function barWidth(percentage: number): string {
  return `${Math.min(Math.max(percentage || 0, 0), 100)}%`
}

function barClass(percentage: number): string {
  if (percentage >= 90) return 'bg-red-500'
  if (percentage >= 70) return 'bg-orange-500'
  return 'bg-green-500'
}

function formatResetsIn(window: ModelQuotaUsageWindow): string {
  const parts = getRemainingDurationParts(window.resets_at)
  if (!parts) return t('userSubscriptions.windowNotActive')
  if (parts.days > 0) return `${parts.days}d ${parts.hours}h`
  if (parts.hours > 0) return `${parts.hours}h ${parts.minutes}m`
  return `${parts.minutes}m`
}

async function load() {
  try {
    items.value = await subscriptionsAPI.getSubscriptionModelQuotaUsage(props.subscriptionId)
  } catch (error) {
    // 按模型额度是订阅卡片的附加信息，加载失败不弹全局错误、不影响主体额度展示
    console.error('Failed to load per-model quota usage:', error)
    items.value = []
  } finally {
    loading.value = false
    emit('loaded', items.value.length)
  }
}

onMounted(load)
</script>

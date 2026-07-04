<!--
  Private extension (not upstream sub2api).

  /monitor 页 分组消耗 (近 1 小时) section。
  按当前用户可见分组展示 分组 → 模型 两级：请求数 / 成功率 / 缓存率 /
  TTFT均值 / TTFT p90 / OTPS均值 / 次均成本 / 总成本。

  数据源：GET /api/v1/monitor/group-usage。自动 60s 刷新（tab 隐藏或已在 loading 时暂停）。
-->
<template>
  <section
    class="mt-6 p-5 rounded-2xl bg-white/70 backdrop-blur-xl border border-gray-200/80 shadow-card dark:bg-dark-800/60 dark:border-dark-700/70"
  >
    <header class="flex flex-wrap items-center justify-between gap-3 mb-4">
      <div>
        <h2 class="text-base font-semibold text-gray-900 dark:text-gray-100">
          {{ t('groupUsage.title') }}
        </h2>
        <p class="mt-0.5 text-xs text-gray-500 dark:text-gray-400">
          {{ t('groupUsage.description') }}
        </p>
      </div>
      <div class="flex items-center gap-3 text-xs text-gray-500 dark:text-gray-400">
        <span v-if="generatedAt">
          {{ t('groupUsage.updatedAt', { time: formattedUpdatedAt }) }}
        </span>
        <button
          type="button"
          class="px-2.5 py-1 rounded-md border border-gray-200 dark:border-dark-700 hover:bg-gray-50 dark:hover:bg-dark-700/50 disabled:opacity-50"
          :disabled="loading"
          @click="reload(false)"
        >
          {{ loading ? t('groupUsage.refreshing') : t('groupUsage.refresh') }}
        </button>
      </div>
    </header>

    <!-- Table -->
    <div class="overflow-x-auto">
      <table class="min-w-full text-sm">
        <thead>
          <tr class="text-left text-xs uppercase tracking-wide text-gray-500 dark:text-gray-400 border-b border-gray-200/70 dark:border-dark-700/60">
            <th class="py-2 pr-4 font-medium w-8"></th>
            <th class="py-2 pr-4 font-medium">{{ t('groupUsage.columns.group') }}</th>
            <th class="py-2 pr-4 font-medium text-right">{{ t('groupUsage.columns.requests') }}</th>
            <th class="py-2 pr-4 font-medium text-right">{{ t('groupUsage.columns.successRate') }}</th>
            <th class="py-2 pr-4 font-medium text-right">{{ t('groupUsage.columns.cacheHitRate') }}</th>
            <th class="py-2 pr-4 font-medium text-right">{{ t('groupUsage.columns.ttftAvg') }}</th>
            <th class="py-2 pr-4 font-medium text-right">{{ t('groupUsage.columns.ttftP90') }}</th>
            <th class="py-2 pr-4 font-medium text-right">{{ t('groupUsage.columns.otpsAvg') }}</th>
            <th class="py-2 pr-4 font-medium text-right">{{ t('groupUsage.columns.costAvg') }}</th>
            <th class="py-2 font-medium text-right">{{ t('groupUsage.columns.totalCost') }}</th>
          </tr>
        </thead>
        <tbody>
          <template v-if="loading && groups.length === 0">
            <tr v-for="i in 3" :key="`skel-${i}`">
              <td colspan="10" class="py-3">
                <div class="h-4 rounded bg-gray-100 dark:bg-dark-700/60 animate-pulse"></div>
              </td>
            </tr>
          </template>
          <template v-else-if="groups.length === 0">
            <tr>
              <td colspan="10" class="py-10 text-center text-gray-400">
                {{ t('groupUsage.empty') }}
              </td>
            </tr>
          </template>
          <template v-else>
            <template v-for="g in groups" :key="`g-${g.group_id ?? 'null'}-${g.group_name}`">
              <tr
                class="border-b border-gray-100/70 dark:border-dark-700/40 hover:bg-gray-50/60 dark:hover:bg-dark-700/30 cursor-pointer"
                @click="toggle(groupKey(g))"
              >
                <td class="py-2 pr-4 text-gray-400">
                  <span :class="['inline-block transition-transform', expanded.has(groupKey(g)) ? 'rotate-90' : '']">▸</span>
                </td>
                <td class="py-2 pr-4">
                  <div class="flex items-center gap-2">
                    <span class="font-medium text-gray-900 dark:text-gray-100 truncate">
                      {{ g.group_name }}
                    </span>
                    <span
                      v-if="g.models.length > 0"
                      class="inline-flex items-center rounded-md px-1.5 py-0.5 text-[10px] font-medium bg-gray-100 text-gray-600 dark:bg-dark-700 dark:text-gray-300"
                    >
                      {{ t('groupUsage.modelCount', { count: g.models.length }) }}
                    </span>
                  </div>
                </td>
                <td class="py-2 pr-4 text-right tabular-nums text-gray-800 dark:text-gray-200">
                  {{ formatInt(g.requests) }}
                </td>
                <td class="py-2 pr-4 text-right tabular-nums" :class="successRateClass(g.success_rate)">
                  {{ formatPct(g.success_rate) }}
                </td>
                <td class="py-2 pr-4 text-right tabular-nums" :class="cacheRateClass(g.cache_hit_rate)">
                  {{ formatPct(g.cache_hit_rate) }}
                </td>
                <td class="py-2 pr-4 text-right tabular-nums" :class="ttftClass(g.ttft_avg_ms)">
                  {{ formatTtft(g.ttft_avg_ms) }}
                </td>
                <td class="py-2 pr-4 text-right tabular-nums" :class="ttftClass(g.ttft_p90_ms)">
                  {{ formatTtft(g.ttft_p90_ms) }}
                </td>
                <td class="py-2 pr-4 text-right tabular-nums text-gray-800 dark:text-gray-200">
                  {{ formatOtps(g.otps_avg) }}
                </td>
                <td class="py-2 pr-4 text-right tabular-nums text-gray-800 dark:text-gray-200">
                  {{ formatCost(g.cost_avg) }}
                </td>
                <td class="py-2 text-right tabular-nums text-gray-800 dark:text-gray-200">
                  {{ formatTotalCost(g.total_cost) }}
                </td>
              </tr>
              <template v-if="expanded.has(groupKey(g))">
                <tr
                  v-for="m in g.models"
                  :key="`m-${g.group_id ?? 'null'}-${m.model}`"
                  class="border-b border-gray-100/50 dark:border-dark-700/30 bg-gray-50/40 dark:bg-dark-700/20"
                >
                  <td class="py-1.5 pr-4"></td>
                  <td class="py-1.5 pr-4 pl-6 text-gray-600 dark:text-gray-400 font-mono text-xs">
                    ↳ {{ m.model }}
                  </td>
                  <td class="py-1.5 pr-4 text-right tabular-nums text-gray-600 dark:text-gray-400">
                    {{ formatInt(m.requests) }}
                  </td>
                  <td class="py-1.5 pr-4 text-right tabular-nums" :class="successRateClass(m.success_rate)">
                    {{ formatPct(m.success_rate) }}
                  </td>
                  <td class="py-1.5 pr-4 text-right tabular-nums" :class="cacheRateClass(m.cache_hit_rate)">
                    {{ formatPct(m.cache_hit_rate) }}
                  </td>
                  <td class="py-1.5 pr-4 text-right tabular-nums" :class="ttftClass(m.ttft_avg_ms)">
                    {{ formatTtft(m.ttft_avg_ms) }}
                  </td>
                  <td class="py-1.5 pr-4 text-right tabular-nums" :class="ttftClass(m.ttft_p90_ms)">
                    {{ formatTtft(m.ttft_p90_ms) }}
                  </td>
                  <td class="py-1.5 pr-4 text-right tabular-nums text-gray-600 dark:text-gray-400">
                    {{ formatOtps(m.otps_avg) }}
                  </td>
                  <td class="py-1.5 pr-4 text-right tabular-nums text-gray-600 dark:text-gray-400">
                    {{ formatCost(m.cost_avg) }}
                  </td>
                  <td class="py-1.5 text-right tabular-nums text-gray-600 dark:text-gray-400">
                    {{ formatTotalCost(m.total_cost) }}
                  </td>
                </tr>
              </template>
            </template>
          </template>
        </tbody>
      </table>
    </div>
  </section>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, reactive, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useAppStore } from '@/stores/app'
import { extractApiErrorMessage } from '@/utils/apiError'
import { useAutoRefresh } from '@/composables/useAutoRefresh'
import {
  fetchMonitorGroupUsage,
  type MonitorGroupUsageGroup,
} from '@/api/monitorGroupUsage'

const { t } = useI18n()
const appStore = useAppStore()

const groups = ref<MonitorGroupUsageGroup[]>([])
const loading = ref(false)
const generatedAt = ref<string>('')
const expanded = reactive(new Set<string>())

let abortController: AbortController | null = null

const REFRESH_SECONDS = 60

const autoRefresh = useAutoRefresh({
  storageKey: 'monitor-group-usage-auto-refresh',
  intervals: [30, 60, 120] as const,
  defaultInterval: REFRESH_SECONDS,
  onRefresh: () => reload(true),
  shouldPause: () => document.hidden || loading.value,
})

const formattedUpdatedAt = computed(() => {
  if (!generatedAt.value) return ''
  try {
    return new Date(generatedAt.value).toLocaleTimeString()
  } catch {
    return generatedAt.value
  }
})

function groupKey(g: MonitorGroupUsageGroup): string {
  return `${g.group_id ?? 'null'}-${g.group_name}`
}

function toggle(key: string) {
  if (expanded.has(key)) expanded.delete(key)
  else expanded.add(key)
}

// ── Formatters ──
function formatInt(n: number | null | undefined): string {
  if (n === null || n === undefined) return '-'
  return n.toLocaleString()
}

function formatPct(v: number | null): string {
  if (v === null || v === undefined) return '-'
  return `${v.toFixed(1)}%`
}

function formatTtft(v: number | null): string {
  if (v === null || v === undefined) return '-'
  if (v >= 1000) return `${(v / 1000).toFixed(1)}s`
  return `${Math.round(v)}ms`
}

function formatOtps(v: number | null): string {
  if (v === null || v === undefined) return '-'
  return `${v.toFixed(1)} t/s`
}

function formatCost(v: number | null): string {
  if (v === null || v === undefined) return '-'
  if (v === 0) return '$0'
  if (v < 0.001) return `$${v.toFixed(6)}`
  if (v < 0.01) return `$${v.toFixed(5)}`
  return `$${v.toFixed(4)}`
}

function formatTotalCost(v: number | null | undefined): string {
  if (v === null || v === undefined) return '-'
  if (v === 0) return '$0'
  if (v < 0.01) return `$${v.toFixed(4)}`
  return `$${v.toFixed(2)}`
}

// ── Classes (threshold coloring) ──
function successRateClass(v: number | null): string {
  if (v === null || v === undefined) return 'text-gray-800 dark:text-gray-200'
  if (v >= 99) return 'text-emerald-600 dark:text-emerald-400 font-medium'
  if (v >= 95) return 'text-amber-600 dark:text-amber-400 font-medium'
  return 'text-red-600 dark:text-red-400 font-semibold'
}

function cacheRateClass(v: number | null): string {
  if (v === null || v === undefined) return 'text-gray-800 dark:text-gray-200'
  if (v >= 85) return 'text-emerald-600 dark:text-emerald-400'
  if (v >= 70) return 'text-gray-800 dark:text-gray-200'
  if (v >= 50) return 'text-amber-600 dark:text-amber-400'
  return 'text-red-600 dark:text-red-400'
}

function ttftClass(v: number | null): string {
  if (v === null || v === undefined) return 'text-gray-800 dark:text-gray-200'
  if (v < 1000) return 'text-emerald-600 dark:text-emerald-400'
  if (v < 3000) return 'text-gray-800 dark:text-gray-200'
  if (v < 8000) return 'text-amber-600 dark:text-amber-400'
  return 'text-red-600 dark:text-red-400'
}

// ── Loader ──
async function reload(silent = false) {
  if (abortController) abortController.abort()
  const ctrl = new AbortController()
  abortController = ctrl
  if (!silent) loading.value = true
  try {
    const res = await fetchMonitorGroupUsage({ signal: ctrl.signal })
    if (ctrl.signal.aborted || abortController !== ctrl) return
    groups.value = res.groups || []
    generatedAt.value = res.generated_at || ''
  } catch (err: unknown) {
    const e = err as { name?: string; code?: string }
    if (e?.name === 'AbortError' || e?.code === 'ERR_CANCELED') return
    appStore.showError(extractApiErrorMessage(err, t('groupUsage.loadError')))
  } finally {
    if (abortController === ctrl) {
      if (!silent) loading.value = false
      abortController = null
    }
  }
}

onMounted(() => {
  void reload(false)
  autoRefresh.setEnabled(true)
})

onBeforeUnmount(() => {
  if (abortController) abortController.abort()
  autoRefresh.stop()
})
</script>

<!--
  DlpPanel.vue
  ============================================================================
  私有扩展（不属于 upstream sub2api）：DLP 敏感信息检测的配置面板。

  与 PolicyPanel（qwen3guard 内容安全）平级但独立：DLP 走本地正则初筛 + LLM
  二次确认，拥有自己的检测器、确认节点与缓存配置。

  与 upstream 合并策略：
    - 全新组件文件。upstream 侧只有 PromptAuditView.vue 两行 import/渲染。
  ============================================================================
-->
<template>
  <section aria-labelledby="dlp-policy-title" class="border-t border-gray-100 py-6 dark:border-dark-800">
    <div class="flex flex-wrap items-start justify-between gap-3">
      <div>
        <h2 id="dlp-policy-title" class="text-base font-semibold text-gray-950 dark:text-white">
          {{ t('admin.promptAudit.dlp.title') }}
        </h2>
        <p class="mt-1 text-sm text-gray-500 dark:text-dark-300">{{ t('admin.promptAudit.dlp.description') }}</p>
      </div>
      <label class="flex items-center gap-2 text-sm text-gray-700 dark:text-dark-200">
        <input
          type="checkbox"
          :checked="dlp.enabled"
          data-test="dlp-enabled"
          :aria-label="t('admin.promptAudit.dlp.enabled')"
          @change="patch({ enabled: ($event.target as HTMLInputElement).checked })"
        />
        <span>{{ t('admin.promptAudit.dlp.enabled') }}</span>
      </label>
    </div>

    <div v-if="dlp.enabled" class="mt-5 grid gap-4 lg:grid-cols-[minmax(0,1fr)_minmax(280px,0.5fr)]">
      <div class="space-y-5 rounded-xl border border-gray-200 p-4 dark:border-dark-700/60 dark:bg-dark-900/20 sm:p-5">
        <fieldset>
          <legend class="text-sm font-medium text-gray-900 dark:text-white">
            {{ t('admin.promptAudit.dlp.detectors') }}
          </legend>
          <p class="mt-1 text-xs text-gray-500 dark:text-dark-400">{{ t('admin.promptAudit.dlp.detectorsHint') }}</p>
          <div class="mt-3 grid gap-2 sm:grid-cols-2">
            <label
              v-for="scanner in DLP_SCANNER_CATALOG"
              :key="scanner.id"
              class="flex items-center gap-2 rounded-md px-2 py-1.5 text-sm text-gray-700 hover:bg-gray-50 dark:text-dark-200 dark:hover:bg-dark-800"
            >
              <input
                type="checkbox"
                :checked="isScannerEnabled(scanner.id)"
                :aria-label="detectorLabel(scanner.id)"
                @change="toggleScanner(scanner.id)"
              />
              <span>{{ detectorLabel(scanner.id) }}</span>
            </label>
          </div>
        </fieldset>

        <fieldset class="border-t border-gray-100 pt-5 dark:border-dark-800">
          <legend class="text-sm font-medium text-gray-900 dark:text-white">
            {{ t('admin.promptAudit.dlp.disposition') }}
          </legend>
          <label class="mt-3 flex items-start gap-2 text-sm text-gray-700 dark:text-dark-200">
            <input
              type="checkbox"
              class="mt-1"
              :checked="dlp.block_on_high_severity"
              data-test="dlp-block-high"
              :aria-label="t('admin.promptAudit.dlp.blockOnHigh')"
              @change="patch({ block_on_high_severity: ($event.target as HTMLInputElement).checked })"
            />
            <span>
              {{ t('admin.promptAudit.dlp.blockOnHigh') }}
              <span class="mt-0.5 block text-xs text-gray-500 dark:text-dark-400">
                {{ t('admin.promptAudit.dlp.blockOnHighHint') }}
              </span>
            </span>
          </label>
        </fieldset>

        <fieldset class="border-t border-gray-100 pt-5 dark:border-dark-800">
          <legend class="text-sm font-medium text-gray-900 dark:text-white">
            {{ t('admin.promptAudit.dlp.confirm') }}
          </legend>
          <label class="mt-3 flex items-start gap-2 text-sm text-gray-700 dark:text-dark-200">
            <input
              type="checkbox"
              class="mt-1"
              :checked="dlp.confirm_enabled"
              data-test="dlp-confirm-enabled"
              :aria-label="t('admin.promptAudit.dlp.confirmEnabled')"
              @change="patch({ confirm_enabled: ($event.target as HTMLInputElement).checked })"
            />
            <span>
              {{ t('admin.promptAudit.dlp.confirmEnabled') }}
              <span class="mt-0.5 block text-xs text-gray-500 dark:text-dark-400">
                {{ t('admin.promptAudit.dlp.confirmEnabledHint') }}
              </span>
            </span>
          </label>

          <div v-if="dlp.confirm_enabled" class="mt-4 space-y-3">
            <label class="block text-sm text-gray-700 dark:text-dark-200">
              <span>{{ t('admin.promptAudit.dlp.confirmTimeout') }}</span>
              <input
                :value="dlp.confirm_timeout_ms"
                type="number"
                min="500"
                max="30000"
                class="input mt-1.5 w-full"
                :aria-label="t('admin.promptAudit.dlp.confirmTimeout')"
                @input="patch({ confirm_timeout_ms: Number(($event.target as HTMLInputElement).value) })"
              />
            </label>
            <p class="rounded-lg bg-amber-50 px-3 py-2 text-xs text-amber-800 dark:bg-amber-950/30 dark:text-amber-200">
              {{ t('admin.promptAudit.dlp.failOpenNotice') }}
            </p>
          </div>
        </fieldset>

        <fieldset class="border-t border-gray-100 pt-5 dark:border-dark-800">
          <legend class="text-sm font-medium text-gray-900 dark:text-white">
            {{ t('admin.promptAudit.dlp.cache') }}
          </legend>
          <label class="mt-3 flex items-start gap-2 text-sm text-gray-700 dark:text-dark-200">
            <input
              type="checkbox"
              class="mt-1"
              :checked="dlp.cache_enabled"
              data-test="dlp-cache-enabled"
              :aria-label="t('admin.promptAudit.dlp.cacheEnabled')"
              @change="patch({ cache_enabled: ($event.target as HTMLInputElement).checked })"
            />
            <span>
              {{ t('admin.promptAudit.dlp.cacheEnabled') }}
              <span class="mt-0.5 block text-xs text-gray-500 dark:text-dark-400">
                {{ t('admin.promptAudit.dlp.cacheEnabledHint') }}
              </span>
            </span>
          </label>
          <div v-if="dlp.cache_enabled" class="mt-4 grid gap-3 sm:grid-cols-2">
            <label class="block text-sm text-gray-700 dark:text-dark-200">
              <span>{{ t('admin.promptAudit.dlp.cacheSensitiveTtl') }}</span>
              <input
                :value="dlp.cache_sensitive_ttl_hours"
                type="number"
                min="0"
                max="720"
                class="input mt-1.5 w-full"
                :aria-label="t('admin.promptAudit.dlp.cacheSensitiveTtl')"
                @input="patch({ cache_sensitive_ttl_hours: Number(($event.target as HTMLInputElement).value) })"
              />
            </label>
            <label class="block text-sm text-gray-700 dark:text-dark-200">
              <span>{{ t('admin.promptAudit.dlp.cacheBenignTtl') }}</span>
              <input
                :value="dlp.cache_benign_ttl_hours"
                type="number"
                min="0"
                max="720"
                class="input mt-1.5 w-full"
                :aria-label="t('admin.promptAudit.dlp.cacheBenignTtl')"
                @input="patch({ cache_benign_ttl_hours: Number(($event.target as HTMLInputElement).value) })"
              />
            </label>
          </div>
        </fieldset>
      </div>

      <div class="space-y-3 rounded-xl border border-gray-200 p-4 dark:border-dark-700/60 dark:bg-dark-900/20 sm:p-5">
        <div class="flex items-center justify-between gap-2">
          <h3 class="text-sm font-medium text-gray-900 dark:text-white">
            {{ t('admin.promptAudit.dlp.endpoints') }}
          </h3>
          <button type="button" class="btn btn-secondary btn-sm" data-test="dlp-add-endpoint" @click="addEndpoint">
            {{ t('admin.promptAudit.dlp.addEndpoint') }}
          </button>
        </div>
        <p class="text-xs text-gray-500 dark:text-dark-400">{{ t('admin.promptAudit.dlp.endpointsHint') }}</p>

        <p
          v-if="dlp.confirm_enabled && dlp.endpoints.length === 0"
          class="rounded-lg bg-red-50 px-3 py-2 text-xs text-red-700 dark:bg-red-950/30 dark:text-red-200"
        >
          {{ t('admin.promptAudit.dlp.endpointRequired') }}
        </p>

        <div
          v-for="(endpoint, index) in dlp.endpoints"
          :key="endpoint.id"
          class="space-y-2 rounded-lg border border-gray-200 p-3 dark:border-dark-700"
        >
          <div class="flex items-center justify-between gap-2">
            <label class="flex items-center gap-2 text-sm text-gray-700 dark:text-dark-200">
              <input
                type="checkbox"
                :checked="endpoint.enabled"
                :aria-label="t('admin.promptAudit.dlp.endpointEnabled')"
                @change="patchEndpoint(index, { enabled: ($event.target as HTMLInputElement).checked })"
              />
              <span>{{ t('admin.promptAudit.dlp.endpointEnabled') }}</span>
            </label>
            <button
              type="button"
              class="text-xs text-red-600 hover:underline dark:text-red-400"
              @click="removeEndpoint(index)"
            >
              {{ t('admin.promptAudit.dlp.removeEndpoint') }}
            </button>
          </div>

          <label class="block text-sm text-gray-700 dark:text-dark-200">
            <span>{{ t('admin.promptAudit.dlp.endpointName') }}</span>
            <input
              :value="endpoint.name"
              class="input mt-1 w-full"
              :aria-label="t('admin.promptAudit.dlp.endpointName')"
              @input="patchEndpoint(index, { name: ($event.target as HTMLInputElement).value })"
            />
          </label>
          <label class="block text-sm text-gray-700 dark:text-dark-200">
            <span>{{ t('admin.promptAudit.dlp.endpointBaseUrl') }}</span>
            <input
              :value="endpoint.base_url"
              placeholder="https://api.example.com"
              class="input mt-1 w-full"
              :aria-label="t('admin.promptAudit.dlp.endpointBaseUrl')"
              @input="patchEndpoint(index, { base_url: ($event.target as HTMLInputElement).value })"
            />
          </label>
          <label class="block text-sm text-gray-700 dark:text-dark-200">
            <span>{{ t('admin.promptAudit.dlp.endpointModel') }}</span>
            <input
              :value="endpoint.model"
              :placeholder="DEFAULT_DLP_CONFIRM_MODEL"
              class="input mt-1 w-full"
              :aria-label="t('admin.promptAudit.dlp.endpointModel')"
              @input="patchEndpoint(index, { model: ($event.target as HTMLInputElement).value })"
            />
          </label>
          <label class="block text-sm text-gray-700 dark:text-dark-200">
            <span>{{ t('admin.promptAudit.dlp.endpointTimeout') }}</span>
            <input
              :value="endpoint.timeout_ms"
              type="number"
              min="500"
              max="30000"
              class="input mt-1 w-full"
              :aria-label="t('admin.promptAudit.dlp.endpointTimeout')"
              @input="patchEndpoint(index, { timeout_ms: Number(($event.target as HTMLInputElement).value) })"
            />
          </label>

          <label class="block text-sm text-gray-700 dark:text-dark-200">
            <span>{{ t('admin.promptAudit.dlp.endpointToken') }}</span>
            <input
              :value="endpoint.token"
              type="password"
              autocomplete="off"
              :placeholder="tokenPlaceholder(endpoint)"
              class="input mt-1 w-full"
              :aria-label="t('admin.promptAudit.dlp.endpointToken')"
              @input="patchEndpoint(index, { token: ($event.target as HTMLInputElement).value })"
            />
          </label>
          <div class="flex flex-wrap items-center gap-3 text-xs">
            <span :class="tokenStatusClass(endpoint)">{{ tokenStatusLabel(endpoint) }}</span>
            <label v-if="endpoint.has_token" class="flex items-center gap-1.5 text-gray-600 dark:text-dark-300">
              <input
                type="checkbox"
                :checked="endpoint.clear_token"
                :aria-label="t('admin.promptAudit.dlp.clearToken')"
                @change="patchEndpoint(index, { clear_token: ($event.target as HTMLInputElement).checked })"
              />
              <span>{{ t('admin.promptAudit.dlp.clearToken') }}</span>
            </label>
          </div>
        </div>
      </div>
    </div>
  </section>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import type { DlpDraft, DlpEndpointDraft } from '../types'
import {
  cloneData,
  createDefaultDlpEndpoint,
  DEFAULT_DLP_CONFIRM_MODEL,
  DLP_SCANNER_CATALOG,
} from '../viewModel'

const props = defineProps<{ draft: DlpDraft }>()
const emit = defineEmits<{ (event: 'update:draft', value: DlpDraft): void }>()
const { t } = useI18n()

const dlp = computed(() => props.draft)

function patch(value: Partial<DlpDraft>) {
  emit('update:draft', { ...cloneData(props.draft), ...value })
}

function patchEndpoint(index: number, value: Partial<DlpEndpointDraft>) {
  const endpoints = cloneData(props.draft.endpoints)
  if (!endpoints[index]) return
  endpoints[index] = { ...endpoints[index], ...value }
  patch({ endpoints })
}

function addEndpoint() {
  patch({ endpoints: [...cloneData(props.draft.endpoints), createDefaultDlpEndpoint(props.draft.endpoints.length + 1)] })
}

function removeEndpoint(index: number) {
  patch({ endpoints: cloneData(props.draft.endpoints).filter((_, position) => position !== index) })
}

// 空的 scanners 列表在后端语义里表示"全部启用"，这里如实反映该语义，
// 避免用户看到全部未勾选却实际全开。
function isScannerEnabled(id: string): boolean {
  if (props.draft.scanners.length === 0) return true
  return props.draft.scanners.includes(id)
}

function toggleScanner(id: string) {
  const catalogIds = DLP_SCANNER_CATALOG.map((item) => item.id)
  const current = props.draft.scanners.length === 0 ? [...catalogIds] : [...props.draft.scanners]
  const selected = new Set(current)
  if (selected.has(id)) selected.delete(id)
  else selected.add(id)
  patch({ scanners: catalogIds.filter((item) => selected.has(item)) })
}

function detectorLabel(id: string): string {
  return t(`admin.promptAudit.dlp.detectorLabels.${id}`)
}

function tokenPlaceholder(endpoint: DlpEndpointDraft): string {
  return endpoint.has_token
    ? t('admin.promptAudit.dlp.tokenKeepPlaceholder')
    : t('admin.promptAudit.dlp.tokenEmptyPlaceholder')
}

function tokenStatusLabel(endpoint: DlpEndpointDraft): string {
  return t(`admin.promptAudit.dlp.tokenStatus.${endpoint.token_status}`)
}

function tokenStatusClass(endpoint: DlpEndpointDraft): string {
  if (endpoint.token_status === 'invalid') return 'text-red-600 dark:text-red-400'
  if (endpoint.token_status === 'configured') return 'text-emerald-600 dark:text-emerald-400'
  return 'text-gray-500 dark:text-dark-400'
}
</script>

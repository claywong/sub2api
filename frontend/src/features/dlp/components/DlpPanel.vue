<!--
  DlpPanel.vue
  ============================================================================
  私有扩展（不属于 upstream sub2api）：DLP 敏感信息检测的配置面板。

  与 qwen3guard 内容安全（features/prompt-audit）平级但独立：DLP 走本地正则初筛
  + LLM 二次确认，拥有自己的检测器、确认节点与缓存配置。

  与 upstream 合并策略：
    - 本文件与整个 features/dlp 目录都是私有新增，upstream 侧零改动。
  ============================================================================
-->
<template>
  <section :aria-label="t('admin.dlp.title')" class="py-6">
    <!--
      启用开关移到了底部保存栏（与 qwen3guard 页面一致），所以关掉时不再隐藏整个面板：
      隐藏会让管理员无法在启用前先配好节点与规则，只能"先开着扫再来调"。
    -->
    <p
      v-if="!dlp.enabled"
      role="status"
      data-test="dlp-disabled-notice"
      class="rounded-lg bg-gray-100 px-4 py-3 text-sm text-gray-600 dark:bg-dark-900/50 dark:text-dark-300"
    >
      {{ t('admin.dlp.disabledNotice') }}
    </p>

    <div
      class="grid gap-4 lg:grid-cols-[minmax(0,1fr)_minmax(280px,0.5fr)]"
      :class="{ 'mt-5': !dlp.enabled }"
    >
      <div class="space-y-5 rounded-xl border border-gray-200 p-4 dark:border-dark-700/60 dark:bg-dark-900/20 sm:p-5">
        <fieldset>
          <legend class="text-sm font-medium text-gray-900 dark:text-white">
            {{ t('admin.dlp.scope') }}
          </legend>
          <p class="mt-1 text-xs text-gray-500 dark:text-dark-400">{{ t('admin.dlp.scopeHint') }}</p>
          <div class="mt-3 flex flex-wrap gap-5 text-sm text-gray-700 dark:text-dark-200">
            <label class="flex items-center gap-2">
              <input
                type="radio"
                name="dlp-scope"
                data-test="dlp-scope-all"
                :checked="dlp.all_groups"
                @change="patch({ all_groups: true, group_ids: [] })"
              />
              {{ t('admin.dlp.allGroups') }}
            </label>
            <label class="flex items-center gap-2">
              <input
                type="radio"
                name="dlp-scope"
                data-test="dlp-scope-selected"
                :checked="!dlp.all_groups"
                @change="patch({ all_groups: false })"
              />
              {{ t('admin.dlp.selectedGroups') }}
            </label>
          </div>

          <div v-if="!dlp.all_groups" class="mt-4">
            <label class="block text-sm text-gray-700 dark:text-dark-200">
              <span>{{ t('admin.dlp.searchGroups') }}</span>
              <input
                v-model="groupSearch"
                type="search"
                class="input mt-1.5 w-full"
                :aria-label="t('admin.dlp.searchGroups')"
              />
            </label>
            <div class="mt-3 max-h-52 overflow-y-auto rounded-lg border border-gray-200 p-2 dark:border-dark-700">
              <label
                v-for="group in filteredGroups"
                :key="group.id"
                class="flex cursor-pointer items-center justify-between gap-3 rounded-md px-2 py-2 text-sm hover:bg-gray-50 dark:hover:bg-dark-800"
              >
                <span class="flex items-center gap-2 text-gray-800 dark:text-dark-100">
                  <input
                    type="checkbox"
                    :checked="dlp.group_ids.includes(group.id)"
                    @change="toggleGroup(group.id)"
                  />
                  {{ group.name }}
                </span>
                <span class="text-xs text-gray-500 dark:text-dark-400">{{ group.platform }} · {{ group.status }}</span>
              </label>
              <p v-if="filteredGroups.length === 0" class="px-2 py-4 text-center text-sm text-gray-500">
                {{ t('admin.dlp.noGroups') }}
              </p>
            </div>
            <div
              v-if="missingGroupIds.length"
              class="mt-3 rounded-lg bg-amber-50 px-3 py-2 text-sm text-amber-800 dark:bg-amber-950/30 dark:text-amber-200"
            >
              {{ t('admin.dlp.missingGroups') }}: {{ missingGroupIds.join(', ') }}
            </div>
            <p
              v-if="dlp.group_ids.length === 0"
              class="mt-2 text-xs text-amber-700 dark:text-amber-300"
              data-test="dlp-scope-empty-warning"
            >
              {{ t('admin.dlp.scopeEmptyWarning') }}
            </p>
            <p v-else class="mt-2 text-xs text-gray-500 dark:text-dark-400">
              {{ t('admin.dlp.selectedCount', { count: dlp.group_ids.length }) }}
            </p>
          </div>
        </fieldset>

        <fieldset class="border-t border-gray-100 pt-5 dark:border-dark-800">
          <legend class="text-sm font-medium text-gray-900 dark:text-white">
            {{ t('admin.dlp.detectors') }}
          </legend>
          <p class="mt-1 text-xs text-gray-500 dark:text-dark-400">{{ t('admin.dlp.detectorsHint') }}</p>

          <div class="mt-4 space-y-4">
            <div
              v-for="scanner in DLP_SCANNER_CATALOG"
              :key="scanner.id"
              class="rounded-lg border border-gray-200 dark:border-dark-700"
            >
              <div class="flex flex-wrap items-center justify-between gap-2 border-b border-gray-100 px-3 py-2.5 dark:border-dark-800">
                <label class="flex items-center gap-2 text-sm font-medium text-gray-800 dark:text-dark-100">
                  <input
                    type="checkbox"
                    :checked="isScannerEnabled(scanner.id)"
                    :data-test="`dlp-scanner-${scanner.id}`"
                    :aria-label="detectorLabel(scanner.id)"
                    @change="toggleScanner(scanner.id)"
                  />
                  <span>{{ detectorLabel(scanner.id) }}</span>
                </label>
                <span class="text-xs text-gray-500 dark:text-dark-400">
                  {{ t('admin.dlp.rules.enabledCount', {
                    enabled: countEnabledRules(dlp.rules, scanner.id),
                    total: rulesByScanner(dlp.rules, scanner.id).length,
                  }) }}
                </span>
              </div>

              <!--
                逐条关光等于关掉整个检测器，但界面上勾选框还是选中的，
                不提示的话看起来仍在生效。
              -->
              <p
                v-if="isScannerEnabled(scanner.id) && hasRules(scanner.id) && countEnabledRules(dlp.rules, scanner.id) === 0"
                class="bg-amber-50 px-3 py-2 text-xs text-amber-800 dark:bg-amber-950/30 dark:text-amber-200"
                :data-test="`dlp-scanner-all-disabled-${scanner.id}`"
              >
                {{ t('admin.dlp.rules.allDisabledWarning') }}
              </p>

              <!--
                检测器被整体关掉时规则明细无意义，折叠起来避免误以为还在生效。
              -->
              <ul v-if="isScannerEnabled(scanner.id)" class="divide-y divide-gray-100 dark:divide-dark-800">
                <li
                  v-for="rule in rulesByScanner(dlp.rules, scanner.id)"
                  :key="rule.id"
                  class="flex flex-wrap items-center gap-x-3 gap-y-2 px-3 py-2"
                  :data-test="`dlp-rule-${rule.id}`"
                >
                  <label class="flex min-w-0 flex-1 items-center gap-2 text-sm">
                    <input
                      type="checkbox"
                      :checked="!rule.disabled"
                      :data-test="`dlp-rule-enabled-${rule.id}`"
                      :aria-label="rule.title"
                      @change="toggleRule(rule.id)"
                    />
                    <span
                      class="truncate"
                      :class="rule.disabled ? 'text-gray-400 line-through dark:text-dark-500' : 'text-gray-800 dark:text-dark-100'"
                    >
                      {{ rule.title }}
                    </span>
                    <span
                      v-if="rule.broad"
                      class="shrink-0 rounded bg-amber-100 px-1.5 py-0.5 text-[11px] text-amber-800 dark:bg-amber-950/40 dark:text-amber-200"
                      :title="t('admin.dlp.rules.broadHint')"
                    >
                      {{ t('admin.dlp.rules.broad') }}
                    </span>
                  </label>

                  <div class="flex shrink-0 items-center gap-2">
                    <span
                      v-if="ruleChangedFromDefault(rule)"
                      class="rounded bg-primary-100 px-1.5 py-0.5 text-[11px] text-primary-800 dark:bg-primary-950/40 dark:text-primary-200"
                      :data-test="`dlp-rule-changed-${rule.id}`"
                      :title="t('admin.dlp.rules.changedHint', { severity: severityLabel(rule.default_severity) })"
                    >
                      {{ t('admin.dlp.rules.changed') }}
                    </span>
                    <span
                      class="w-16 text-right text-[11px]"
                      :class="ruleBlocks(dlp, rule) ? 'text-red-600 dark:text-red-400' : 'text-gray-500 dark:text-dark-400'"
                      :data-test="`dlp-rule-effect-${rule.id}`"
                    >
                      {{ rule.disabled
                        ? t('admin.dlp.rules.effectOff')
                        : ruleBlocks(dlp, rule) ? t('admin.dlp.rules.effectBlock') : t('admin.dlp.rules.effectAudit') }}
                    </span>
                    <select
                      class="input w-24 py-1 text-xs"
                      :value="rule.severity"
                      :disabled="rule.disabled"
                      :data-test="`dlp-rule-severity-${rule.id}`"
                      :aria-label="t('admin.dlp.rules.severityFor', { rule: rule.title })"
                      @change="setRuleSeverity(rule.id, ($event.target as HTMLSelectElement).value)"
                    >
                      <option v-for="level in dlp.available_severities" :key="level" :value="level">
                        {{ severityLabel(level) }}
                      </option>
                    </select>
                  </div>
                </li>
              </ul>
            </div>
          </div>
        </fieldset>

        <!--
          开关本身在保存栏，这里只留说明：哪几条算高危需要配合下面的规则表看，
          规则行右侧的「会拦 / 仅记录」已经实时反映了开关与严重度的组合结果。
        -->
        <div class="border-t border-gray-100 pt-5 dark:border-dark-800">
          <h3 class="text-sm font-medium text-gray-900 dark:text-white">
            {{ t('admin.dlp.disposition') }}
          </h3>
          <p class="mt-1 text-xs text-gray-500 dark:text-dark-400" data-test="dlp-block-high-hint">
            {{ t('admin.dlp.blockOnHighHint') }}
          </p>

          <!--
            这个开关不在底部保存栏：它需要一段关于事件表增长的警示说明，
            保存栏放不下解释性文案。
          -->
          <label class="mt-4 flex items-start gap-2 text-sm text-gray-700 dark:text-dark-200">
            <input
              type="checkbox"
              class="mt-1"
              :checked="dlp.record_regex_hits"
              data-test="dlp-record-regex-hits"
              :aria-label="t('admin.dlp.recordRegexHits')"
              @change="patch({ record_regex_hits: ($event.target as HTMLInputElement).checked })"
            />
            <span>
              {{ t('admin.dlp.recordRegexHits') }}
              <span class="mt-0.5 block text-xs text-gray-500 dark:text-dark-400">
                {{ t('admin.dlp.recordRegexHitsHint') }}
              </span>
            </span>
          </label>
          <p
            v-if="dlp.record_regex_hits"
            class="mt-2 rounded-lg bg-amber-50 px-3 py-2 text-xs text-amber-800 dark:bg-amber-950/30 dark:text-amber-200"
            data-test="dlp-record-regex-hits-warning"
          >
            {{ t('admin.dlp.recordRegexHitsWarning') }}
          </p>
        </div>

        <fieldset class="border-t border-gray-100 pt-5 dark:border-dark-800">
          <legend class="text-sm font-medium text-gray-900 dark:text-white">
            {{ t('admin.dlp.confirm') }}
          </legend>
          <p class="mt-1 text-xs text-gray-500 dark:text-dark-400">
            {{ t('admin.dlp.confirmEnabledHint') }}
          </p>

          <div v-if="dlp.confirm_enabled" class="mt-4 space-y-3">
            <label class="block text-sm text-gray-700 dark:text-dark-200">
              <span>{{ t('admin.dlp.confirmTimeout') }}</span>
              <input
                :value="dlp.confirm_timeout_ms"
                type="number"
                min="500"
                max="30000"
                class="input mt-1.5 w-full"
                :aria-label="t('admin.dlp.confirmTimeout')"
                @input="patch({ confirm_timeout_ms: Number(($event.target as HTMLInputElement).value) })"
              />
            </label>
            <p class="rounded-lg bg-amber-50 px-3 py-2 text-xs text-amber-800 dark:bg-amber-950/30 dark:text-amber-200">
              {{ t('admin.dlp.failOpenNotice') }}
            </p>
          </div>
        </fieldset>

        <fieldset class="border-t border-gray-100 pt-5 dark:border-dark-800">
          <legend class="text-sm font-medium text-gray-900 dark:text-white">
            {{ t('admin.dlp.cache') }}
          </legend>
          <p class="mt-1 text-xs text-gray-500 dark:text-dark-400">
            {{ t('admin.dlp.cacheEnabledHint') }}
          </p>
          <div v-if="dlp.cache_enabled && dlp.confirm_enabled" class="mt-4 grid gap-3 sm:grid-cols-2">
            <label class="block text-sm text-gray-700 dark:text-dark-200">
              <span>{{ t('admin.dlp.cacheSensitiveTtl') }}</span>
              <input
                :value="dlp.cache_sensitive_ttl_hours"
                type="number"
                min="0"
                max="720"
                class="input mt-1.5 w-full"
                :aria-label="t('admin.dlp.cacheSensitiveTtl')"
                @input="patch({ cache_sensitive_ttl_hours: Number(($event.target as HTMLInputElement).value) })"
              />
            </label>
            <label class="block text-sm text-gray-700 dark:text-dark-200">
              <span>{{ t('admin.dlp.cacheBenignTtl') }}</span>
              <input
                :value="dlp.cache_benign_ttl_hours"
                type="number"
                min="0"
                max="720"
                class="input mt-1.5 w-full"
                :aria-label="t('admin.dlp.cacheBenignTtl')"
                @input="patch({ cache_benign_ttl_hours: Number(($event.target as HTMLInputElement).value) })"
              />
            </label>
          </div>
        </fieldset>
      </div>

      <div class="space-y-3 rounded-xl border border-gray-200 p-4 dark:border-dark-700/60 dark:bg-dark-900/20 sm:p-5">
        <div class="flex items-center justify-between gap-2">
          <h3 class="text-sm font-medium text-gray-900 dark:text-white">
            {{ t('admin.dlp.endpoints') }}
          </h3>
          <button type="button" class="btn btn-secondary btn-sm" data-test="dlp-add-endpoint" @click="addEndpoint">
            {{ t('admin.dlp.addEndpoint') }}
          </button>
        </div>
        <p class="text-xs text-gray-500 dark:text-dark-400">{{ t('admin.dlp.endpointsHint') }}</p>

        <p
          v-if="dlp.confirm_enabled && dlp.endpoints.length === 0"
          class="rounded-lg bg-red-50 px-3 py-2 text-xs text-red-700 dark:bg-red-950/30 dark:text-red-200"
        >
          {{ t('admin.dlp.endpointRequired') }}
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
                :aria-label="t('admin.dlp.endpointEnabled')"
                @change="patchEndpoint(index, { enabled: ($event.target as HTMLInputElement).checked })"
              />
              <span>{{ t('admin.dlp.endpointEnabled') }}</span>
            </label>
            <button
              type="button"
              class="text-xs text-red-600 hover:underline dark:text-red-400"
              @click="removeEndpoint(index)"
            >
              {{ t('admin.dlp.removeEndpoint') }}
            </button>
          </div>

          <label class="block text-sm text-gray-700 dark:text-dark-200">
            <span>{{ t('admin.dlp.endpointName') }}</span>
            <input
              :value="endpoint.name"
              class="input mt-1 w-full"
              :aria-label="t('admin.dlp.endpointName')"
              @input="patchEndpoint(index, { name: ($event.target as HTMLInputElement).value })"
            />
          </label>
          <label class="block text-sm text-gray-700 dark:text-dark-200">
            <span>{{ t('admin.dlp.endpointBaseUrl') }}</span>
            <input
              :value="endpoint.base_url"
              placeholder="https://api.example.com"
              class="input mt-1 w-full"
              :aria-label="t('admin.dlp.endpointBaseUrl')"
              @input="patchEndpoint(index, { base_url: ($event.target as HTMLInputElement).value })"
            />
          </label>
          <label class="block text-sm text-gray-700 dark:text-dark-200">
            <span>{{ t('admin.dlp.endpointModel') }}</span>
            <input
              :value="endpoint.model"
              :placeholder="DEFAULT_DLP_CONFIRM_MODEL"
              class="input mt-1 w-full"
              :aria-label="t('admin.dlp.endpointModel')"
              @input="patchEndpoint(index, { model: ($event.target as HTMLInputElement).value })"
            />
          </label>
          <label class="block text-sm text-gray-700 dark:text-dark-200">
            <span>{{ t('admin.dlp.endpointTimeout') }}</span>
            <input
              :value="endpoint.timeout_ms"
              type="number"
              min="500"
              max="30000"
              class="input mt-1 w-full"
              :aria-label="t('admin.dlp.endpointTimeout')"
              @input="patchEndpoint(index, { timeout_ms: Number(($event.target as HTMLInputElement).value) })"
            />
          </label>

          <label class="block text-sm text-gray-700 dark:text-dark-200">
            <span>{{ t('admin.dlp.endpointToken') }}</span>
            <input
              :value="endpoint.token"
              type="password"
              autocomplete="off"
              :placeholder="tokenPlaceholder(endpoint)"
              class="input mt-1 w-full"
              :aria-label="t('admin.dlp.endpointToken')"
              @input="patchEndpoint(index, { token: ($event.target as HTMLInputElement).value })"
            />
          </label>
          <div class="flex flex-wrap items-center gap-3 text-xs">
            <span :class="tokenStatusClass(endpoint)">{{ tokenStatusLabel(endpoint) }}</span>
            <label v-if="endpoint.has_token" class="flex items-center gap-1.5 text-gray-600 dark:text-dark-300">
              <input
                type="checkbox"
                :checked="endpoint.clear_token"
                :aria-label="t('admin.dlp.clearToken')"
                @change="patchEndpoint(index, { clear_token: ($event.target as HTMLInputElement).checked })"
              />
              <span>{{ t('admin.dlp.clearToken') }}</span>
            </label>
          </div>
        </div>
      </div>
    </div>
  </section>
</template>

<script setup lang="ts">
import { computed, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import type { PromptAuditGroup } from '@/features/prompt-audit/types'
import type { DlpDraft, DlpEndpointDraft, DlpRule } from '../types'
import {
  cloneDlpData,
  countEnabledRules,
  createDefaultDlpEndpoint,
  DEFAULT_DLP_CONFIRM_MODEL,
  DLP_SCANNER_CATALOG,
  ruleBlocks,
  ruleChangedFromDefault,
  rulesByScanner,
} from '../viewModel'

// groups 收的是与 qwen3guard 同一份分组列表，但选择结果各自独立存储。
const props = defineProps<{ draft: DlpDraft; groups: PromptAuditGroup[] }>()
const emit = defineEmits<{ (event: 'update:draft', value: DlpDraft): void }>()
const { t } = useI18n()
const groupSearch = ref('')

const dlp = computed(() => props.draft)

const filteredGroups = computed(() => {
  const query = groupSearch.value.trim().toLowerCase()
  if (!query) return props.groups
  return props.groups.filter((group) =>
    `${group.name} ${group.id} ${group.platform}`.toLowerCase().includes(query),
  )
})
const knownGroupIds = computed(() => new Set(props.groups.map((group) => group.id)))
// 分组被删除后配置里仍留着它的 ID，要显式提示而不是静默忽略。
const missingGroupIds = computed(() => props.draft.group_ids.filter((id) => !knownGroupIds.value.has(id)))

function patch(value: Partial<DlpDraft>) {
  emit('update:draft', { ...cloneDlpData(props.draft), ...value })
}

function toggleGroup(id: number) {
  const selected = new Set(props.draft.group_ids)
  if (selected.has(id)) selected.delete(id)
  else selected.add(id)
  patch({ group_ids: [...selected].sort((a, b) => a - b) })
}

function patchEndpoint(index: number, value: Partial<DlpEndpointDraft>) {
  const endpoints = cloneDlpData(props.draft.endpoints)
  if (!endpoints[index]) return
  endpoints[index] = { ...endpoints[index], ...value }
  patch({ endpoints })
}

function addEndpoint() {
  patch({ endpoints: [...cloneDlpData(props.draft.endpoints), createDefaultDlpEndpoint(props.draft.endpoints.length + 1)] })
}

function removeEndpoint(index: number) {
  patch({ endpoints: cloneDlpData(props.draft.endpoints).filter((_, position) => position !== index) })
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
  return t(`admin.dlp.detectorLabels.${id}`)
}

// severityLabel 把后端下发的严重度值转成文案。
// 后端将来新增取值时回落到原值，界面不会显示空白。
function severityLabel(level: string): string {
  const key = `admin.dlp.rules.severity.${level}`
  const translated = t(key)
  return translated === key ? level : translated
}

// patchRule 改写单条规则。
//
// 规则列表整体重建而非原地改：props 是只读的响应式代理，直接改会绕过
// update:draft，父组件的脏检查也就发现不了这次改动。
function patchRule(ruleID: string, value: Partial<DlpRule>) {
  patch({
    rules: props.draft.rules.map((rule) =>
      rule.id === ruleID ? { ...rule, ...value } : { ...rule },
    ),
  })
}

// hasRules 判断检测器下是否有规则。
// 后端未下发规则表（接口降级）时不该显示「全部已关闭」这种误导性警告。
function hasRules(scannerID: string): boolean {
  return rulesByScanner(props.draft.rules, scannerID).length > 0
}

function toggleRule(ruleID: string) {
  const target = props.draft.rules.find((rule) => rule.id === ruleID)
  if (!target) return
  patchRule(ruleID, { disabled: !target.disabled })
}

function setRuleSeverity(ruleID: string, severity: string) {
  patchRule(ruleID, { severity })
}

function tokenPlaceholder(endpoint: DlpEndpointDraft): string {
  return endpoint.has_token
    ? t('admin.dlp.tokenKeepPlaceholder')
    : t('admin.dlp.tokenEmptyPlaceholder')
}

function tokenStatusLabel(endpoint: DlpEndpointDraft): string {
  return t(`admin.dlp.tokenStatus.${endpoint.token_status}`)
}

function tokenStatusClass(endpoint: DlpEndpointDraft): string {
  if (endpoint.token_status === 'invalid') return 'text-red-600 dark:text-red-400'
  if (endpoint.token_status === 'configured') return 'text-emerald-600 dark:text-emerald-400'
  return 'text-gray-500 dark:text-dark-400'
}
</script>

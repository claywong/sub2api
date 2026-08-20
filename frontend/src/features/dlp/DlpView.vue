<!--
  DlpView.vue
  ============================================================================
  私有扩展（不属于 upstream sub2api）：数据防泄漏（DLP）管理页面。

  与 features/prompt-audit（qwen3guard 内容安全）拆成两个独立页面，原因：
    - 两者是互不依赖的检测器。DLP 有自己的 enabled、生效分组与处置阈值，
      挂在 prompt-audit 页面时，底部保存栏那几个 qwen3guard 顶层开关会被误当成
      DLP 的开关——关掉「启用提示词审计」并不会关掉 DLP。
    - 事件虽同表，但按 scanner_backend 分流后各看各的（见 api.ts 的 source=dlp）。

  刻意不展示运行态面板：DLP 在 Coordinator.Check 里同步执行，没有 qwen3guard
  那套 worker/队列，运行态指标对它没有意义。

  与 upstream 合并策略：
    - 本文件与整个 features/dlp 目录都是私有新增。复用 prompt-audit 的事件列表、
      详情弹窗与按条件删除弹窗（单向 import），不复制其代码。
  ============================================================================
-->
<template>
  <AppLayout>
    <div class="mx-auto max-w-[1600px]" :class="activeTab === 'config' && draft ? 'pb-28' : 'pb-8'">
      <header class="mb-6 flex flex-wrap items-end justify-between gap-4">
        <div>
          <p class="text-xs font-semibold uppercase tracking-[0.16em] text-primary-600 dark:text-primary-400">
            {{ t('nav.securityAudit') }}
          </p>
          <h1 class="mt-1 text-2xl font-semibold tracking-tight text-gray-950 dark:text-white">
            {{ t('admin.dlp.title') }}
          </h1>
          <p class="mt-2 max-w-3xl text-sm text-gray-500 dark:text-dark-300">{{ t('admin.dlp.description') }}</p>
        </div>
        <div v-if="draft" class="text-right text-xs text-gray-500 dark:text-dark-400">
          <p>{{ t('admin.dlp.configVersion', { version: draft.config_version }) }}</p>
          <p v-if="draft.updated_at" class="mt-1">{{ formatDate(draft.updated_at) }}</p>
        </div>
      </header>

      <div
        v-if="loadErrors.config && !draft"
        role="alert"
        class="rounded-xl border border-red-200 bg-red-50 p-5 dark:border-red-900 dark:bg-red-950/30"
      >
        <p class="text-sm text-red-700 dark:text-red-300">{{ loadErrors.config }}</p>
        <button type="button" class="btn btn-secondary btn-sm mt-3" @click="loadConfig">
          {{ t('admin.dlp.actions.retry') }}
        </button>
      </div>

      <template v-else>
        <div class="mb-4" role="tablist" :aria-label="t('admin.dlp.title')">
          <div class="tabs inline-flex">
            <button
              v-for="tab in pageTabs"
              :key="tab.id"
              type="button"
              role="tab"
              class="tab"
              :class="{ 'tab-active': activeTab === tab.id }"
              :aria-selected="activeTab === tab.id"
              :data-test="`tab-${tab.id}`"
              @click="activeTab = tab.id"
            >
              {{ tab.label }}
            </button>
          </div>
        </div>

        <main class="card px-4 sm:px-6 lg:px-8">
          <div v-show="activeTab === 'config'" data-test="tab-panel-config">
            <div v-if="loadErrors.groups" role="alert" class="mt-5 rounded-lg bg-amber-50 px-4 py-3 text-sm text-amber-800 dark:bg-amber-950/30 dark:text-amber-200">
              {{ loadErrors.groups }}
            </div>
            <DlpPanel v-if="draft" :draft="draft.dlp" :groups="groups" @update:draft="replaceDlpDraft" />
          </div>

          <div v-show="activeTab === 'events'" data-test="tab-panel-events">
            <EventWorkspace
              :events="events.items"
              :total="events.total"
              :page="events.page"
              :page-size="events.page_size"
              :filters="filters"
              :selected-ids="selectedEventIds"
              :loading="loading.events"
              :error="loadErrors.events"
              @filters-change="handleFiltersChanged"
              @search="applyEventFilters"
              @selection="selectedEventIds = $event"
              @page="changePage"
              @page-size="changePageSize"
              @view="openEvent"
              @delete="requestSingleDelete"
              @batch-delete="requestBatchDelete"
              @preview-delete="requestFilterDeletePreview"
            />
          </div>
        </main>
      </template>
    </div>

    <!--
      保存栏只放保存/重置：DLP 的启用开关与处置阈值都在 DlpPanel 内部，
      不再像 prompt-audit 页面那样把 qwen3guard 的顶层开关摆在这里。
    -->
    <div
      v-if="draft && activeTab === 'config'"
      class="fixed inset-x-0 bottom-0 z-30 border-t border-gray-200 bg-white/95 px-4 py-3 shadow-[0_-12px_35px_rgba(15,23,42,0.08)] backdrop-blur dark:border-dark-700/80 dark:bg-dark-900/95 dark:shadow-[0_-12px_35px_rgba(0,0,0,0.35)] lg:left-64"
    >
      <div class="mx-auto flex max-w-[1600px] flex-wrap items-center justify-end gap-3">
        <span class="text-sm" :class="dirty ? 'text-amber-700 dark:text-amber-300' : 'text-gray-500 dark:text-dark-400'">
          {{ dirty ? t('admin.dlp.saveBar.dirty') : t('admin.dlp.saveBar.synced') }}
        </span>
        <button type="button" class="btn btn-secondary" :disabled="!dirty || loading.saving" @click="resetDraft">
          {{ t('common.reset') }}
        </button>
        <button
          type="button"
          class="btn btn-primary"
          :disabled="!dirty || loading.saving"
          data-test="save-config"
          @click="saveConfig"
        >
          {{ loading.saving ? t('common.saving') : t('common.save') }}
        </button>
      </div>
    </div>

    <ConfirmDialog
      :show="deleteRequest.mode !== ''"
      :title="t('admin.dlp.events.deleteConfirmTitle')"
      :message="t('admin.dlp.events.deleteConfirmMessage', { count: deleteRequest.ids.length })"
      :confirm-text="t('common.delete')"
      danger
      @confirm="confirmIDDelete"
      @cancel="clearDeleteRequest"
    />
    <FilterDeleteDialog
      :show="showFilterDelete"
      :initial-filters="filters"
      :preview="deletePreview"
      :previewing="loading.previewing"
      :deleting="loading.deleting"
      @close="closeFilterDelete"
      @preview="runFilterDeletePreview"
      @confirm="confirmFilterDelete"
      @criteria-change="clearDeletePreview"
    />
    <EventDetailDialog :show="showEventDetail" :event="activeEvent" :loading="loading.detail" @close="closeEventDetail" />
  </AppLayout>
</template>
<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import AppLayout from '@/components/layout/AppLayout.vue'
import ConfirmDialog from '@/components/common/ConfirmDialog.vue'
import { useAppStore } from '@/stores/app'
import { extractApiErrorCode, extractApiErrorMessage } from '@/utils/apiError'
// 复用 upstream 的事件列表与弹窗：DLP 事件与 qwen3guard 事件同表同结构，
// 差别只在筛选条件，没有理由复制一份 400 行的表格实现。
import EventWorkspace from '@/features/prompt-audit/components/EventWorkspace.vue'
import EventDetailDialog from '@/features/prompt-audit/components/EventDetailDialog.vue'
import FilterDeleteDialog from '@/features/prompt-audit/components/FilterDeleteDialog.vue'
import type {
  PromptAuditEvent,
  PromptAuditGroup,
  PromptDeletePreview,
  PromptEventFilters,
  PromptEventPage,
} from '@/features/prompt-audit/types'
import { cloneData, emptyEventFilters } from '@/features/prompt-audit/viewModel'
import DlpPanel from './components/DlpPanel.vue'
import dlpAPI from './api'
import type { DlpDraft, DlpLoadErrors, DlpPageDraft } from './types'
import { buildConfigUpdateRequest, dlpDraftFingerprint, responseToPageDraft } from './viewModel'

const { t, locale } = useI18n()
const appStore = useAppStore()

type DlpPageTab = 'config' | 'events'
const activeTab = ref<DlpPageTab>('events')
const pageTabs = computed(() => [
  { id: 'events' as const, label: t('admin.dlp.tabs.events') },
  { id: 'config' as const, label: t('admin.dlp.tabs.config') },
])

const serverConfig = ref<DlpPageDraft | null>(null)
const draft = ref<DlpPageDraft | null>(null)
const groups = ref<PromptAuditGroup[]>([])
const events = reactive<PromptEventPage>({ items: [], total: 0, page: 1, page_size: 20, pages: 0 })
const filters = ref<PromptEventFilters>(emptyEventFilters())
const appliedFilters = ref<PromptEventFilters>(emptyEventFilters())
const selectedEventIds = ref<number[]>([])
const activeEvent = ref<PromptAuditEvent | null>(null)
const showEventDetail = ref(false)
const showFilterDelete = ref(false)
const deletePreview = ref<PromptDeletePreview | null>(null)
const deletePreviewFilters = ref<PromptEventFilters | null>(null)
const deleteRequest = reactive<{ mode: '' | 'single' | 'batch'; ids: number[] }>({ mode: '', ids: [] })
const loading = reactive({
  config: false, groups: false, events: false, saving: false, detail: false, deleting: false, previewing: false,
})
const loadErrors = reactive<DlpLoadErrors>({ config: '', groups: '', events: '' })

// 指纹只覆盖 dlp 子树：guard 字段在本页面不可编辑，纳入比较会让别处改动
// 误报成「本页面有未保存修改」。
const dirty = computed(() => dlpDraftFingerprint(draft.value) !== dlpDraftFingerprint(serverConfig.value))

function errorMessage(error: unknown, fallbackKey: string): string {
  const code = extractApiErrorCode(error)
  if (code) {
    // 错误码文案优先取 DLP 自己的表，缺失时回落到 upstream 的提示词审计文案，
    // 因为两者共用同一套配置接口，后端会抛出同一批错误码。
    for (const prefix of ['admin.dlp.errors.', 'admin.promptAudit.errors.']) {
      const key = `${prefix}${code}`
      const translated = t(key)
      if (translated !== key) return translated
    }
  }
  return extractApiErrorMessage(error, t(fallbackKey))
}

async function loadConfig() {
  loading.config = true
  loadErrors.config = ''
  try {
    const config = await dlpAPI.getConfig()
    serverConfig.value = responseToPageDraft(config)
    draft.value = responseToPageDraft(config)
  } catch (error) {
    loadErrors.config = errorMessage(error, 'admin.dlp.errors.loadConfig')
  } finally {
    loading.config = false
  }
}

async function loadGroups() {
  loading.groups = true
  loadErrors.groups = ''
  try {
    groups.value = await dlpAPI.listGroups()
  } catch (error) {
    loadErrors.groups = errorMessage(error, 'admin.dlp.errors.loadGroups')
  } finally {
    loading.groups = false
  }
}

async function loadEvents() {
  loading.events = true
  loadErrors.events = ''
  try {
    const result = await dlpAPI.listEvents(appliedFilters.value, events.page, events.page_size)
    Object.assign(events, result)
    selectedEventIds.value = []
  } catch (error) {
    loadErrors.events = errorMessage(error, 'admin.dlp.errors.loadEvents')
  } finally {
    loading.events = false
  }
}

async function loadInitial() {
  await Promise.allSettled([loadConfig(), loadGroups(), loadEvents()])
}

function replaceDlpDraft(value: DlpDraft) {
  if (!draft.value) return
  draft.value = { ...draft.value, dlp: cloneData(value) }
}

function resetDraft() {
  if (serverConfig.value) draft.value = cloneData(serverConfig.value)
}

async function saveConfig() {
  if (!draft.value || !dirty.value) return
  loading.saving = true
  try {
    // buildConfigUpdateRequest 会原样带回 qwen3guard 字段，
    // 否则后端会把缺失字段解成零值并清空其配置（见 viewModel 的注释）。
    const saved = await dlpAPI.updateConfig(buildConfigUpdateRequest(draft.value))
    serverConfig.value = responseToPageDraft(saved)
    draft.value = responseToPageDraft(saved)
    appStore.showSuccess(t('admin.dlp.messages.saved'))
  } catch (error) {
    const code = extractApiErrorCode(error)
    appStore.showError(
      errorMessage(
        error,
        code === 'prompt_audit_config_conflict' ? 'admin.dlp.errors.conflict' : 'admin.dlp.errors.saveConfig',
      ),
    )
  } finally {
    loading.saving = false
  }
}

function handleFiltersChanged(value: PromptEventFilters) {
  filters.value = cloneData(value)
  clearDeletePreview()
}

function applyEventFilters(value: PromptEventFilters) {
  filters.value = cloneData(value)
  appliedFilters.value = cloneData(value)
  events.page = 1
  clearDeletePreview()
  void loadEvents()
}

function changePage(value: number) {
  events.page = value
  void loadEvents()
}

function changePageSize(value: number) {
  events.page_size = value
  events.page = 1
  void loadEvents()
}

async function openEvent(id: number) {
  showEventDetail.value = true
  loading.detail = true
  activeEvent.value = null
  try {
    activeEvent.value = await dlpAPI.getEvent(id)
  } catch (error) {
    appStore.showError(errorMessage(error, 'admin.dlp.errors.loadDetail'))
    showEventDetail.value = false
  } finally {
    loading.detail = false
  }
}

function closeEventDetail() {
  showEventDetail.value = false
  activeEvent.value = null
}

function requestSingleDelete(id: number) {
  deleteRequest.mode = 'single'
  deleteRequest.ids = [id]
}

function requestBatchDelete() {
  if (selectedEventIds.value.length) {
    deleteRequest.mode = 'batch'
    deleteRequest.ids = [...selectedEventIds.value]
  }
}

function clearDeleteRequest() {
  deleteRequest.mode = ''
  deleteRequest.ids = []
}

async function confirmIDDelete() {
  const mode = deleteRequest.mode
  const ids = [...deleteRequest.ids]
  clearDeleteRequest()
  if (!mode || ids.length === 0) return
  loading.deleting = true
  try {
    const result = mode === 'single' ? await dlpAPI.deleteEvent(ids[0]) : await dlpAPI.batchDeleteEvents(ids)
    appStore.showSuccess(t('admin.dlp.messages.deleted', { count: result.deleted_events }))
    await loadEvents()
  } catch (error) {
    appStore.showError(errorMessage(error, 'admin.dlp.errors.delete'))
  } finally {
    loading.deleting = false
  }
}

function clearDeletePreview() {
  deletePreview.value = null
  deletePreviewFilters.value = null
}

function requestFilterDeletePreview() {
  clearDeletePreview()
  showFilterDelete.value = true
}

function closeFilterDelete() {
  showFilterDelete.value = false
  clearDeletePreview()
}

async function runFilterDeletePreview(value: PromptEventFilters) {
  loading.previewing = true
  try {
    deletePreview.value = await dlpAPI.previewDelete(value)
    deletePreviewFilters.value = cloneData(value)
  } catch (error) {
    clearDeletePreview()
    appStore.showError(errorMessage(error, 'admin.dlp.errors.previewDelete'))
  } finally {
    loading.previewing = false
  }
}

async function confirmFilterDelete(criteria?: PromptEventFilters) {
  if (loading.deleting) return
  loading.deleting = true
  try {
    let preview = deletePreview.value
    let previewFilters = deletePreviewFilters.value ? cloneData(deletePreviewFilters.value) : null
    // 一键路径：没有可用预览（从未请求过，或条件变更后被清掉）时，
    // 用弹窗刚发出的条件即时换一个确认 token，再在同一个动作里删除。
    if ((!preview || !previewFilters) && criteria) {
      preview = await dlpAPI.previewDelete(criteria)
      previewFilters = cloneData(criteria)
    }
    if (!preview || !previewFilters) return
    const result = await dlpAPI.deleteEventsByFilter(previewFilters, preview)
    closeFilterDelete()
    appStore.showSuccess(t('admin.dlp.messages.deleted', { count: result.deleted_events }))
    await loadEvents()
  } catch (error) {
    clearDeletePreview()
    appStore.showError(errorMessage(error, 'admin.dlp.errors.deleteConfirmation'))
  } finally {
    loading.deleting = false
  }
}

function formatDate(value: string): string {
  return new Intl.DateTimeFormat(locale.value, { dateStyle: 'medium', timeStyle: 'medium' }).format(new Date(value))
}

onMounted(loadInitial)
</script>


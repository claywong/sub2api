<!--
  飞书离职自动禁用 —— 配置面板 + 执行历史（私有扩展，不属于 upstream sub2api）

  该功能会自动禁用用户账号，属于破坏性操作，UI 上刻意放大风险可见性：
  - dry-run 开关默认开启，说明文字明确「只记录不真正禁用」
  - 「立即执行一次」强制二次确认，确认框内默认勾选 dry-run
  - 执行历史中 disabled_count > 0 与熔断记录均有醒目标识

  merge 策略：纯新增文件，upstream 不存在同名文件，永不冲突。

  @author wangzhong
-->
<template>
  <section class="space-y-6">
    <!-- 配置区 -->
    <div class="card">
      <div class="border-b border-gray-100 px-6 py-4 dark:border-dark-700">
        <h3 class="text-base font-medium text-gray-900 dark:text-white">
          {{ t("admin.settings.feishuOffboard.title") }}
        </h3>
        <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">
          {{ t("admin.settings.feishuOffboard.description") }}
        </p>
      </div>

      <div v-if="configLoading" class="px-6 py-10 text-center text-sm text-gray-400">
        <span class="animate-pulse">{{ t("admin.settings.feishuOffboard.loading") }}</span>
      </div>

      <div v-else class="space-y-5 px-6 py-6">
        <!-- 后端未接线提示 -->
        <div
          v-if="serviceUnavailable"
          class="rounded-xl border border-gray-200 bg-gray-50/80 px-4 py-3 text-sm text-gray-600 dark:border-dark-600 dark:bg-dark-800/50 dark:text-gray-300"
          role="status"
        >
          <p class="flex items-start gap-2">
            <Icon name="infoCircle" size="sm" class="mt-0.5 shrink-0" />
            <span>{{ t("admin.settings.feishuOffboard.serviceUnavailable") }}</span>
          </p>
        </div>

        <!-- 风险提示 -->
        <div
          class="rounded-xl border border-amber-200 bg-amber-50/80 px-4 py-3 text-sm text-amber-900 dark:border-amber-800/50 dark:bg-amber-900/20 dark:text-amber-100"
          role="note"
        >
          <p class="flex items-start gap-2">
            <Icon name="exclamationTriangle" size="sm" class="mt-0.5 shrink-0" />
            <span>{{ t("admin.settings.feishuOffboard.riskNotice") }}</span>
          </p>
        </div>

        <!-- 总开关 -->
        <div class="flex items-start justify-between gap-4">
          <div class="min-w-0">
            <label class="mb-0 block text-sm font-medium text-gray-700 dark:text-gray-300">
              {{ t("admin.settings.feishuOffboard.enabled") }}
            </label>
            <p class="mt-1 text-xs text-gray-500 dark:text-gray-400">
              {{ t("admin.settings.feishuOffboard.enabledHint") }}
            </p>
          </div>
          <Toggle v-model="form.enabled" />
        </div>

        <!-- cron 表达式 -->
        <div>
          <label class="mb-2 block text-sm font-medium text-gray-700 dark:text-gray-300">
            {{ t("admin.settings.feishuOffboard.schedule") }}
          </label>
          <div class="flex flex-wrap items-center gap-2">
            <input
              v-model.trim="form.schedule"
              type="text"
              class="input font-mono text-sm sm:max-w-xs"
              :placeholder="DEFAULT_SCHEDULE"
            />
            <span class="badge badge-gray whitespace-nowrap">
              {{ t("admin.settings.feishuOffboard.scheduleDefaultHint") }}
            </span>
          </div>
          <p class="mt-1.5 text-xs text-gray-500 dark:text-gray-400">
            {{ t("admin.settings.feishuOffboard.scheduleHint") }}
          </p>
        </div>

        <!-- 飞书 App ID -->
        <div>
          <label class="mb-2 block text-sm font-medium text-gray-700 dark:text-gray-300">
            {{ t("admin.settings.feishuOffboard.appId") }}
          </label>
          <input
            v-model.trim="form.app_id"
            type="text"
            class="input font-mono text-sm"
            :placeholder="t('admin.settings.feishuOffboard.appIdPlaceholder')"
          />
          <p class="mt-1.5 text-xs text-gray-500 dark:text-gray-400">
            {{ t("admin.settings.feishuOffboard.appIdHint") }}
          </p>
        </div>

        <!-- 飞书 App Secret：后端不回显明文，留空表示不修改 -->
        <div>
          <label class="mb-2 flex flex-wrap items-center gap-2 text-sm font-medium text-gray-700 dark:text-gray-300">
            {{ t("admin.settings.feishuOffboard.appSecret") }}
            <span
              class="badge whitespace-nowrap"
              :class="secretConfigured ? 'badge-success' : 'badge-gray'"
            >
              {{
                secretConfigured
                  ? t("admin.settings.feishuOffboard.secretConfigured")
                  : t("admin.settings.feishuOffboard.secretNotConfigured")
              }}
            </span>
          </label>
          <input
            v-model="form.app_secret"
            type="password"
            autocomplete="new-password"
            class="input font-mono text-sm"
            :placeholder="t('admin.settings.feishuOffboard.appSecretPlaceholder')"
          />
          <p class="mt-1.5 text-xs text-gray-500 dark:text-gray-400">
            {{
              secretConfigured
                ? t("admin.settings.feishuOffboard.appSecretConfiguredHint")
                : t("admin.settings.feishuOffboard.appSecretHint")
            }}
          </p>
        </div>

        <!-- dry-run 开关 -->
        <div
          class="flex items-start justify-between gap-4 rounded-xl border border-gray-200 bg-gray-50/70 px-4 py-3 dark:border-dark-600 dark:bg-dark-800/50"
        >
          <div class="min-w-0">
            <label class="mb-0 block text-sm font-medium text-gray-700 dark:text-gray-300">
              {{ t("admin.settings.feishuOffboard.dryRun") }}
            </label>
            <p class="mt-1 text-xs text-gray-500 dark:text-gray-400">
              {{ t("admin.settings.feishuOffboard.dryRunHint") }}
            </p>
          </div>
          <Toggle v-model="form.dry_run" />
        </div>

        <!-- 熔断阈值 -->
        <div>
          <label class="mb-2 block text-sm font-medium text-gray-700 dark:text-gray-300">
            {{ t("admin.settings.feishuOffboard.circuitBreaker") }}
          </label>
          <input
            v-model.number="form.circuit_breaker_threshold"
            type="number"
            min="1"
            step="1"
            class="input sm:max-w-[12rem]"
          />
          <p class="mt-1.5 text-xs text-gray-500 dark:text-gray-400">
            {{ t("admin.settings.feishuOffboard.circuitBreakerHint") }}
          </p>
        </div>

        <!-- 通知邮箱 -->
        <div>
          <label class="mb-2 block text-sm font-medium text-gray-700 dark:text-gray-300">
            {{ t("admin.settings.feishuOffboard.notifyTo") }}
          </label>
          <div class="space-y-2">
            <div
              v-for="(entry, index) in notifyEmails"
              :key="index"
              class="flex items-center gap-2"
            >
              <input
                v-model.trim="entry.email"
                type="email"
                class="input flex-1"
                :placeholder="t('admin.settings.feishuOffboard.notifyToPlaceholder')"
              />
              <button
                type="button"
                class="btn btn-secondary px-2"
                :aria-label="t('admin.settings.feishuOffboard.removeEmail')"
                @click="removeNotifyEmail(index)"
              >
                <Icon name="x" size="xs" class="h-4 w-4" />
              </button>
            </div>
            <button type="button" class="btn btn-secondary btn-sm" @click="addNotifyEmail">
              + {{ t("admin.settings.feishuOffboard.addEmail") }}
            </button>
          </div>
          <p class="mt-1.5 text-xs text-gray-500 dark:text-gray-400">
            {{ t("admin.settings.feishuOffboard.notifyToHint") }}
          </p>
        </div>

        <!-- 操作按钮 -->
        <div class="flex flex-wrap items-center gap-3 border-t border-gray-100 pt-5 dark:border-dark-700">
          <button
            type="button"
            class="btn btn-primary"
            :disabled="saving"
            @click="saveConfig"
          >
            <Icon v-if="!saving" name="check" size="sm" />
            {{ saving ? t("admin.settings.saving") : t("admin.settings.feishuOffboard.save") }}
          </button>
          <button
            type="button"
            class="btn btn-secondary"
            :disabled="testing"
            @click="testConnection"
          >
            <Icon name="link" size="sm" />
            {{ testing ? t("admin.settings.feishuOffboard.testing") : t("admin.settings.feishuOffboard.test") }}
          </button>
          <button
            type="button"
            class="btn btn-danger"
            :disabled="running"
            @click="openRunConfirm"
          >
            <Icon name="play" size="sm" />
            {{ running ? t("admin.settings.feishuOffboard.running") : t("admin.settings.feishuOffboard.runNow") }}
          </button>
        </div>
      </div>
    </div>

    <!-- 执行历史区 -->
    <div class="card">
      <div
        class="flex flex-wrap items-center justify-between gap-3 border-b border-gray-100 px-6 py-4 dark:border-dark-700"
      >
        <div class="min-w-0">
          <h3 class="text-base font-medium text-gray-900 dark:text-white">
            {{ t("admin.settings.feishuOffboard.history.title") }}
          </h3>
          <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">
            {{ t("admin.settings.feishuOffboard.history.description") }}
          </p>
        </div>
        <button
          type="button"
          class="btn btn-secondary btn-sm"
          :disabled="runsLoading"
          @click="loadRuns"
        >
          <Icon name="refresh" size="sm" />
          {{ t("admin.settings.feishuOffboard.history.refresh") }}
        </button>
      </div>

      <div v-if="runsLoading" class="px-6 py-10 text-center text-sm text-gray-400">
        <span class="animate-pulse">{{ t("admin.settings.feishuOffboard.loading") }}</span>
      </div>

      <div v-else-if="runs.length === 0" class="px-6 py-10 text-center text-sm text-gray-400">
        {{ t("admin.settings.feishuOffboard.history.empty") }}
      </div>

      <div v-else class="overflow-x-auto">
        <table class="min-w-full text-sm">
          <thead
            class="bg-gray-50 text-xs uppercase text-gray-500 dark:bg-dark-800 dark:text-dark-400"
          >
            <tr>
              <th class="whitespace-nowrap px-4 py-2 text-left">
                {{ t("admin.settings.feishuOffboard.history.columns.runAt") }}
              </th>
              <th class="whitespace-nowrap px-4 py-2 text-left">
                {{ t("admin.settings.feishuOffboard.history.columns.trigger") }}
              </th>
              <th class="whitespace-nowrap px-4 py-2 text-left">
                {{ t("admin.settings.feishuOffboard.history.columns.mode") }}
              </th>
              <th class="whitespace-nowrap px-4 py-2 text-right">
                {{ t("admin.settings.feishuOffboard.history.columns.checked") }}
              </th>
              <th class="whitespace-nowrap px-4 py-2 text-right">
                {{ t("admin.settings.feishuOffboard.history.columns.resigned") }}
              </th>
              <th class="whitespace-nowrap px-4 py-2 text-right">
                {{ t("admin.settings.feishuOffboard.history.columns.disabled") }}
              </th>
              <th class="whitespace-nowrap px-4 py-2 text-right">
                {{ t("admin.settings.feishuOffboard.history.columns.unverifiable") }}
              </th>
              <th class="whitespace-nowrap px-4 py-2 text-left">
                {{ t("admin.settings.feishuOffboard.history.columns.circuit") }}
              </th>
              <th class="whitespace-nowrap px-4 py-2 text-right">
                {{ t("admin.settings.feishuOffboard.history.columns.duration") }}
              </th>
              <th class="whitespace-nowrap px-4 py-2 text-right">
                {{ t("admin.settings.feishuOffboard.history.columns.actions") }}
              </th>
            </tr>
          </thead>
          <tbody class="divide-y divide-gray-200 dark:divide-dark-700">
            <tr
              v-for="run in runs"
              :key="run.id"
              class="cursor-pointer transition-colors hover:bg-gray-50 dark:hover:bg-dark-800/60"
              :class="run.circuit_broken ? 'bg-red-50/60 dark:bg-red-900/10' : ''"
              @click="openRunDetail(run)"
            >
              <td class="whitespace-nowrap px-4 py-2 text-gray-900 dark:text-white">
                {{ formatDateTime(run.run_at) }}
              </td>
              <td class="whitespace-nowrap px-4 py-2">
                <span class="badge badge-gray">{{ triggerLabel(run.trigger_source) }}</span>
              </td>
              <td class="whitespace-nowrap px-4 py-2">
                <span class="badge" :class="run.dry_run ? 'badge-primary' : 'badge-warning'">
                  {{
                    run.dry_run
                      ? t("admin.settings.feishuOffboard.history.modeDryRun")
                      : t("admin.settings.feishuOffboard.history.modeReal")
                  }}
                </span>
              </td>
              <td class="whitespace-nowrap px-4 py-2 text-right text-gray-600 dark:text-gray-300">
                {{ run.checked_count }}
              </td>
              <td
                class="whitespace-nowrap px-4 py-2 text-right"
                :class="
                  run.resigned_count > 0
                    ? 'font-semibold text-amber-600 dark:text-amber-400'
                    : 'text-gray-600 dark:text-gray-300'
                "
              >
                {{ run.resigned_count }}
              </td>
              <!-- 真的有人被停用：最醒目 -->
              <td
                class="whitespace-nowrap px-4 py-2 text-right"
                :class="
                  run.disabled_count > 0
                    ? 'font-bold text-red-600 dark:text-red-400'
                    : 'text-gray-600 dark:text-gray-300'
                "
              >
                {{ run.disabled_count }}
              </td>
              <td class="whitespace-nowrap px-4 py-2 text-right text-gray-600 dark:text-gray-300">
                {{ run.unverifiable_count }}
              </td>
              <td class="whitespace-nowrap px-4 py-2">
                <span
                  v-if="run.circuit_broken"
                  class="badge badge-danger"
                  :title="t('admin.settings.feishuOffboard.history.circuitBrokenHint')"
                >
                  <Icon name="ban" size="xs" class="h-3 w-3" />
                  {{ t("admin.settings.feishuOffboard.history.circuitBroken") }}
                </span>
                <span v-else class="text-gray-400">-</span>
              </td>
              <td class="whitespace-nowrap px-4 py-2 text-right text-gray-600 dark:text-gray-300">
                {{ formatDuration(run.duration_ms) }}
              </td>
              <td class="whitespace-nowrap px-4 py-2 text-right">
                <button
                  type="button"
                  class="btn btn-secondary btn-sm"
                  @click.stop="openRunDetail(run)"
                >
                  {{ t("admin.settings.feishuOffboard.history.viewDetail") }}
                </button>
              </td>
            </tr>
          </tbody>
        </table>
      </div>

      <!-- 熔断说明：只要历史中出现过熔断就常驻提示 -->
      <div
        v-if="hasCircuitBroken"
        class="border-t border-gray-100 px-6 py-3 text-xs text-red-600 dark:border-dark-700 dark:text-red-400"
      >
        {{ t("admin.settings.feishuOffboard.history.circuitBrokenHint") }}
      </div>
    </div>

    <!-- 立即执行确认框：破坏性操作，确认框内可选择 dry-run -->
    <ConfirmDialog
      :show="runConfirmVisible"
      :title="t('admin.settings.feishuOffboard.runConfirm.title')"
      :message="t('admin.settings.feishuOffboard.runConfirm.message')"
      :confirm-text="t('admin.settings.feishuOffboard.runConfirm.confirm')"
      :danger="!runConfirmDryRun"
      @cancel="runConfirmVisible = false"
      @confirm="confirmRunNow"
    >
      <label
        class="flex cursor-pointer items-start gap-3 rounded-xl border border-gray-200 bg-gray-50/70 px-4 py-3 dark:border-dark-600 dark:bg-dark-800/50"
      >
        <input
          v-model="runConfirmDryRun"
          type="checkbox"
          class="mt-0.5 h-4 w-4 rounded border-gray-300 text-primary-600 focus:ring-primary-500/40"
        />
        <span class="min-w-0">
          <span class="block text-sm font-medium text-gray-700 dark:text-gray-300">
            {{ t("admin.settings.feishuOffboard.runConfirm.dryRunLabel") }}
          </span>
          <span class="mt-1 block text-xs text-gray-500 dark:text-gray-400">
            {{ t("admin.settings.feishuOffboard.runConfirm.dryRunHint") }}
          </span>
        </span>
      </label>
      <p
        v-if="!runConfirmDryRun"
        class="rounded-xl border border-red-200 bg-red-50/80 px-4 py-3 text-sm font-medium text-red-700 dark:border-red-800/50 dark:bg-red-900/20 dark:text-red-300"
      >
        {{ t("admin.settings.feishuOffboard.runConfirm.realWarning") }}
      </p>
    </ConfirmDialog>

    <!-- 判定明细弹窗 -->
    <BaseDialog
      :show="detailVisible"
      :title="t('admin.settings.feishuOffboard.detail.title')"
      width="extra-wide"
      @close="closeRunDetail"
    >
      <div v-if="detailLoading" class="py-10 text-center text-sm text-gray-400">
        <span class="animate-pulse">{{ t("admin.settings.feishuOffboard.loading") }}</span>
      </div>

      <div v-else-if="detailRun" class="space-y-4">
        <!-- 汇总 -->
        <div class="grid grid-cols-2 gap-3 sm:grid-cols-4">
          <div
            v-for="stat in detailStats"
            :key="stat.key"
            class="rounded-xl border border-gray-200 px-3 py-2 dark:border-dark-600"
          >
            <p class="text-xs text-gray-500 dark:text-gray-400">{{ stat.label }}</p>
            <p class="mt-0.5 text-lg font-semibold" :class="stat.valueClass">{{ stat.value }}</p>
          </div>
        </div>

        <div class="flex flex-wrap items-center gap-2 text-xs text-gray-500 dark:text-gray-400">
          <span>{{ formatDateTime(detailRun.run_at) }}</span>
          <span class="badge badge-gray">{{ triggerLabel(detailRun.trigger_source) }}</span>
          <span class="badge" :class="detailRun.dry_run ? 'badge-primary' : 'badge-warning'">
            {{
              detailRun.dry_run
                ? t("admin.settings.feishuOffboard.history.modeDryRun")
                : t("admin.settings.feishuOffboard.history.modeReal")
            }}
          </span>
          <span v-if="detailRun.circuit_broken" class="badge badge-danger">
            {{ t("admin.settings.feishuOffboard.history.circuitBroken") }}
          </span>
        </div>

        <p
          v-if="detailRun.circuit_broken"
          class="rounded-xl border border-red-200 bg-red-50/80 px-4 py-3 text-sm text-red-700 dark:border-red-800/50 dark:bg-red-900/20 dark:text-red-300"
        >
          {{ t("admin.settings.feishuOffboard.history.circuitBrokenHint") }}
        </p>

        <p
          v-if="detailRun.error_message"
          class="rounded-xl border border-red-200 bg-red-50/80 px-4 py-3 text-sm text-red-700 dark:border-red-800/50 dark:bg-red-900/20 dark:text-red-300"
        >
          {{ detailRun.error_message }}
        </p>

        <!-- 每人判定明细 -->
        <div v-if="detailDecisions.length" class="overflow-x-auto">
          <table class="min-w-full text-sm">
            <thead
              class="bg-gray-50 text-xs uppercase text-gray-500 dark:bg-dark-800 dark:text-dark-400"
            >
              <tr>
                <th class="whitespace-nowrap px-3 py-2 text-left">
                  {{ t("admin.settings.feishuOffboard.detail.columns.user") }}
                </th>
                <th class="whitespace-nowrap px-3 py-2 text-left">
                  {{ t("admin.settings.feishuOffboard.detail.columns.verdict") }}
                </th>
                <th class="whitespace-nowrap px-3 py-2 text-left">
                  {{ t("admin.settings.feishuOffboard.detail.columns.feishu") }}
                </th>
                <th class="whitespace-nowrap px-3 py-2 text-left">
                  {{ t("admin.settings.feishuOffboard.detail.columns.flags") }}
                </th>
                <th class="whitespace-nowrap px-3 py-2 text-right">
                  {{ t("admin.settings.feishuOffboard.detail.columns.candidates") }}
                </th>
                <th class="whitespace-nowrap px-3 py-2 text-left">
                  {{ t("admin.settings.feishuOffboard.detail.columns.disabled") }}
                </th>
                <th class="px-3 py-2 text-left">
                  {{ t("admin.settings.feishuOffboard.detail.columns.reason") }}
                </th>
              </tr>
            </thead>
            <tbody class="divide-y divide-gray-200 align-top dark:divide-dark-700">
              <tr
                v-for="decision in detailDecisions"
                :key="decision.user_id"
                :class="decision.verdict === 'resigned' ? 'bg-red-50/50 dark:bg-red-900/10' : ''"
              >
                <td class="px-3 py-2">
                  <p class="font-medium text-gray-900 dark:text-white">
                    {{ decision.username || "-" }}
                  </p>
                  <p class="text-xs text-gray-500 dark:text-gray-400">{{ decision.email }}</p>
                </td>
                <td class="whitespace-nowrap px-3 py-2">
                  <span class="badge" :class="verdictBadgeClass(decision.verdict)">
                    {{ verdictLabel(decision.verdict) }}
                  </span>
                </td>
                <td class="px-3 py-2 text-xs text-gray-600 dark:text-gray-300">
                  <p v-if="decision.feishu_name">{{ decision.feishu_name }}</p>
                  <p v-if="decision.employee_no" class="font-mono text-gray-400">
                    {{ decision.employee_no }}
                  </p>
                  <p v-if="!decision.feishu_name && !decision.employee_no" class="text-gray-400">
                    -
                  </p>
                </td>
                <!-- 飞书原始状态位：判定依据摊开给人工复核 -->
                <td class="px-3 py-2">
                  <template v-if="decision.feishu_flags">
                    <div class="flex flex-wrap gap-1">
                      <span
                        v-for="flag in flagChips(decision.feishu_flags)"
                        :key="flag.key"
                        class="badge whitespace-nowrap"
                        :class="flag.badgeClass"
                        :title="flag.title"
                      >
                        {{ flag.label }}
                      </span>
                    </div>
                    <!-- 离职后仍激活是正常现象，不解释的话复核的人会以为判错了 -->
                    <p
                      v-if="showsActivatedNote(decision.feishu_flags)"
                      class="mt-1 text-xs text-gray-500 dark:text-gray-400"
                    >
                      {{ t("admin.settings.feishuOffboard.detail.activatedNote") }}
                    </p>
                  </template>
                  <span v-else class="text-xs text-gray-400">
                    {{ t("admin.settings.feishuOffboard.detail.flagsMissing") }}
                  </span>
                </td>
                <td
                  class="whitespace-nowrap px-3 py-2 text-right"
                  :class="
                    decision.candidate_count > 1
                      ? 'font-semibold text-amber-600 dark:text-amber-400'
                      : 'text-gray-600 dark:text-gray-300'
                  "
                >
                  {{ decision.candidate_count }}
                </td>
                <td class="whitespace-nowrap px-3 py-2">
                  <span v-if="decision.disabled" class="badge badge-danger">
                    {{ t("admin.settings.feishuOffboard.detail.disabledYes") }}
                  </span>
                  <span v-else class="text-gray-400">-</span>
                </td>
                <td class="px-3 py-2 text-xs text-gray-600 dark:text-gray-300">
                  <p>{{ decision.reason || "-" }}</p>
                  <!-- 多候选 + 已离职：说明系统做过 enterprise_email 精确甄别，
                       否则复核的人会怀疑禁错了人（邮箱回收复用占比约 5.3%） -->
                  <p
                    v-if="showsMultiCandidateNote(decision)"
                    class="mt-0.5 text-amber-600 dark:text-amber-400"
                  >
                    {{ t("admin.settings.feishuOffboard.detail.multiCandidateNote") }}
                  </p>
                  <p v-if="decision.disable_error" class="mt-0.5 text-red-600 dark:text-red-400">
                    {{ decision.disable_error }}
                  </p>
                </td>
              </tr>
            </tbody>
          </table>
        </div>

        <p v-else class="py-6 text-center text-sm text-gray-400">
          {{ t("admin.settings.feishuOffboard.detail.empty") }}
        </p>
      </div>

      <template #footer>
        <div class="flex justify-end">
          <button type="button" class="btn btn-secondary" @click="closeRunDetail">
            {{ t("common.close") }}
          </button>
        </div>
      </template>
    </BaseDialog>
  </section>
</template>

<script setup lang="ts">
import { computed, onMounted, reactive, ref } from "vue";
import { useI18n } from "vue-i18n";
import BaseDialog from "@/components/common/BaseDialog.vue";
import ConfirmDialog from "@/components/common/ConfirmDialog.vue";
import Toggle from "@/components/common/Toggle.vue";
import Icon from "@/components/icons/Icon.vue";
import { useAppStore } from "@/stores";
import { extractApiErrorMessage } from "@/utils/apiError";
import { formatDateTime } from "@/utils/format";
import {
  defaultFeishuOffboardConfig,
  feishuOffboardAPI,
  type FeishuOffboardConfig,
  type FeishuOffboardDecision,
  type FeishuOffboardRun,
  type FeishuOffboardTriggerSource,
  type FeishuOffboardVerdict,
  type FeishuUserStatusFlags,
} from "@/api/admin/feishuOffboard";

const { t } = useI18n();
const appStore = useAppStore();

const DEFAULT_SCHEDULE = "0 1 * * *";
const HISTORY_PAGE_SIZE = 20;

// ── 配置表单 ────────────────────────────────────────────────
const configLoading = ref(true);
const saving = ref(false);
const testing = ref(false);
const running = ref(false);
/** 后端是否已存密钥；表单里的 app_secret 留空即不修改 */
const secretConfigured = ref(false);
/** 后端服务未接线（503）：面板照常渲染，但顶部给出明确提示 */
const serviceUnavailable = ref(false);

const form = reactive({
  enabled: false,
  schedule: DEFAULT_SCHEDULE,
  app_id: "",
  /** 留空 = 不修改后端已存密钥 */
  app_secret: "",
  dry_run: true,
  circuit_breaker_threshold: 15,
});

/**
 * 通知邮箱用对象数组维护：直接用 string[] 时 v-model 需要写回数组下标，
 * 包一层 { email } 才能让每行输入框稳定绑定。
 */
const notifyEmails = ref<{ email: string }[]>([]);

// ── 执行历史 ────────────────────────────────────────────────
const runsLoading = ref(true);
const runs = ref<FeishuOffboardRun[]>([]);
const hasCircuitBroken = computed(() => runs.value.some((run) => run.circuit_broken));

// ── 明细弹窗 ────────────────────────────────────────────────
const detailVisible = ref(false);
const detailLoading = ref(false);
const detailRun = ref<FeishuOffboardRun | null>(null);
const detailDecisions = computed<FeishuOffboardDecision[]>(
  () => detailRun.value?.decisions ?? [],
);

// ── 立即执行确认 ────────────────────────────────────────────
const runConfirmVisible = ref(false);
/** 默认勾选 dry-run：更安全 */
const runConfirmDryRun = ref(true);

/** 明细弹窗顶部的汇总卡片 */
const detailStats = computed(() => {
  const run = detailRun.value;
  if (!run) return [];
  return [
    {
      key: "checked",
      label: t("admin.settings.feishuOffboard.history.columns.checked"),
      value: run.checked_count,
      valueClass: "text-gray-900 dark:text-white",
    },
    {
      key: "resigned",
      label: t("admin.settings.feishuOffboard.history.columns.resigned"),
      value: run.resigned_count,
      valueClass:
        run.resigned_count > 0
          ? "text-amber-600 dark:text-amber-400"
          : "text-gray-900 dark:text-white",
    },
    {
      key: "disabled",
      label: t("admin.settings.feishuOffboard.history.columns.disabled"),
      value: run.disabled_count,
      valueClass:
        run.disabled_count > 0
          ? "text-red-600 dark:text-red-400"
          : "text-gray-900 dark:text-white",
    },
    {
      key: "unverifiable",
      label: t("admin.settings.feishuOffboard.history.columns.unverifiable"),
      value: run.unverifiable_count,
      valueClass: "text-gray-900 dark:text-white",
    },
  ];
});

function triggerLabel(source: FeishuOffboardTriggerSource): string {
  return t(`admin.settings.feishuOffboard.trigger.${source}`);
}

function verdictLabel(verdict: FeishuOffboardVerdict): string {
  return t(`admin.settings.feishuOffboard.verdict.${verdict}`);
}

/** verdict 颜色区分，resigned 用 danger 最醒目 */
function verdictBadgeClass(verdict: FeishuOffboardVerdict): string {
  const classes: Record<FeishuOffboardVerdict, string> = {
    resigned: "badge-danger",
    in_service: "badge-success",
    frozen: "badge-warning",
    unverifiable: "badge-gray",
    skip_admin: "badge-primary",
  };
  return classes[verdict] ?? "badge-gray";
}

/**
 * 飞书状态位的展示顺序：is_resigned 与 is_activated 刻意并排放最前。
 *
 * 判定只认 is_resigned / is_exited，不看 is_activated，而已离职员工普遍是
 * is_resigned=true 且 is_activated 仍为 true。把两者同时摊开，复核的人才能
 * 自己确认判定用的是正确的字段，而不是只能相信后端。
 */
const FLAG_ORDER: {
  key: keyof FeishuUserStatusFlags;
  /** 是否为判定依据：命中时标红加粗 */
  decisive: boolean;
}[] = [
  { key: "is_resigned", decisive: true },
  { key: "is_activated", decisive: false },
  { key: "is_exited", decisive: true },
  { key: "is_frozen", decisive: false },
  { key: "is_unjoin", decisive: false },
];

/** 状态位 chip：命中判定的位标红加粗，其余用中性色 */
function flagChips(flags: FeishuUserStatusFlags) {
  return FLAG_ORDER.map(({ key, decisive }) => {
    const active = flags[key] === true;
    return {
      key,
      label: `${t(`admin.settings.feishuOffboard.flags.${key}`)}：${
        active
          ? t("admin.settings.feishuOffboard.flags.yes")
          : t("admin.settings.feishuOffboard.flags.no")
      }`,
      badgeClass:
        active && decisive
          ? "badge-danger font-bold"
          : active
            ? "badge-gray"
            : "badge-gray opacity-60",
      title: t(`admin.settings.feishuOffboard.flags.${key}Hint`),
    };
  });
}

/** 已离职但仍激活：需要额外解释，否则会被误读成判错 */
function showsActivatedNote(flags: FeishuUserStatusFlags): boolean {
  return flags.is_resigned === true && flags.is_activated === true;
}

/** 多候选且判定为已离职时，说明系统做过 enterprise_email 精确甄别 */
function showsMultiCandidateNote(decision: FeishuOffboardDecision): boolean {
  return decision.candidate_count > 1 && decision.verdict === "resigned";
}

function formatDuration(ms: number): string {
  if (!Number.isFinite(ms) || ms < 0) return "-";
  return ms < 1000 ? `${ms}ms` : `${(ms / 1000).toFixed(1)}s`;
}

function addNotifyEmail(): void {
  notifyEmails.value.push({ email: "" });
}

function removeNotifyEmail(index: number): void {
  notifyEmails.value.splice(index, 1);
}

/** 提交前清洗邮箱：去空、去重 */
function sanitizedNotifyTo(): string[] {
  const cleaned = notifyEmails.value
    .map((entry) => entry.email.trim())
    .filter(Boolean);
  return [...new Set(cleaned)];
}

// ── 数据加载 ────────────────────────────────────────────────

async function loadConfig(): Promise<void> {
  configLoading.value = true;
  try {
    const config = await feishuOffboardAPI.getConfig();
    applyConfig(config);
    serviceUnavailable.value = false;
  } catch (error) {
    // 后端接口未就绪时用默认值兜底，保证面板可见可编辑
    applyConfig(defaultFeishuOffboardConfig());
    // 503 = 服务未接线，用顶部横幅说明而不是弹错误 toast
    if (isServiceUnavailable(error)) {
      serviceUnavailable.value = true;
      return;
    }
    appStore.showError(
      extractApiErrorMessage(error, t("admin.settings.feishuOffboard.loadFailed")),
    );
  } finally {
    configLoading.value = false;
  }
}

/** 判定是否为「服务未接线」的 503 */
function isServiceUnavailable(error: unknown): boolean {
  const status = (error as { response?: { status?: number } } | null)?.response?.status;
  return status === 503;
}

function applyConfig(config: FeishuOffboardConfig): void {
  form.enabled = config.enabled === true;
  form.schedule = config.schedule || DEFAULT_SCHEDULE;
  form.app_id = config.app_id || "";
  form.dry_run = config.dry_run === true;
  form.circuit_breaker_threshold = normalizeThreshold(config.circuit_breaker_threshold);
  // 密钥永不回显，保存成功后清空输入框
  form.app_secret = "";
  secretConfigured.value = config.app_secret_configured === true;
  notifyEmails.value = Array.isArray(config.notify_to)
    ? config.notify_to.map((email) => ({ email }))
    : [];
}

function normalizeThreshold(value: unknown): number {
  const parsed = Number(value);
  return Number.isFinite(parsed) && parsed >= 1 ? Math.trunc(parsed) : 15;
}

async function loadRuns(): Promise<void> {
  runsLoading.value = true;
  try {
    const response = await feishuOffboardAPI.getRuns({
      page: 1,
      page_size: HISTORY_PAGE_SIZE,
    });
    runs.value = response.items;
  } catch (error) {
    runs.value = [];
    // 服务未接线时 loadConfig 已给出横幅，这里不再重复报错
    if (isServiceUnavailable(error)) {
      return;
    }
    appStore.showError(
      extractApiErrorMessage(error, t("admin.settings.feishuOffboard.history.loadFailed")),
    );
  } finally {
    runsLoading.value = false;
  }
}

// ── 操作 ────────────────────────────────────────────────────

async function saveConfig(): Promise<void> {
  saving.value = true;
  try {
    const config = await feishuOffboardAPI.updateConfig({
      enabled: form.enabled,
      schedule: form.schedule.trim() || DEFAULT_SCHEDULE,
      app_id: form.app_id.trim(),
      // 空字符串 = 保持后端已存密钥不变
      app_secret: form.app_secret,
      dry_run: form.dry_run,
      circuit_breaker_threshold: normalizeThreshold(form.circuit_breaker_threshold),
      notify_to: sanitizedNotifyTo(),
    });
    applyConfig(config);
    appStore.showSuccess(t("admin.settings.feishuOffboard.saveSuccess"));
  } catch (error) {
    appStore.showError(
      extractApiErrorMessage(error, t("admin.settings.feishuOffboard.saveFailed")),
    );
  } finally {
    saving.value = false;
  }
}

async function testConnection(): Promise<void> {
  testing.value = true;
  try {
    // 后端成功时返回 { ok: true }，失败走 HTTP 错误码
    await feishuOffboardAPI.testConnection();
    appStore.showSuccess(t("admin.settings.feishuOffboard.testSuccess"));
  } catch (error) {
    appStore.showError(
      extractApiErrorMessage(error, t("admin.settings.feishuOffboard.testFailed")),
    );
  } finally {
    testing.value = false;
  }
}

function openRunConfirm(): void {
  // 每次打开都重置为 dry-run，避免沿用上次的真实执行选择
  runConfirmDryRun.value = true;
  runConfirmVisible.value = true;
}

async function confirmRunNow(): Promise<void> {
  const dryRun = runConfirmDryRun.value;
  runConfirmVisible.value = false;
  running.value = true;
  try {
    const { run, summary } = await feishuOffboardAPI.runNow(dryRun);
    // 用 summary.dry_run 而非请求值：系统配置可能强制 dry-run，
    // 此时请求传 false 也不会真禁，提示语必须跟着实际模式走。
    appStore.showSuccess(
      summary.dry_run
        ? t("admin.settings.feishuOffboard.runDryRunSuccess", {
            resigned: summary.resigned_count,
          })
        : t("admin.settings.feishuOffboard.runSuccess", {
            disabled: summary.disabled_count,
          }),
    );
    await loadRuns();
    // 直接展开本次结果，方便立刻核对判定
    await openRunDetail(run);
  } catch (error) {
    appStore.showError(
      extractApiErrorMessage(error, t("admin.settings.feishuOffboard.runFailed")),
    );
  } finally {
    running.value = false;
  }
}

async function openRunDetail(run: FeishuOffboardRun): Promise<void> {
  detailRun.value = run;
  detailVisible.value = true;

  // 列表接口不带 decisions，需要单独拉详情
  if (run.decisions && run.decisions.length > 0) return;

  detailLoading.value = true;
  try {
    detailRun.value = await feishuOffboardAPI.getRun(run.id);
  } catch (error) {
    appStore.showError(
      extractApiErrorMessage(error, t("admin.settings.feishuOffboard.detail.loadFailed")),
    );
  } finally {
    detailLoading.value = false;
  }
}

function closeRunDetail(): void {
  detailVisible.value = false;
  detailRun.value = null;
}

onMounted(() => {
  void loadConfig();
  void loadRuns();
});
</script>

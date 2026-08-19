/**
 * 飞书离职自动禁用 —— 管理端 API（私有扩展，不属于 upstream sub2api）
 *
 * 后端每天凌晨按 cron 查询飞书在职状态，命中「已离职」的用户自动禁用其
 * sub2api 账号。本文件封装配置读写、连通性测试、手动触发与执行历史查询。
 *
 * merge 策略：本文件为纯新增文件，upstream 不存在同名文件，永不冲突。
 *
 * @author wangzhong
 */

import { apiClient } from "../client";

const BASE_PATH = "/admin/feishu-offboard";

// ==================== 配置 ====================

/**
 * 配置读取响应。
 * 注意：后端永不返回 app_secret 明文，只通过 app_secret_configured 表达「是否已配置」。
 */
export interface FeishuOffboardConfig {
  /** 总开关：关闭后 cron 任务不执行 */
  enabled: boolean;
  /** cron 表达式，默认 "0 1 * * *"（每天 01:00） */
  schedule: string;
  /** 飞书自建应用 App ID */
  app_id: string;
  /** 飞书 App Secret 是否已配置（后端不回显明文） */
  app_secret_configured: boolean;
  /** 演练模式：只记录判定结果，不真正禁用账号 */
  dry_run: boolean;
  /** 安全熔断阈值：单次命中离职数超过此值时只告警不执行 */
  circuit_breaker_threshold: number;
  /** 执行结果通知邮箱 */
  notify_to: string[];
}

/**
 * 配置更新请求。
 * app_secret 传空字符串表示「保持后端已存的密钥不变」。
 */
export interface UpdateFeishuOffboardConfigRequest {
  enabled: boolean;
  schedule: string;
  app_id: string;
  /** 空字符串 = 不修改已存密钥 */
  app_secret: string;
  dry_run: boolean;
  circuit_breaker_threshold: number;
  notify_to: string[];
}

// ==================== 执行记录 ====================

/** 触发方式：cron 定时 / manual 管理员手动 */
export type FeishuOffboardTriggerSource = "cron" | "manual";

/**
 * 单个用户的判定结论：
 * - resigned      已离职（会被禁用）
 * - in_service    在职
 * - frozen        需人工判断（飞书状态为暂停/冻结等）
 * - unverifiable  无法核实（飞书查不到或邮箱对不上），不会被禁用
 * - skip_admin    跳过管理员
 */
export type FeishuOffboardVerdict =
  | "resigned"
  | "in_service"
  | "frozen"
  | "unverifiable"
  | "skip_admin";

/**
 * 飞书返回的用户状态位（原始值，用于人工复核「凭什么判定为离职」）。
 *
 * 判定只认 is_resigned / is_exited，不看 is_activated：实测已离职员工
 * 普遍是 is_resigned=true 而 is_activated 仍为 true，两者并不互斥。
 */
export interface FeishuUserStatusFlags {
  /** 已离职 */
  is_resigned: boolean;
  /** 已主动退出租户 */
  is_exited: boolean;
  /** 已冻结/暂停 */
  is_frozen: boolean;
  /** 已激活（离职后仍可能为 true，不作为判定依据） */
  is_activated: boolean;
  /** 未加入（待确认入职） */
  is_unjoin: boolean;
}

/** 单个用户的判定明细 */
export interface FeishuOffboardDecision {
  user_id: number;
  email: string;
  username: string;
  verdict: FeishuOffboardVerdict;
  /** 判定依据说明 */
  reason: string;
  feishu_open_id?: string;
  feishu_name?: string;
  employee_no?: string;
  /**
   * 飞书返回的原始状态位。为 null/undefined 表示飞书没返回状态
   * （通讯录查不到），此时判定必然是 unverifiable，不会禁用。
   */
  feishu_flags?: FeishuUserStatusFlags | null;
  /** 飞书侧匹配到的候选人数（>1 表示存在歧义） */
  candidate_count: number;
  /** 本次是否真的执行了禁用（dry-run 下恒为 false） */
  disabled: boolean;
  /** 禁用失败时的错误信息 */
  disable_error?: string;
}

/** 一次执行的汇总记录 */
export interface FeishuOffboardRun {
  id: number;
  /** 执行开始时间 */
  run_at: string;
  trigger_source: FeishuOffboardTriggerSource;
  dry_run: boolean;
  /** 本次检查的用户数 */
  checked_count: number;
  /** 判定为已离职的人数 */
  resigned_count: number;
  /** 实际被禁用的账号数 */
  disabled_count: number;
  /** 无法核实的人数 */
  unverifiable_count: number;
  /** 跳过的人数（如管理员） */
  skipped_count: number;
  /** 是否触发安全熔断（命中过多，已阻止执行） */
  circuit_broken: boolean;
  duration_ms: number;
  error_message?: string;
  /** 每人判定明细，仅详情接口返回 */
  decisions?: FeishuOffboardDecision[];
}

/** 执行历史查询参数 */
export interface FeishuOffboardRunsQuery {
  page?: number;
  page_size?: number;
}

/**
 * 执行历史列表。
 * 后端可能直接返回数组，也可能返回分页对象；统一归一化为该结构。
 */
export interface FeishuOffboardRunsResponse {
  items: FeishuOffboardRun[];
  total: number;
}

/**
 * 连通性测试结果。
 * 后端成功时返回 { ok: true }，失败走 HTTP 错误码（由调用方 catch）。
 */
export interface FeishuOffboardTestResult {
  ok: boolean;
}

/**
 * 手动触发的响应：后端把执行记录包在 run 里，另给一份 summary 汇总，
 * 便于前端一眼看到「是不是演练」「到底禁了几个人」。
 */
export interface FeishuOffboardRunResponse {
  run: FeishuOffboardRun;
  summary: FeishuOffboardRunSummary;
}

/** 本次执行的判定汇总（字段与 run 同源，后端为破坏性操作单独给出） */
export interface FeishuOffboardRunSummary {
  /** 实际生效的模式：系统配置可能强制 dry-run，请求传 false 也不会真禁 */
  dry_run: boolean;
  circuit_broken: boolean;
  checked_count: number;
  resigned_count: number;
  disabled_count: number;
  unverifiable_count: number;
  skipped_count: number;
  duration_ms: number;
  error_message?: string;
}

// ==================== 接口 ====================

/** 读取飞书离职自动禁用配置 */
export async function getFeishuOffboardConfig(): Promise<FeishuOffboardConfig> {
  const { data } = await apiClient.get<FeishuOffboardConfig>(
    `${BASE_PATH}/config`,
  );
  return data;
}

/** 更新配置（app_secret 传空字符串表示不修改） */
export async function updateFeishuOffboardConfig(
  config: UpdateFeishuOffboardConfigRequest,
): Promise<FeishuOffboardConfig> {
  const { data } = await apiClient.put<FeishuOffboardConfig>(
    `${BASE_PATH}/config`,
    config,
  );
  return data;
}

/** 测试飞书应用凭据连通性 */
export async function testFeishuOffboardConnection(): Promise<FeishuOffboardTestResult> {
  const { data } = await apiClient.post<FeishuOffboardTestResult>(
    `${BASE_PATH}/test`,
  );
  return data;
}

/**
 * 立即执行一次离职核查。
 * 破坏性操作：dry_run 为 false 时会真正禁用命中的账号。
 */
export async function runFeishuOffboardNow(
  dryRun: boolean,
): Promise<FeishuOffboardRunResponse> {
  const { data } = await apiClient.post<FeishuOffboardRunResponse>(
    `${BASE_PATH}/run`,
    { dry_run: dryRun },
  );
  return data;
}

/** 查询执行历史 */
export async function getFeishuOffboardRuns(
  params: FeishuOffboardRunsQuery = {},
): Promise<FeishuOffboardRunsResponse> {
  const { data } = await apiClient.get(`${BASE_PATH}/runs`, { params });
  return normalizeRunsResponse(data);
}

/** 查询某次执行的判定明细 */
export async function getFeishuOffboardRun(
  id: number,
): Promise<FeishuOffboardRun> {
  const { data } = await apiClient.get<FeishuOffboardRun>(
    `${BASE_PATH}/runs/${id}`,
  );
  return data;
}

// ==================== 归一化辅助 ====================

/**
 * 兼容后端可能的三种返回形态：裸数组、{ items, total }、{ runs, total }。
 * 避免前端在后端定型前写死一种结构。
 */
function normalizeRunsResponse(payload: unknown): FeishuOffboardRunsResponse {
  if (Array.isArray(payload)) {
    return { items: payload as FeishuOffboardRun[], total: payload.length };
  }

  const raw = (payload ?? {}) as Record<string, unknown>;
  const list = [raw.items, raw.runs, raw.data].find((value) =>
    Array.isArray(value),
  );
  const items = (Array.isArray(list) ? list : []) as FeishuOffboardRun[];
  const total =
    typeof raw.total === "number" && Number.isFinite(raw.total)
      ? raw.total
      : items.length;

  return { items, total };
}

/** 默认配置：后端未就绪或读取失败时用于表单初始化 */
export function defaultFeishuOffboardConfig(): FeishuOffboardConfig {
  return {
    enabled: false,
    schedule: "0 1 * * *",
    app_id: "",
    app_secret_configured: false,
    dry_run: true,
    circuit_breaker_threshold: 15,
    notify_to: [],
  };
}

export const feishuOffboardAPI = {
  getConfig: getFeishuOffboardConfig,
  updateConfig: updateFeishuOffboardConfig,
  testConnection: testFeishuOffboardConnection,
  runNow: runFeishuOffboardNow,
  getRuns: getFeishuOffboardRuns,
  getRun: getFeishuOffboardRun,
};

export default feishuOffboardAPI;

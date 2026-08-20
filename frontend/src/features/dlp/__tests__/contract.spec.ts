// contract.spec.ts
// ============================================================================
// 私有扩展（不属于 upstream sub2api）：DLP 配置接口的前后端契约测试。
//
// 本文件读取的 fixture 由后端测试生成（见 prompt_config_dlp_contract_test.go），
// 是 publicDLPFromStorage 真实序列化的字节，而不是手写的。
//
// 为什么必须这样验一遍：
//   本目录其余测试用手写 fixture，后端测试也用手写 fixture，两边都绿并不能
//   说明字段名对得上。规则表下发功能上线后界面显示「0/0 条规则已启用」，
//   就是这类断点——两侧单测全绿，接的却不是同一份结构。
// ============================================================================
import { describe, expect, it } from 'vitest'
import backendConfig from './fixtures/dlpConfig.backend.json'
import { DLP_SCANNER_CATALOG, countEnabledRules, dlpConfigToDraft, rulesByScanner } from '../viewModel'
import type { DlpConfig } from '../types'

// 后端的 JSON 直接当作 DlpConfig 使用：这一步就是契约本身，
// 若字段名/层级对不上，下面的断言会失败而不是静默降级。
const config = backendConfig as unknown as DlpConfig

describe('DLP config contract with the backend', () => {
  it('parses the backend payload into a non-empty rule table', () => {
    const draft = dlpConfigToDraft(config)
    // 界面显示 0/0 的直接症状就是这里为空。
    expect(draft.rules.length).toBeGreaterThan(0)
    expect(draft.rules.length).toBe(config.rules.length)
  })

  it('matches every rule to a detector the UI can render', () => {
    const draft = dlpConfigToDraft(config)
    const catalogIDs = DLP_SCANNER_CATALOG.map((scanner) => scanner.id)

    // 后端下发的 scanner_id 必须落在前端目录里，否则规则会归到没有界面入口的
    // 分组，表现为检测器展开后一条都没有。
    for (const rule of draft.rules) {
      expect(catalogIDs).toContain(rule.scanner_id)
    }

    // 反向也要成立：每个检测器都得分到规则，否则那一栏永远是 0/0。
    for (const scannerID of catalogIDs) {
      expect(rulesByScanner(draft.rules, scannerID).length).toBeGreaterThan(0)
    }
  })

  it('counts enabled rules per detector instead of reporting 0/0', () => {
    const draft = dlpConfigToDraft(config)
    for (const scanner of DLP_SCANNER_CATALOG) {
      const total = rulesByScanner(draft.rules, scanner.id).length
      const enabled = countEnabledRules(draft.rules, scanner.id)
      expect(total).toBeGreaterThan(0)
      expect(enabled).toBeGreaterThan(0)
      expect(enabled).toBeLessThanOrEqual(total)
    }
  })

  it('carries each rule field the table renders', () => {
    const draft = dlpConfigToDraft(config)
    for (const rule of draft.rules) {
      // 标题来自后端，缺失会让界面显示空行。
      expect(rule.title).toBeTruthy()
      expect(rule.severity).toBeTruthy()
      expect(rule.default_severity).toBeTruthy()
      expect(typeof rule.disabled).toBe('boolean')
      expect(typeof rule.broad).toBe('boolean')
    }
  })

  it('receives the admin overrides rather than defaults only', () => {
    // fixture 里刻意包含一条改过严重度、一条被关掉的规则（后端测试保证）。
    // 这两个字段传不过来的话，管理员的改动看起来会像没保存。
    const draft = dlpConfigToDraft(config)
    expect(draft.rules.some((rule) => rule.severity !== rule.default_severity)).toBe(true)
    expect(draft.rules.some((rule) => rule.disabled)).toBe(true)
  })

  it('receives the severity vocabulary the selector needs', () => {
    const draft = dlpConfigToDraft(config)
    // 空数组会让严重度选择器渲染不出任何选项。
    expect(draft.available_severities.length).toBeGreaterThan(0)
    // 每条规则的当前严重度都必须是可选项之一，否则 select 显示空白。
    for (const rule of draft.rules) {
      expect(draft.available_severities).toContain(rule.severity)
    }
  })

  it('receives the blocking threshold so the effect column is computable', () => {
    const draft = dlpConfigToDraft(config)
    // 阈值为空时每条规则都会算成「仅记录」，掩盖真实的拦截行为。
    expect(draft.blocking_severities.length).toBeGreaterThan(0)
  })

  it('carries every top-level switch the panel and save bar bind to', () => {
    // 少一个字段前端就读到 undefined，开关会静默显示成关闭——而且保存时会把
    // 后端的真实值覆盖掉。
    const draft = dlpConfigToDraft(config)
    for (const key of [
      'enabled', 'confirm_enabled', 'cache_enabled',
      'block_on_high_severity', 'record_regex_hits',
    ] as const) {
      expect(typeof draft[key]).toBe('boolean')
    }
  })

  it('keeps the detector list in sync with the backend', () => {
    // 后端新增检测器但前端目录没跟上时，那个检测器的规则在界面上不可见。
    const backendIDs = config.available_scanners.map((scanner) => scanner.id).sort()
    const frontendIDs = DLP_SCANNER_CATALOG.map((scanner) => scanner.id).sort()
    expect(frontendIDs).toEqual(backendIDs)
  })
})

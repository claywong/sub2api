// 私有扩展（不属于 upstream sub2api）。
//
// 文件作用：ProtectedModelQuota（共享额度）在 service 层（*ProtectedModelQuota）
//   与 ent 层（map[string]interface{}）之间的双向转换工具函数。
//   ent 仍使用 JSON map 存储（约定 key 为 sharedQuotaKey），避免 schema 变更。
//   被 group_repo.go（写入）和 api_key_repo.go（读取）共同调用。
//
// merge 策略：全新文件，无 upstream 冲突。

package repository

import (
	"encoding/json"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// sharedQuotaKey 是 ent JSON map 中存放共享额度的唯一 key。
const sharedQuotaKey = "*"

// toRawQuota 将 service 层的 *ProtectedModelQuota 转换为 ent 所需的 map[string]interface{}。
// nil 输入返回空 map（避免 JSON null）。
func toRawQuota(q *service.ProtectedModelQuota) map[string]interface{} {
	if q == nil {
		return map[string]interface{}{}
	}
	b, err := json.Marshal(q)
	if err != nil {
		return map[string]interface{}{}
	}
	var inner map[string]interface{}
	if err := json.Unmarshal(b, &inner); err != nil {
		return map[string]interface{}{}
	}
	return map[string]interface{}{sharedQuotaKey: inner}
}

// fromRawQuota 将 ent 层读取到的 map[string]interface{} 转换回 *ProtectedModelQuota。
// 若 raw 为空或无有效条目，返回 nil（等同于未配置额度）。
// 兼容老数据：若没有 sharedQuotaKey 但有其他 key，取首个条目作为共享额度。
func fromRawQuota(raw map[string]interface{}) *service.ProtectedModelQuota {
	if len(raw) == 0 {
		return nil
	}
	v, ok := raw[sharedQuotaKey]
	if !ok {
		for _, vv := range raw {
			v = vv
			break
		}
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var q service.ProtectedModelQuota
	if err := json.Unmarshal(b, &q); err != nil {
		return nil
	}
	return &q
}

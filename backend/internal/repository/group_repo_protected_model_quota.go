// 私有扩展（不属于 upstream sub2api）。
//
// 文件作用：ProtectedModelQuotas 在 service 层（map[string]ProtectedModelQuota）
//   与 ent 层（map[string]interface{}）之间的双向转换工具函数。
//   被 group_repo.go（写入）和 api_key_repo.go（读取）共同调用。
//
// merge 策略：全新文件，无 upstream 冲突。

package repository

import (
	"encoding/json"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// toRawQuotaMap 将 service 层的 ProtectedModelQuotas 转换为 ent 所需的 map[string]interface{}。
// ent 使用 JSON 序列化写入 DB；nil 输入返回空 map（不返回 nil，避免 JSON null）。
func toRawQuotaMap(quotas map[string]service.ProtectedModelQuota) map[string]interface{} {
	if len(quotas) == 0 {
		return map[string]interface{}{}
	}
	raw := make(map[string]interface{}, len(quotas))
	for pattern, q := range quotas {
		// 通过 JSON 往返确保字段名一致（使用 ProtectedModelQuota 的 json tag）
		b, _ := json.Marshal(q)
		var m map[string]interface{}
		_ = json.Unmarshal(b, &m)
		raw[pattern] = m
	}
	return raw
}

// fromRawQuotaMap 将 ent 层读取到的 map[string]interface{} 转换回 service 层类型。
// 若 raw 为空或转换失败，返回 nil（等同于未配置额度）。
func fromRawQuotaMap(raw map[string]interface{}) map[string]service.ProtectedModelQuota {
	if len(raw) == 0 {
		return nil
	}
	result := make(map[string]service.ProtectedModelQuota, len(raw))
	for pattern, v := range raw {
		b, err := json.Marshal(v)
		if err != nil {
			continue
		}
		var q service.ProtectedModelQuota
		if err := json.Unmarshal(b, &q); err != nil {
			continue
		}
		result[pattern] = q
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

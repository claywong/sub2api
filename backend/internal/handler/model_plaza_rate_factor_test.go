//go:build unit

package handler

import (
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// 单模型倍率系数只在生效（>0 且 ≠1）时下发，前端缺省按 1 处理。
func TestPlazaRateFactorPtr(t *testing.T) {
	require.Nil(t, plazaRateFactorPtr(0), "零值表示未配置")
	require.Nil(t, plazaRateFactorPtr(1), "系数 1 等价于未配置")
	require.Nil(t, plazaRateFactorPtr(-1))
	got := plazaRateFactorPtr(2)
	require.NotNil(t, got)
	require.Equal(t, 2.0, *got)
}

func TestToModelPlazaGroupDTO_RateFactorJSON(t *testing.T) {
	dto := toModelPlazaGroupDTO(&service.PlazaGroup{
		ID: 1,
		Models: []service.PlazaModel{
			{Name: "claude-opus-4-6", Platform: service.PlatformAnthropic, RateFactor: 2},
			{Name: "claude-sonnet-4", Platform: service.PlatformAnthropic},
		},
	}, nil)

	payload, err := json.Marshal(dto.Models)
	require.NoError(t, err)
	var models []map[string]any
	require.NoError(t, json.Unmarshal(payload, &models))
	require.Equal(t, 2.0, models[0]["rate_factor"])
	_, present := models[1]["rate_factor"]
	require.False(t, present, "未配置系数时 JSON 省略 rate_factor")
}

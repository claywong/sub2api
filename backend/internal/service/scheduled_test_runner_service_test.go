//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestScheduledTestRunner_TryRecoverAccount_ClearsHealthVerdictState(t *testing.T) {
	repo := &rateLimitClearRepoStub{
		getByIDAccount: &Account{
			ID:          42,
			Status:      StatusActive,
			Schedulable: true,
			Extra:       map[string]any{},
		},
	}
	rateLimitSvc := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	healthCache := NewAccountTestHealthCache(nil)
	healthCache.RecordRealCall(42, CallSample{Success: false, TTFTMs: 12000})
	healthCache.RecordRealCall(42, CallSample{Success: false, TTFTMs: 12000})
	healthCache.RecordRealCall(42, CallSample{Success: false, TTFTMs: 12000})
	healthCache.RecordRealCall(42, CallSample{Success: false, TTFTMs: 12000})
	healthCache.RecordRealCall(42, CallSample{Success: false, TTFTMs: 12000})

	_, verdict, _ := healthCache.SnapshotAndVerdict(42)
	require.NotEqual(t, HealthOK, verdict)

	runner := NewScheduledTestRunnerService(nil, nil, nil, rateLimitSvc, nil, healthCache)
	runner.tryRecoverAccount(context.Background(), 42, 1001)

	_, verdict, _ = healthCache.SnapshotAndVerdict(42)
	require.Equal(t, HealthOK, verdict)
	require.Nil(t, healthCache.Get(42))
}

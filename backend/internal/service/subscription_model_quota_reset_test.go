//go:build unit

package service

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

type modelQuotaResetCall struct {
	userID, groupID                       int64
	ruleKey                               string
	resetDaily, resetWeekly, resetMonthly bool
}

type modelQuotaResetterStub struct {
	resetCalls      []modelQuotaResetCall
	invalidateCalls int
	resetErr        error
}

func (m *modelQuotaResetterStub) ResetUsage(_ context.Context, userID, groupID int64, ruleKey string, resetDaily, resetWeekly, resetMonthly bool) error {
	m.resetCalls = append(m.resetCalls, modelQuotaResetCall{userID, groupID, ruleKey, resetDaily, resetWeekly, resetMonthly})
	return m.resetErr
}

func (m *modelQuotaResetterStub) InvalidateUsageCache(context.Context, int64, int64, string) error {
	m.invalidateCalls++
	return nil
}

func TestAdminResetQuota_AlsoResetsModelQuotaUsage(t *testing.T) {
	stub := &resetQuotaUserSubRepoStub{sub: &UserSubscription{ID: 1, UserID: 10, GroupID: 20}}
	svc := newResetQuotaSvc(stub)
	resetter := &modelQuotaResetterStub{}
	svc.modelQuotaResetter = resetter

	_, err := svc.AdminResetQuota(context.Background(), 1, true, false, true)

	require.NoError(t, err)
	require.Equal(t, []modelQuotaResetCall{{
		userID: 10, groupID: 20, ruleKey: "",
		resetDaily: true, resetWeekly: false, resetMonthly: true,
	}}, resetter.resetCalls, "应按订阅重置的窗口重置该分组下全部模型规则")
}

func TestAdminResetQuota_ModelQuotaResetErrorPropagates(t *testing.T) {
	stub := &resetQuotaUserSubRepoStub{sub: &UserSubscription{ID: 1, UserID: 10, GroupID: 20}}
	svc := newResetQuotaSvc(stub)
	svc.modelQuotaResetter = &modelQuotaResetterStub{resetErr: errors.New("db down")}

	_, err := svc.AdminResetQuota(context.Background(), 1, true, true, true)

	require.ErrorContains(t, err, "reset model quota usage")
}

func TestAdminResetQuota_WithoutModelQuotaResetter(t *testing.T) {
	stub := &resetQuotaUserSubRepoStub{sub: &UserSubscription{ID: 1, UserID: 10, GroupID: 20}}
	svc := newResetQuotaSvc(stub)

	_, err := svc.AdminResetQuota(context.Background(), 1, true, true, true)

	require.NoError(t, err)
}

func TestBulkResetQuota_InvalidatesModelQuotaCacheAfterCommit(t *testing.T) {
	stub := &resetQuotaUserSubRepoStub{sub: &UserSubscription{ID: 1, UserID: 10, GroupID: 20}}
	svc := newResetQuotaSvc(stub)
	resetter := &modelQuotaResetterStub{}
	svc.modelQuotaResetter = resetter

	result, err := svc.BulkSubscriptionAction(context.Background(), &BulkSubscriptionActionInput{
		SubscriptionIDs: []int64{1},
		Action:          "reset_quota",
		Daily:           true,
	})

	require.NoError(t, err)
	require.Len(t, result.Results, 1)
	require.Len(t, resetter.resetCalls, 1)
	require.Equal(t, 1, resetter.invalidateCalls, "提交后应再次失效按模型用量缓存")
}

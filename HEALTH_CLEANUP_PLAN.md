# 账号健康管理功能清理计划

## 目标
删除所有账号健康管理相关代码，恢复与 upstream 一致

## 已完成
- [x] 删除 8 个健康管理测试文件
- [x] 删除 config.go 中的 AccountHealthConfig 结构
- [x] 删除 config.go 中的 SchedulingHealthConfig 结构
- [x] 删除 GatewaySchedulingConfig 中的 AccountHealth 和 Health 字段

## 待处理文件清单

### 1. Service 层核心文件
- [ ] `backend/internal/service/gateway_service.go`
  - 删除 healthCache 字段
  - 删除 healthCache 相关的初始化和回调
  - 删除 onHealthVerdictChange 方法
  - 删除 healthVerdictConfig 方法

- [ ] `backend/internal/service/gateway_service_scheduling.go`
  - 删除健康状态检查逻辑
  - 恢复原始的调度逻辑

- [ ] `backend/internal/service/gateway_service_auto_recovery.go`
  - 删除健康状态相关的自动恢复逻辑

- [ ] `backend/internal/service/gateway_service_weighted_select.go`
  - 删除 HealthVerdict 过滤逻辑
  - 删除健康状态相关的注释

- [ ] `backend/internal/service/account_model_quality_cache.go`
  - 删除健康状态相关的质量评分逻辑

- [ ] `backend/internal/service/gateway_service_quality_bucket.go`
  - 删除健康状态相关的分桶逻辑

### 2. Handler 层
- [ ] `backend/internal/handler/admin/account_handler.go`
  - 删除健康状态相关的 API 字段
  - 删除健康状态暴露逻辑

- [ ] `backend/internal/handler/admin/scheduler_handler.go`
  - 删除健康质量相关的 API 端点

### 3. 测试文件
- [ ] `backend/internal/service/gateway_service_weighted_select_test.go`
  - 删除健康状态相关的测试用例

- [ ] `backend/internal/service/scheduled_test_runner_service_test.go`
  - 删除健康状态相关的测试用例

### 4. 清理策略
对于每个文件，需要：
1. 对比 upstream 版本，找出差异
2. 删除仅在我们 fork 中存在的健康管理代码
3. 保留 upstream 原有的逻辑
4. 确保编译通过
5. 确保测试通过

## 执行建议
由于改动范围大，建议：
1. 创建专门的清理分支 `cleanup/remove-health-management`
2. 分批次提交，每次处理 1-2 个文件
3. 每次提交后运行测试确保不破坏现有功能
4. 最后与 upstream 对比验证一致性

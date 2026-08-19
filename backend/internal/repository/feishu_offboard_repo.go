// 私有扩展（不属于 upstream sub2api）。
//
// 本文件：service.FeishuOffboardRepository 的 PostgreSQL 实现（裸 SQL）。
// 所含内容：feishu_offboard_runs 表的 Insert / List / GetByID。
// merge 策略：纯新增文件，与 upstream 无交集，merge 时保留即可。
//
// 为什么用裸 *sql.DB 而不是 ent：906 表是私有迁移，没有对应的 ent schema。
// 走裸 SQL 可以避免为一张私有表引入 ent codegen 改动，进一步减少与 upstream 的冲突面。
//
// @author wangzhong
package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// ErrFeishuOffboardRunNotFound 执行记录不存在。
//
// 刻意定义在 repository 包而不是 service 包：service 层的契约文件
// (feishu_offboard_types.go) 只放类型与签名，不引入错误常量，避免多人并行开发时
// 互相改同一个文件。它是 *infraerrors.ApplicationError，HTTP 层会自动渲染成 404，
// 调用方也可以用 errors.Is(err, sql.ErrNoRows) 判断（cause 已保留）。
var ErrFeishuOffboardRunNotFound = infraerrors.NotFound(
	"FEISHU_OFFBOARD_RUN_NOT_FOUND", "feishu offboard run not found",
)

const (
	// feishuOffboardRunDefaultPageSize 列表默认页大小。
	feishuOffboardRunDefaultPageSize = 20
	// feishuOffboardRunMaxPageSize 列表页大小上限，防止前端传超大 page_size 拖垮库。
	feishuOffboardRunMaxPageSize = 100
	// feishuOffboardTriggerSourceMaxLen 与 906 迁移里 VARCHAR(16) 保持一致。
	// 超长会被 Postgres 直接拒绝，这里先截断，避免因为一个无关字段丢掉整条执行记录。
	feishuOffboardTriggerSourceMaxLen = 16
)

// feishuOffboardRepository 飞书离职执行记录仓储（raw SQL，append-only）。
// 与审计日志同理：执行记录只允许追加，不提供 Update / Delete。
type feishuOffboardRepository struct {
	db *sql.DB
}

// NewFeishuOffboardRepository 创建飞书离职执行记录仓储。
func NewFeishuOffboardRepository(db *sql.DB) service.FeishuOffboardRepository {
	return &feishuOffboardRepository{db: db}
}

// ── Insert ────────────────────────────────────────────────────────────

// Insert 写入一次执行记录并回填自增 ID。
//
// created_at 交给数据库默认值（now()），不由应用侧传：这样即使调用方所在机器
// 时钟漂移，落库时间线仍然单调；run_at 则允许调用方指定（可能与写库时刻有偏差，
// 比如任务跑了 3 分钟才写结果），零值时回退到当前时间。
func (r *feishuOffboardRepository) Insert(ctx context.Context, run *service.FeishuOffboardRun) error {
	if r == nil || r.db == nil {
		return fmt.Errorf("nil feishu offboard repository")
	}
	if run == nil {
		return fmt.Errorf("nil feishu offboard run")
	}

	runAt := run.RunAt
	if runAt.IsZero() {
		runAt = time.Now().UTC()
	}

	details, err := marshalOffboardDecisions(run.Decisions)
	if err != nil {
		return err
	}

	query := `INSERT INTO feishu_offboard_runs (
  run_at, trigger_source, dry_run,
  checked_count, resigned_count, disabled_count, unverifiable_count, skipped_count,
  circuit_broken, duration_ms, error_message, details
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
RETURNING id, created_at`

	row := r.db.QueryRowContext(ctx, query,
		runAt.UTC(),
		truncateString(run.TriggerSource, feishuOffboardTriggerSourceMaxLen),
		run.DryRun,
		run.CheckedCount,
		run.ResignedCount,
		run.DisabledCount,
		run.UnverifiableCount,
		run.SkippedCount,
		run.CircuitBroken,
		run.DurationMs,
		nullStringOrNil(run.ErrorMessage),
		details,
	)
	if err := row.Scan(&run.ID, &run.CreatedAt); err != nil {
		return err
	}
	run.RunAt = runAt
	return nil
}

// marshalOffboardDecisions 把判定明细序列化成 details 列的值。
//
// 返回 nil（而不是 "[]"）表示写 SQL NULL：本次没有任何判定明细时，
// 让列保持 NULL 比存一个空数组更能表达"没有数据"，也省掉一次 JSONB 解析。
func marshalOffboardDecisions(decisions []service.OffboardDecision) (any, error) {
	if len(decisions) == 0 {
		return nil, nil
	}
	encoded, err := json.Marshal(decisions)
	if err != nil {
		// 明细序列化失败就整条失败，不静默降级成"执行成功但没有依据"。
		// 这张表存在的意义就是事后追溯"凭什么禁了这个人"，
		// 丢了 details 的记录等于没有记录，还会误导排查。
		return nil, fmt.Errorf("marshal offboard decisions: %w", err)
	}
	return string(encoded), nil
}

// nullStringOrNil 空字符串落库为 NULL。
// error_message 是 nullable，用 NULL 表示"本次没有错误"，
// 比空字符串更好查（WHERE error_message IS NOT NULL 就能筛出失败的执行）。
func nullStringOrNil(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}

// ── List ──────────────────────────────────────────────────────────────

// feishuOffboardRunListColumns 列表查询的列。
//
// 刻意不含 details：单次执行的 details 可能有几百条 decision（一次全量巡检
// 就是全部在册用户数量级），列表页一次拉 20 行会把几 MB JSON 拖进内存再序列化给前端，
// 而列表页压根不展示明细。完整 decisions 只在 GetByID 里返回。
const feishuOffboardRunListColumns = `
  id,
  run_at,
  COALESCE(trigger_source, ''),
  dry_run,
  checked_count,
  resigned_count,
  disabled_count,
  unverifiable_count,
  skipped_count,
  circuit_broken,
  duration_ms,
  COALESCE(error_message, ''),
  created_at`

// List 按 run_at 倒序分页查询执行历史（不含 details）。
func (r *feishuOffboardRepository) List(
	ctx context.Context,
	filter service.FeishuOffboardRunListFilter,
) (*service.FeishuOffboardRunList, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("nil feishu offboard repository")
	}

	page, pageSize := normalizeFeishuOffboardPaging(filter)

	var total int64
	if err := r.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM feishu_offboard_runs").Scan(&total); err != nil {
		return nil, err
	}

	result := &service.FeishuOffboardRunList{
		Items:    make([]service.FeishuOffboardRun, 0, pageSize),
		Total:    total,
		Page:     page,
		PageSize: pageSize,
	}
	if total == 0 {
		return result, nil
	}

	// 同一秒内可能有 cron 与手动触发两条记录，run_at 相同时按 id 兜底排序，
	// 否则翻页时行序不稳定，会出现重复或漏行。
	query := "SELECT" + feishuOffboardRunListColumns + `
FROM feishu_offboard_runs
ORDER BY run_at DESC, id DESC
LIMIT $1 OFFSET $2`

	rows, err := r.db.QueryContext(ctx, query, pageSize, (page-1)*pageSize)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		item, err := scanFeishuOffboardRunRow(rows.Scan)
		if err != nil {
			return nil, err
		}
		result.Items = append(result.Items, *item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// normalizeFeishuOffboardPaging 兜底分页参数，避免负 LIMIT / OFFSET 打到数据库。
func normalizeFeishuOffboardPaging(filter service.FeishuOffboardRunListFilter) (page, pageSize int) {
	page = filter.Page
	if page <= 0 {
		page = 1
	}
	pageSize = filter.PageSize
	if pageSize <= 0 {
		pageSize = feishuOffboardRunDefaultPageSize
	}
	if pageSize > feishuOffboardRunMaxPageSize {
		pageSize = feishuOffboardRunMaxPageSize
	}
	return page, pageSize
}

// scanFeishuOffboardRunRow 扫描不含 details 的一行。
// error_message 在 SQL 侧已 COALESCE 成空串，所以这里不需要 sql.NullString。
func scanFeishuOffboardRunRow(scan func(dest ...any) error) (*service.FeishuOffboardRun, error) {
	item := &service.FeishuOffboardRun{}
	if err := scan(
		&item.ID,
		&item.RunAt,
		&item.TriggerSource,
		&item.DryRun,
		&item.CheckedCount,
		&item.ResignedCount,
		&item.DisabledCount,
		&item.UnverifiableCount,
		&item.SkippedCount,
		&item.CircuitBroken,
		&item.DurationMs,
		&item.ErrorMessage,
		&item.CreatedAt,
	); err != nil {
		return nil, err
	}
	return item, nil
}

// ── GetByID ───────────────────────────────────────────────────────────

// GetByID 取单次执行详情，含完整 decisions。
func (r *feishuOffboardRepository) GetByID(ctx context.Context, id int64) (*service.FeishuOffboardRun, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("nil feishu offboard repository")
	}

	// details 用 ::text 取出再自行 unmarshal：驱动对 JSONB 只会给到 []byte，
	// 转成 text 后能统一按字符串判空（NULL / 'null' / '[]' 都视作没有明细）。
	query := "SELECT" + feishuOffboardRunListColumns + `,
  COALESCE(details::text, '')
FROM feishu_offboard_runs
WHERE id = $1`

	row := r.db.QueryRowContext(ctx, query, id)
	item := &service.FeishuOffboardRun{}
	var detailsRaw string
	if err := row.Scan(
		&item.ID,
		&item.RunAt,
		&item.TriggerSource,
		&item.DryRun,
		&item.CheckedCount,
		&item.ResignedCount,
		&item.DisabledCount,
		&item.UnverifiableCount,
		&item.SkippedCount,
		&item.CircuitBroken,
		&item.DurationMs,
		&item.ErrorMessage,
		&item.CreatedAt,
		&detailsRaw,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrFeishuOffboardRunNotFound.WithCause(err)
		}
		return nil, err
	}

	decisions, err := unmarshalOffboardDecisions(detailsRaw)
	if err != nil {
		return nil, err
	}
	item.Decisions = decisions
	return item, nil
}

// unmarshalOffboardDecisions 解析 details 列。
//
// details 允许为 NULL（历史记录、或本次没有任何判定），所以解析前先判空。
// 解析失败则整个请求失败：details 坏了说明写入侧或表结构出了问题，
// 静默返回一条"没有明细"的记录会让人误以为这次执行真的没判定任何人。
func unmarshalOffboardDecisions(raw string) ([]service.OffboardDecision, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "null" || raw == "[]" {
		return nil, nil
	}
	var decisions []service.OffboardDecision
	if err := json.Unmarshal([]byte(raw), &decisions); err != nil {
		return nil, fmt.Errorf("unmarshal offboard decisions: %w", err)
	}
	return decisions, nil
}

package repository

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"go.uber.org/zap"
)

const (
	requestLogQueueCap  = 8192
	requestLogBatchSize = 128
	requestLogBatchWin  = 50 * time.Millisecond
)

type requestLogRepository struct {
	db      *sql.DB
	ch      chan *service.RequestLog
	once    sync.Once
}

// NewRequestLogRepository 创建请求日志仓库，复用主库连接。
func NewRequestLogRepository(db *sql.DB) service.RequestLogRepository {
	return &requestLogRepository{db: db}
}

// CreateBestEffort 投入异步队列，队列满时静默丢弃，不阻塞调用方。
func (r *requestLogRepository) CreateBestEffort(ctx context.Context, log *service.RequestLog) {
	r.once.Do(r.startWorker)
	if log.CreatedAt.IsZero() {
		log.CreatedAt = time.Now()
	}
	select {
	case r.ch <- log:
	default:
		logger.L().Debug("request_log: queue full, dropping", zap.String("request_id", log.RequestID))
	}
}

func (r *requestLogRepository) startWorker() {
	r.ch = make(chan *service.RequestLog, requestLogQueueCap)
	go r.runBatchLoop()
}

func (r *requestLogRepository) runBatchLoop() {
	batch := make([]*service.RequestLog, 0, requestLogBatchSize)
	ticker := time.NewTicker(requestLogBatchWin)
	defer ticker.Stop()

	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := r.insertBatch(batch); err != nil {
			logger.L().Warn("request_log: batch insert failed", zap.Error(err), zap.Int("count", len(batch)))
		}
		batch = batch[:0]
	}

	for {
		select {
		case log, ok := <-r.ch:
			if !ok {
				flush()
				return
			}
			batch = append(batch, log)
			if len(batch) >= requestLogBatchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

// GetByRequestIDs 按 request_id 批量查询，返回 map[requestID]*RequestLog。
func (r *requestLogRepository) GetByRequestIDs(ctx context.Context, requestIDs []string) (map[string]*service.RequestLog, error) {
	if len(requestIDs) == 0 {
		return nil, nil
	}

	placeholders := make([]string, len(requestIDs))
	args := make([]any, len(requestIDs))
	for i, id := range requestIDs {
		placeholders[i] = fmt.Sprintf("$%d", i+1)
		args[i] = id
	}
	query := "SELECT request_id, session_id, request_body, response_body FROM request_logs WHERE request_id IN (" +
		strings.Join(placeholders, ",") + ")"

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[string]*service.RequestLog, len(requestIDs))
	for rows.Next() {
		var log service.RequestLog
		var sessionID sql.NullString
		if err := rows.Scan(&log.RequestID, &sessionID, &log.RequestBody, &log.ResponseBody); err != nil {
			return nil, err
		}
		if sessionID.Valid {
			log.SessionID = sessionID.String
		}
		result[log.RequestID] = &log
	}
	return result, rows.Err()
}

func (r *requestLogRepository) insertBatch(logs []*service.RequestLog) error {
	if len(logs) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO request_logs (request_id, session_id, user_id, request_body, response_body, created_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (request_id) DO NOTHING
	`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, log := range logs {
		sessionID := sql.NullString{String: log.SessionID, Valid: log.SessionID != ""}
		if _, err := stmt.ExecContext(ctx,
			log.RequestID, sessionID, log.UserID,
			log.RequestBody, log.ResponseBody, log.CreatedAt,
		); err != nil {
			logger.L().Warn("request_log: row insert failed",
				zap.String("request_id", log.RequestID), zap.Error(err))
		}
	}
	return tx.Commit()
}

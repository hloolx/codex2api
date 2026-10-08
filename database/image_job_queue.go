package database

import (
	"context"
	"fmt"
	"time"
)

// InitImageJobQueue is an additive, repeatable migration. Old readers may
// continue using the existing job projections; no image data is rewritten.
func (db *DB) InitImageJobQueue(ctx context.Context) error {
	for _, col := range []struct{ name, def string }{
		{"queue_owner", "TEXT NOT NULL DEFAULT ''"},
		{"queue_lease_until", "BIGINT NOT NULL DEFAULT 0"},
	} {
		if db.isSQLite() {
			if err := db.ensureSQLiteColumn(ctx, "image_generation_jobs", col.name, col.def); err != nil {
				return err
			}
		} else if _, err := db.conn.ExecContext(ctx, fmt.Sprintf("ALTER TABLE image_generation_jobs ADD COLUMN IF NOT EXISTS %s %s", col.name, col.def)); err != nil {
			return err
		}
	}
	_, err := db.conn.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_image_jobs_queue_status_id ON image_generation_jobs(status,id)`)
	return err
}

// Candidate IDs only: never load every queued job's inputs into memory.
func (db *DB) QueuedImageJobIDs(ctx context.Context, limit int) ([]int64, error) {
	rows, err := db.conn.QueryContext(ctx, `SELECT id FROM image_generation_jobs WHERE status=$1 ORDER BY id LIMIT $2`, ImageJobQueued, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (db *DB) ClaimImageJob(ctx context.Context, id int64, owner string, until time.Time) (bool, error) {
	r, err := db.conn.ExecContext(ctx, `UPDATE image_generation_jobs SET status=$1, started_at=CURRENT_TIMESTAMP, queue_owner=$2, queue_lease_until=$3 WHERE id=$4 AND status=$5`, ImageJobRunning, owner, until.Unix(), id, ImageJobQueued)
	if err != nil {
		return false, err
	}
	n, err := r.RowsAffected()
	return n == 1, err
}

func (db *DB) RenewImageJobLease(ctx context.Context, id int64, owner string, until time.Time) (bool, error) {
	r, err := db.conn.ExecContext(ctx, `UPDATE image_generation_jobs SET queue_lease_until=$1 WHERE id=$2 AND queue_owner=$3 AND status=$4`, until.Unix(), id, owner, ImageJobRunning)
	if err != nil {
		return false, err
	}
	n, err := r.RowsAffected()
	return n == 1, err
}

// Never resubmit an interrupted upstream request: its billing/result may be
// unknown. Queued work is deliberately preserved across restarts.
func (db *DB) ExpireImageJobLeases(ctx context.Context, now time.Time) error {
	_, err := db.conn.ExecContext(ctx, `UPDATE image_generation_jobs SET status=$1,error_message=$2,completed_at=CURRENT_TIMESTAMP WHERE status=$3 AND queue_lease_until<$4`, ImageJobFailed, "图片任务执行中断，结果未知；未自动重新提交上游，请核对后手动重试", ImageJobRunning, now.Unix())
	return err
}

// Fences late worker writes after lease expiry/recovery.
func (db *DB) FinishLeasedImageJob(ctx context.Context, id int64, owner, status, message string, duration int) error {
	runes := []rune(message)
	if len(runes) > 2000 {
		message = string(runes[:2000])
	}
	_, err := db.conn.ExecContext(ctx, `UPDATE image_generation_jobs SET status=$1,error_message=$2,duration_ms=$3,completed_at=CURRENT_TIMESTAMP WHERE id=$4 AND queue_owner=$5 AND status=$6 AND queue_lease_until >= $7`, status, message, duration, id, owner, ImageJobRunning, time.Now().Unix())
	return err
}

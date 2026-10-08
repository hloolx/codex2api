package database

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

func (db *DB) InitImageMaintenance(ctx context.Context) error {
	for _, q := range []string{
		`CREATE INDEX IF NOT EXISTS idx_image_jobs_key_created_id ON image_generation_jobs(api_key_id,created_at,id)`,
		`CREATE INDEX IF NOT EXISTS idx_image_assets_created_id ON image_assets(created_at,id)`,
	} {
		if _, err := db.conn.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return nil
}

const compactJobColumns = `id,status,prompt,CASE WHEN length(params_json)<=65536 THEN params_json ELSE '{}' END,api_key_id,api_key_name,api_key_masked,error_message,duration_ms,created_at,started_at,completed_at`

func compactImageJob(job *ImageGenerationJob) {
	var params map[string]json.RawMessage
	_ = json.Unmarshal([]byte(job.ParamsJSON), &params)
	safe := map[string]json.RawMessage{}
	for _, key := range []string{"model", "size", "quality", "output_format", "background", "style", "upscale", "strict_size", "upscale_fit", "n", "api_key_id", "template_id"} {
		if value, ok := params[key]; ok && len(value) <= 8192 {
			safe[key] = value
		}
	}
	b, _ := json.Marshal(safe)
	job.ParamsJSON = string(b)
}

func (db *DB) GetImageJobSummary(ctx context.Context, id int64) (*ImageGenerationJob, error) {
	job, err := scanImageGenerationJob(db.conn.QueryRowContext(ctx, `SELECT `+compactJobColumns+` FROM image_generation_jobs WHERE id=$1`, id))
	if err != nil {
		return nil, err
	}
	compactImageJob(job)
	job.Assets, err = db.ListImageAssetsByJobID(ctx, id)
	return job, err
}

func (db *DB) ListImageJobSummaries(ctx context.Context, page, pageSize int, keyID int64) (*ImageJobPage, error) {
	page, pageSize = normalizePage(page, pageSize)
	where := ""
	args := []any{}
	if keyID > 0 {
		where = " WHERE api_key_id=$1"
		args = append(args, keyID)
	}
	var total int64
	if err := db.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM image_generation_jobs`+where, args...).Scan(&total); err != nil {
		return nil, err
	}
	q := `SELECT ` + compactJobColumns + ` FROM image_generation_jobs` + where + fmt.Sprintf(` ORDER BY created_at DESC,id DESC LIMIT $%d OFFSET $%d`, len(args)+1, len(args)+2)
	args = append(args, pageSize, (page-1)*pageSize)
	rows, err := db.conn.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	jobs := []ImageGenerationJob{}
	for rows.Next() {
		job, e := scanImageGenerationJob(rows)
		if e != nil {
			rows.Close()
			return nil, e
		}
		compactImageJob(job)
		jobs = append(jobs, *job)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if len(jobs) > 0 {
		ids := make([]any, len(jobs))
		binds := make([]string, len(jobs))
		index := map[int64]int{}
		for i, j := range jobs {
			ids[i] = j.ID
			binds[i] = fmt.Sprintf("$%d", i+1)
			index[j.ID] = i
		}
		rows, err = db.conn.QueryContext(ctx, imageAssetSelectSQL("")+` WHERE job_id IN (`+strings.Join(binds, ",")+`) ORDER BY id`, ids...)
		if err != nil {
			return nil, err
		}
		assets, e := scanImageAssets(rows)
		rows.Close()
		if e != nil {
			return nil, e
		}
		for _, a := range assets {
			i := index[a.JobID]
			jobs[i].Assets = append(jobs[i].Assets, a)
		}
	}
	return &ImageJobPage{Jobs: jobs, Total: total}, nil
}

func (db *DB) ExpiredImageAssets(ctx context.Context, cutoff time.Time, after int64, limit int) ([]ImageAsset, error) {
	rows, err := db.conn.QueryContext(ctx, imageAssetSelectSQL("a")+` JOIN image_generation_jobs j ON j.id=a.job_id WHERE a.expires_at=0 AND a.created_at<$1 AND a.id>$2 AND j.status IN ('succeeded','failed') ORDER BY a.id LIMIT $3`, db.imageRetentionCutoff(cutoff), after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanImageAssets(rows)
}

func (db *DB) DeleteExpiredImageJobs(ctx context.Context, cutoff time.Time, limit int) (int64, error) {
	r, err := db.conn.ExecContext(ctx, `DELETE FROM image_generation_jobs WHERE id IN (SELECT j.id FROM image_generation_jobs j WHERE j.status IN ('succeeded','failed') AND COALESCE(j.completed_at,j.created_at)<$1 AND NOT EXISTS (SELECT 1 FROM image_assets a WHERE a.job_id=j.id) ORDER BY j.id LIMIT $2)`, db.imageRetentionCutoff(cutoff), limit)
	if err != nil {
		return 0, err
	}
	return r.RowsAffected()
}

// Read only active jobs with spooled inputs, one row at a time. Never load
// historical Base64 payloads to decide whether a temporary file is in use.
func (db *DB) ActiveImageQueueInputs(ctx context.Context) (map[string]bool, error) {
	rows, err := db.conn.QueryContext(ctx, `SELECT params_json FROM image_generation_jobs WHERE status IN ('queued','running') AND params_json LIKE '%queue-input:%'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	refs := map[string]bool{}
	for rows.Next() {
		var raw string
		if err = rows.Scan(&raw); err != nil {
			return nil, err
		}
		var p struct {
			InputImages []string `json:"input_images"`
		}
		if err = json.Unmarshal([]byte(raw), &p); err != nil {
			return nil, err
		}
		for _, v := range p.InputImages {
			refs[v] = true
		}
	}
	return refs, rows.Err()
}

// Strip expired legacy input bytes without deleting the text/history record.
// A single row per iteration keeps multi-megabyte legacy payloads bounded.
func (db *DB) PruneExpiredImageInputs(ctx context.Context, cutoff time.Time, after int64) (int64, error) {
	var id int64
	var raw string
	err := db.conn.QueryRowContext(ctx, `SELECT id,params_json FROM image_generation_jobs WHERE id>$1 AND status IN ('succeeded','failed') AND COALESCE(completed_at,created_at)<$2 AND (params_json LIKE '%;base64,%' OR params_json LIKE '%queue-input:%') ORDER BY id LIMIT 1`, after, db.imageRetentionCutoff(cutoff)).Scan(&id, &raw)
	if err != nil {
		return 0, err
	}
	var p map[string]json.RawMessage
	if err = json.Unmarshal([]byte(raw), &p); err != nil {
		return id, nil
	}
	delete(p, "input_images")
	b, err := json.Marshal(p)
	if err != nil {
		return id, err
	}
	_, err = db.conn.ExecContext(ctx, `UPDATE image_generation_jobs SET params_json=$1 WHERE id=$2 AND status IN ('succeeded','failed')`, string(b), id)
	return id, err
}

func (db *DB) imageRetentionCutoff(t time.Time) any {
	if db.isSQLite() {
		return t.UTC().Format("2006-01-02 15:04:05")
	}
	return t.UTC()
}

package database

import (
	"context"
	"fmt"
	"strings"
)

// GetImageGenerationJobResult reads an owner's job without loading prompts,
// expanded input images, or API key display metadata. The full job API is unchanged.
func (db *DB) GetImageGenerationJobResult(ctx context.Context, id, apiKeyID int64) (*ImageGenerationJob, error) {
	job, err := scanImageGenerationJob(db.conn.QueryRowContext(ctx, `
		SELECT id, status, '', '', api_key_id, '', '', error_message,
			duration_ms, created_at, started_at, completed_at
		FROM image_generation_jobs WHERE id=$1 AND api_key_id=$2
	`, id, apiKeyID))
	if err != nil {
		return nil, err
	}
	job.Assets, err = db.ListImageAssetsByJobID(ctx, id)
	if err != nil {
		return nil, err
	}
	return job, nil
}

// ListImageGenerationJobResults reads multiple owner's jobs without loading
// prompts, expanded input images, or API key display metadata. Missing IDs are
// intentionally omitted so callers can safely poll a mixed batch.
func (db *DB) ListImageGenerationJobResults(ctx context.Context, ids []int64, apiKeyID int64) ([]ImageGenerationJob, error) {
	if len(ids) == 0 {
		return []ImageGenerationJob{}, nil
	}
	placeholders := make([]string, 0, len(ids))
	args := make([]any, 0, len(ids)+1)
	for i, id := range ids {
		placeholders = append(placeholders, fmt.Sprintf("$%d", i+1))
		args = append(args, id)
	}
	args = append(args, apiKeyID)
	keyPlaceholder := fmt.Sprintf("$%d", len(args))
	rows, err := db.conn.QueryContext(ctx, `
		SELECT id, status, '', '', api_key_id, '', '', error_message,
			duration_ms, created_at, started_at, completed_at
		FROM image_generation_jobs
		WHERE id IN (`+strings.Join(placeholders, ",")+`) AND api_key_id=`+keyPlaceholder+`
		ORDER BY id ASC
	`, args...)
	if err != nil {
		return nil, err
	}
	jobs := make([]ImageGenerationJob, 0, len(ids))
	for rows.Next() {
		job, scanErr := scanImageGenerationJob(rows)
		if scanErr != nil {
			rows.Close()
			return nil, scanErr
		}
		jobs = append(jobs, *job)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	// Release the first query before loading assets (SQLite may have one connection).
	rows.Close()
	if len(jobs) == 0 {
		return jobs, nil
	}
	jobIndex := make(map[int64]int, len(jobs))
	for i := range jobs {
		jobIndex[jobs[i].ID] = i
	}
	assetPlaceholders := make([]string, 0, len(jobs))
	assetArgs := make([]any, 0, len(jobs))
	for i, job := range jobs {
		assetPlaceholders = append(assetPlaceholders, fmt.Sprintf("$%d", i+1))
		assetArgs = append(assetArgs, job.ID)
	}
	assetRows, err := db.conn.QueryContext(ctx, imageAssetSelectSQL("")+`
		WHERE job_id IN (`+strings.Join(assetPlaceholders, ",")+`) ORDER BY id ASC
	`, assetArgs...)
	if err != nil {
		return nil, err
	}
	assets, err := scanImageAssets(assetRows)
	assetRows.Close()
	if err != nil {
		return nil, err
	}
	for _, asset := range assets {
		if index, ok := jobIndex[asset.JobID]; ok {
			jobs[index].Assets = append(jobs[index].Assets, asset)
		}
	}
	return jobs, nil
}

package database

import (
	"context"
	"fmt"
	"time"
)

// ensureImageAssetRetentionSchema adds optional per-image expiry without
// changing the retention of existing rows. Unix seconds avoid DB timezone drift.
func (db *DB) ensureImageAssetRetentionSchema(ctx context.Context) error {
	for _, column := range []struct{ name, definition string }{
		{"expires_at", "BIGINT NOT NULL DEFAULT 0"},
		{"delete_after_read", "BOOLEAN NOT NULL DEFAULT FALSE"},
	} {
		if db.isSQLite() {
			if err := db.ensureSQLiteColumn(ctx, "image_assets", column.name, column.definition); err != nil {
				return err
			}
		} else if _, err := db.conn.ExecContext(ctx, fmt.Sprintf("ALTER TABLE image_assets ADD COLUMN IF NOT EXISTS %s %s", column.name, column.definition)); err != nil {
			return err
		}
	}
	_, err := db.conn.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_image_assets_expires_id ON image_assets(expires_at,id) WHERE expires_at > 0`)
	return err
}

// DueImageAssets selects only caller-managed expirations, in bounded batches.
func (db *DB) DueImageAssets(ctx context.Context, now time.Time, after int64, limit int) ([]ImageAsset, error) {
	rows, err := db.conn.QueryContext(ctx, imageAssetSelectSQL("")+` WHERE expires_at > 0 AND expires_at <= $1 AND id > $2 ORDER BY id LIMIT $3`, now.Unix(), after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanImageAssets(rows)
}

// ExpireImageAsset revokes further reads before deleting storage. If deletion
// fails, the expiry sweeper can retry using the retained asset row.
func (db *DB) ExpireImageAsset(ctx context.Context, id int64, now time.Time) error {
	_, err := db.conn.ExecContext(ctx, `UPDATE image_assets SET expires_at=$1 WHERE id=$2`, now.Unix(), id)
	return err
}

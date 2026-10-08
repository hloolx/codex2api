package database

import (
	"context"
	"database/sql"
	"errors"
)

const codexClientVersionCacheSchema = `CREATE TABLE IF NOT EXISTS codex_client_version_cache (
	client_kind TEXT NOT NULL,
	target_platform TEXT NOT NULL,
	payload TEXT NOT NULL DEFAULT '{}',
	PRIMARY KEY (client_kind, target_platform)
)`

type CodexClientVersionCacheKey struct {
	ClientKind     string
	TargetPlatform string
}

type CodexClientVersionCacheRow struct {
	CodexClientVersionCacheKey
	Payload string
}

type codexClientVersionMutation struct {
	key    CodexClientVersionCacheKey
	update func(string) (string, error)
}

func (db *DB) GetCodexClientVersionCache(ctx context.Context) ([]CodexClientVersionCacheRow, error) {
	if db == nil || db.conn == nil {
		return nil, errors.New("database unavailable")
	}
	rows, err := db.conn.QueryContext(ctx, `SELECT client_kind, target_platform, payload FROM codex_client_version_cache ORDER BY client_kind, target_platform`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []CodexClientVersionCacheRow
	for rows.Next() {
		var row CodexClientVersionCacheRow
		if err := rows.Scan(&row.ClientKind, &row.TargetPlatform, &row.Payload); err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	return result, rows.Err()
}

// MutateCodexClientVersionCache 串行合并单个目标的缓存，不回写系统设置。
func (db *DB) MutateCodexClientVersionCache(ctx context.Context, key CodexClientVersionCacheKey, update func(string) (string, error)) (string, error) {
	if db == nil || db.conn == nil {
		return "", errors.New("database unavailable")
	}
	var saved string
	err := db.withSQLiteWriteLock(ctx, func() error {
		tx, err := db.conn.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		saved, err = db.mutateCodexClientVersionCacheTx(ctx, tx, codexClientVersionMutation{key: key, update: update})
		if err != nil {
			return err
		}
		return tx.Commit()
	})
	return saved, err
}

func (db *DB) mutateCodexClientVersionCacheTx(ctx context.Context, tx *sql.Tx, mutation codexClientVersionMutation) (string, error) {
	key := mutation.key
	_, err := tx.ExecContext(ctx, `INSERT INTO codex_client_version_cache (client_kind, target_platform) VALUES ($1, $2) ON CONFLICT DO NOTHING`, key.ClientKind, key.TargetPlatform)
	if err != nil {
		return "", err
	}
	query := `SELECT payload FROM codex_client_version_cache WHERE client_kind = $1 AND target_platform = $2`
	if !db.isSQLite() {
		query += ` FOR UPDATE`
	}
	var raw string
	if err := tx.QueryRowContext(ctx, query, key.ClientKind, key.TargetPlatform).Scan(&raw); err != nil {
		return "", err
	}
	saved, err := mutation.update(raw)
	if err != nil {
		return "", err
	}
	_, err = tx.ExecContext(ctx, `UPDATE codex_client_version_cache SET payload = $1 WHERE client_kind = $2 AND target_platform = $3`, saved, key.ClientKind, key.TargetPlatform)
	return saved, err
}

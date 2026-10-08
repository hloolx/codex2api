package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// DaybreakSnapshot 独立保存完整权限目录，成功刷新时替换而非合并。
type DaybreakSnapshot struct {
	Identity   string              `json:"identity"`
	ObservedAt int64               `json:"observed_at"`
	CheckedAt  int64               `json:"checked_at"`
	Models     map[string][]string `json:"models"`
}

const daybreakSchema = `CREATE TABLE IF NOT EXISTS daybreak_snapshots (
 account_id BIGINT PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
 identity TEXT NOT NULL, observed_at BIGINT NOT NULL, checked_at BIGINT NOT NULL, models_json TEXT NOT NULL
);`

func (db *DB) SaveDaybreakSnapshot(ctx context.Context, id int64, snapshot DaybreakSnapshot) error {
	encoded, err := json.Marshal(snapshot.Models)
	if err != nil {
		return err
	}
	return db.withWriteTx(ctx, func(tx *sql.Tx) error {
		query := `SELECT credentials FROM accounts WHERE id=$1 AND deleted_at IS NULL`
		if !db.isSQLite() {
			query += ` FOR UPDATE`
		}
		var raw any
		if err := tx.QueryRowContext(ctx, query, id).Scan(&raw); err != nil {
			return err
		}
		if snapshot.Identity != daybreakRowIdentity(raw) {
			return fmt.Errorf("Daybreak account identity changed")
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO daybreak_snapshots(account_id,identity,observed_at,checked_at,models_json)
 SELECT id,$2,$3,$4,$5 FROM accounts WHERE id=$1 AND deleted_at IS NULL
 ON CONFLICT(account_id) DO UPDATE SET identity=excluded.identity,observed_at=excluded.observed_at,checked_at=excluded.checked_at,models_json=excluded.models_json
 WHERE daybreak_snapshots.observed_at < excluded.observed_at`, id, snapshot.Identity, snapshot.ObservedAt, snapshot.CheckedAt, string(encoded))
		return err
	})
}

func (db *DB) LoadDaybreakSnapshot(ctx context.Context, id int64) (DaybreakSnapshot, error) {
	var snapshot DaybreakSnapshot
	var raw string
	err := db.conn.QueryRowContext(ctx, `SELECT identity,observed_at,checked_at,models_json FROM daybreak_snapshots WHERE account_id=$1`, id).Scan(&snapshot.Identity, &snapshot.ObservedAt, &snapshot.CheckedAt, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return snapshot, nil
	}
	if err != nil {
		return snapshot, err
	}
	err = json.Unmarshal([]byte(raw), &snapshot.Models)
	return snapshot, err
}

// 身份编辑与权限清理共用事务，切回旧身份也必须重新检查目录。
func invalidateDaybreakIdentity(ctx context.Context, tx *sql.Tx, id int64) error {
	var raw any
	if err := tx.QueryRowContext(ctx, `SELECT credentials FROM accounts WHERE id=$1`, id).Scan(&raw); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM daybreak_snapshots WHERE account_id=$1 AND identity<>$2`, id, daybreakRowIdentity(raw))
	return err
}

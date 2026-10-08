package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"
)

// ModelTraceBankOverride is an operator-installed ModelTrace bank. It lives in
// its own table so the ~1 MB blob never rides along with system_settings reads.
type ModelTraceBankOverride struct {
	Revision    string
	BuiltAt     string
	BankJSON    []byte
	InstalledAt time.Time
}

var (
	modelTraceBankInitMu sync.Mutex
	modelTraceBankReady  = make(map[*DB]bool)
)

func (db *DB) ensureModelTraceBankSchema(ctx context.Context) error {
	if db == nil || db.conn == nil {
		return fmt.Errorf("数据库不可用")
	}
	modelTraceBankInitMu.Lock()
	defer modelTraceBankInitMu.Unlock()
	if modelTraceBankReady[db] {
		return nil
	}
	if _, err := db.conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS modeltrace_bank_override (
		singleton_id INTEGER PRIMARY KEY CHECK (singleton_id = 1),
		revision VARCHAR(64) NOT NULL,
		built_at VARCHAR(64) NOT NULL DEFAULT '',
		bank_json TEXT NOT NULL,
		installed_at TIMESTAMP NOT NULL
	)`); err != nil {
		return err
	}
	modelTraceBankReady[db] = true
	return nil
}

// GetModelTraceBankRevision returns the installed override revision without
// loading the bank body; empty means no override is installed.
func (db *DB) GetModelTraceBankRevision(ctx context.Context) (string, error) {
	if err := db.ensureModelTraceBankSchema(ctx); err != nil {
		return "", err
	}
	var revision string
	err := db.conn.QueryRowContext(ctx, `SELECT revision FROM modeltrace_bank_override WHERE singleton_id = 1`).Scan(&revision)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return revision, err
}

// GetModelTraceBankOverride returns the installed override, or nil when none.
func (db *DB) GetModelTraceBankOverride(ctx context.Context) (*ModelTraceBankOverride, error) {
	if err := db.ensureModelTraceBankSchema(ctx); err != nil {
		return nil, err
	}
	var override ModelTraceBankOverride
	var body string
	err := db.conn.QueryRowContext(ctx, `SELECT revision, built_at, bank_json, installed_at
		FROM modeltrace_bank_override WHERE singleton_id = 1`).Scan(
		&override.Revision, &override.BuiltAt, &body, &override.InstalledAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	override.BankJSON = []byte(body)
	return &override, nil
}

// ErrModelTraceBankConflict means the stored override changed after the
// caller read it (a concurrent update or reset); the caller should re-read.
var ErrModelTraceBankConflict = errors.New("ModelTrace bank override changed concurrently")

// SaveModelTraceBankOverride installs override only if the stored revision is
// still expectedRevision ("" meaning no override), so overlapping updates and
// resets cannot be silently overwritten by a stale writer.
func (db *DB) SaveModelTraceBankOverride(ctx context.Context, override ModelTraceBankOverride, expectedRevision string) error {
	if err := db.ensureModelTraceBankSchema(ctx); err != nil {
		return err
	}
	var result sql.Result
	var err error
	if expectedRevision == "" {
		result, err = db.conn.ExecContext(ctx, `INSERT INTO modeltrace_bank_override (singleton_id, revision, built_at, bank_json, installed_at)
			VALUES (1, $1, $2, $3, $4)
			ON CONFLICT (singleton_id) DO NOTHING`,
			override.Revision, override.BuiltAt, string(override.BankJSON), override.InstalledAt.UTC())
	} else {
		result, err = db.conn.ExecContext(ctx, `UPDATE modeltrace_bank_override
			SET revision = $1, built_at = $2, bank_json = $3, installed_at = $4
			WHERE singleton_id = 1 AND revision = $5`,
			override.Revision, override.BuiltAt, string(override.BankJSON), override.InstalledAt.UTC(), expectedRevision)
	}
	if err != nil {
		return err
	}
	if affected, err := result.RowsAffected(); err != nil {
		return err
	} else if affected == 0 {
		return ErrModelTraceBankConflict
	}
	return nil
}

func (db *DB) DeleteModelTraceBankOverride(ctx context.Context) error {
	if err := db.ensureModelTraceBankSchema(ctx); err != nil {
		return err
	}
	_, err := db.conn.ExecContext(ctx, `DELETE FROM modeltrace_bank_override WHERE singleton_id = 1`)
	return err
}

package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
)

const ModelQualityIntervalSeconds int64 = 600
const ModelQualityLeaseSeconds int64 = 120

var ErrModelQualityConflict = errors.New("quality configuration changed; reload and retry")

type ModelQualityConfig struct {
	Enabled  bool     `json:"enabled"`
	Models   []string `json:"models"`
	Revision int64    `json:"revision"`
}

type ModelQualityState struct {
	AccountID   int64  `json:"account_id"`
	Model       string `json:"model"`
	Generation  int64  `json:"-"`
	Status      string `json:"status"`
	LastOutcome string `json:"last_outcome"`
	Reason      string `json:"reason"`
	CheckedAt   int64  `json:"checked_at"`
	NextCheckAt int64  `json:"next_check_at"`
	Running     bool   `json:"running"`
}

func (db *DB) ensureModelQualitySchema(ctx context.Context) error {
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS model_quality_config (id INTEGER PRIMARY KEY, enabled BOOLEAN NOT NULL DEFAULT FALSE, models TEXT NOT NULL DEFAULT '[]', revision BIGINT NOT NULL DEFAULT 1)`,
		`INSERT INTO model_quality_config(id) VALUES(1) ON CONFLICT(id) DO NOTHING`,
		`CREATE TABLE IF NOT EXISTS model_quality_states (account_id BIGINT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE, model TEXT NOT NULL, generation BIGINT NOT NULL DEFAULT 0, status TEXT NOT NULL DEFAULT 'pending', last_outcome TEXT NOT NULL DEFAULT '', reason TEXT NOT NULL DEFAULT '', checked_at BIGINT NOT NULL DEFAULT 0, next_check_at BIGINT NOT NULL DEFAULT 0, PRIMARY KEY(account_id,model))`,
		`CREATE TABLE IF NOT EXISTS model_quality_leases (account_id BIGINT PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE, owner TEXT NOT NULL DEFAULT '', model TEXT NOT NULL DEFAULT '', revision BIGINT NOT NULL DEFAULT 0, lease_until BIGINT NOT NULL DEFAULT 0)`,
	} {
		if _, err := db.conn.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("initialize model quality: %w", err)
		}
	}
	return nil
}

// Read the configuration and verdicts from one database snapshot.
func (db *DB) ModelQualitySnapshot(ctx context.Context, now int64) (ModelQualityConfig, []ModelQualityState, error) {
	var cfg ModelQualityConfig
	tx, err := db.conn.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable, ReadOnly: true})
	if err != nil {
		return cfg, nil, err
	}
	defer tx.Rollback()
	var models string
	if err = tx.QueryRowContext(ctx, `SELECT enabled,models,revision FROM model_quality_config WHERE id=1`).Scan(&cfg.Enabled, &models, &cfg.Revision); err != nil {
		return cfg, nil, err
	}
	if err = json.Unmarshal([]byte(models), &cfg.Models); err != nil {
		return cfg, nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT s.account_id,s.model,s.generation,s.status,s.last_outcome,s.reason,s.checked_at,s.next_check_at,COALESCE(l.lease_until > $1 AND l.model=s.model AND l.revision=$2,FALSE) FROM model_quality_states s LEFT JOIN model_quality_leases l ON l.account_id=s.account_id`, now, cfg.Revision)
	if err != nil {
		return cfg, nil, err
	}
	states := make([]ModelQualityState, 0)
	for rows.Next() {
		var s ModelQualityState
		if err = rows.Scan(&s.AccountID, &s.Model, &s.Generation, &s.Status, &s.LastOutcome, &s.Reason, &s.CheckedAt, &s.NextCheckAt, &s.Running); err != nil {
			rows.Close()
			return cfg, nil, err
		}
		states = append(states, s)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return cfg, nil, err
	}
	return cfg, states, tx.Commit()
}

// A revision fences in-flight probes and concurrent administrator edits.
func (db *DB) SaveModelQualityConfig(ctx context.Context, cfg ModelQualityConfig) error {
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	models, err := json.Marshal(cfg.Models)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE model_quality_config SET enabled=$1,models=$2,revision=revision+1 WHERE id=1 AND revision=$3`, cfg.Enabled, string(models), cfg.Revision)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrModelQualityConflict
	}
	// Keep verdicts for retained models, but never resurrect deselected/disabled gates.
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT model FROM model_quality_states`)
	if err != nil {
		return err
	}
	var removed []string
	for rows.Next() {
		var model string
		if err = rows.Scan(&model); err != nil {
			rows.Close()
			return err
		}
		if !cfg.Enabled || !slices.Contains(cfg.Models, model) {
			removed = append(removed, model)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, model := range removed {
		if _, err = tx.ExecContext(ctx, `DELETE FROM model_quality_states WHERE model=$1`, model); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (db *DB) EnsureModelQualityState(ctx context.Context, id, generation int64, model string) error {
	_, err := db.conn.ExecContext(ctx, `INSERT INTO model_quality_states(account_id,model,generation) VALUES($1,$2,$3) ON CONFLICT(account_id,model) DO UPDATE SET generation=$3,status='pending',last_outcome='',reason='',checked_at=0,next_check_at=0 WHERE model_quality_states.generation<>$3`, id, model, generation)
	return err
}

// One renewable lease per account serializes its models across replicas.
func (db *DB) ClaimModelQuality(ctx context.Context, id int64, model, owner string, revision, now int64) (bool, error) {
	if _, err := db.conn.ExecContext(ctx, `INSERT INTO model_quality_leases(account_id) VALUES($1) ON CONFLICT(account_id) DO NOTHING`, id); err != nil {
		return false, err
	}
	result, err := db.conn.ExecContext(ctx, `UPDATE model_quality_leases SET owner=$1,model=$2,revision=$3,lease_until=$4 WHERE account_id=$5 AND lease_until<=$6 AND EXISTS(SELECT 1 FROM model_quality_config WHERE id=1 AND enabled=TRUE AND revision=$3) AND EXISTS(SELECT 1 FROM model_quality_states WHERE account_id=$5 AND model=$2 AND next_check_at<=$6)`, owner, model, revision, now+ModelQualityLeaseSeconds, id, now)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

func (db *DB) RenewModelQuality(ctx context.Context, id int64, owner string, revision, now int64) (bool, error) {
	result, err := db.conn.ExecContext(ctx, `UPDATE model_quality_leases SET lease_until=$1 WHERE account_id=$2 AND owner=$3 AND revision=$4 AND lease_until>$5 AND EXISTS(SELECT 1 FROM model_quality_config WHERE id=1 AND enabled=TRUE AND revision=$4)`, now+ModelQualityLeaseSeconds, id, owner, revision, now)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

func (db *DB) ReleaseModelQuality(ctx context.Context, id int64, owner string) error {
	_, err := db.conn.ExecContext(ctx, `UPDATE model_quality_leases SET lease_until=0,owner='' WHERE account_id=$1 AND owner=$2`, id, owner)
	return err
}

// Inconclusive outcomes leave the last conclusive verdict unchanged.
func (db *DB) FinishModelQuality(ctx context.Context, s ModelQualityState, owner string, revision, now int64) (bool, error) {
	result, err := db.conn.ExecContext(ctx, `UPDATE model_quality_states SET status=CASE WHEN $1 IN ('pass','fail') THEN $1 ELSE status END,last_outcome=$1,reason=$2,checked_at=$3,next_check_at=$4 WHERE account_id=$5 AND model=$6 AND generation=$7 AND EXISTS(SELECT 1 FROM model_quality_config WHERE id=1 AND enabled=TRUE AND revision=$8) AND EXISTS(SELECT 1 FROM model_quality_leases WHERE account_id=$5 AND owner=$9 AND lease_until>$3 AND revision=$8 AND model=$6)`, s.LastOutcome, s.Reason, now, now+ModelQualityIntervalSeconds, s.AccountID, s.Model, s.Generation, revision, owner)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

func (db *DB) RetestModelQuality(ctx context.Context, id int64, model string) error {
	_, err := db.conn.ExecContext(ctx, `UPDATE model_quality_states SET next_check_at=0 WHERE account_id=$1 AND model=$2`, id, model)
	return err
}

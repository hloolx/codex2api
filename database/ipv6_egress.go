package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
)

var ErrIPv6PoolExhausted = errors.New("no available dedicated IPv6 address")
var ErrIPv6ConfigConflict = errors.New("IPv6 configuration changed; reload and retry")

type IPv6EgressConfig struct {
	Enabled         bool     `json:"enabled"`
	SourceIPs       []string `json:"source_ips"`
	CooldownSeconds int      `json:"cooldown_seconds"`
	MaxAttempts     int      `json:"max_attempts"`
	Retry5xx        bool     `json:"retry_5xx"`
	Revision        int64    `json:"revision"`
}

type IPv6Binding struct {
	AccountID int64  `json:"account_id"`
	IP        string `json:"ip"`
	ChangedAt int64  `json:"changed_at"`
	Reason    string `json:"reason"`
	Rotations int64  `json:"rotations"`
}

type IPv6Cooldown struct {
	IP    string `json:"ip"`
	Until int64  `json:"until"`
}

func (db *DB) ensureIPv6EgressSchema(ctx context.Context) error {
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS ipv6_egress_config(id INTEGER PRIMARY KEY, enabled BOOLEAN NOT NULL DEFAULT FALSE, source_ips TEXT NOT NULL DEFAULT '[]', cooldown_seconds INTEGER NOT NULL DEFAULT 600, max_attempts INTEGER NOT NULL DEFAULT 3, retry_5xx BOOLEAN NOT NULL DEFAULT TRUE, revision BIGINT NOT NULL DEFAULT 1)`,
		`INSERT INTO ipv6_egress_config(id) VALUES(1) ON CONFLICT(id) DO NOTHING`,
		`CREATE TABLE IF NOT EXISTS ipv6_egress_bindings(account_id BIGINT PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE, ip TEXT NOT NULL UNIQUE, changed_at BIGINT NOT NULL DEFAULT 0, reason TEXT NOT NULL DEFAULT '', rotations BIGINT NOT NULL DEFAULT 0)`,
		`CREATE TABLE IF NOT EXISTS ipv6_egress_cooldowns(ip TEXT PRIMARY KEY, until_time BIGINT NOT NULL DEFAULT 0)`,
	} {
		if _, err := db.conn.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("initialize IPv6 egress: %w", err)
		}
	}
	return nil
}

type ipv6ConfigScanner interface{ Scan(...any) error }

func scanIPv6Config(row ipv6ConfigScanner) (IPv6EgressConfig, error) {
	var c IPv6EgressConfig
	var raw string
	err := row.Scan(&c.Enabled, &raw, &c.CooldownSeconds, &c.MaxAttempts, &c.Retry5xx, &c.Revision)
	if err == nil {
		err = json.Unmarshal([]byte(raw), &c.SourceIPs)
	}
	return c, err
}

const ipv6ConfigSelect = `SELECT enabled,source_ips,cooldown_seconds,max_attempts,retry_5xx,revision FROM ipv6_egress_config WHERE id=1`

func (db *DB) IPv6EgressConfig(ctx context.Context) (IPv6EgressConfig, error) {
	return scanIPv6Config(db.conn.QueryRowContext(ctx, ipv6ConfigSelect))
}

func (db *DB) SaveIPv6EgressConfig(ctx context.Context, c IPv6EgressConfig) error {
	if c.CooldownSeconds < 30 || c.CooldownSeconds > 86400 || c.MaxAttempts < 1 || c.MaxAttempts > 5 {
		return errors.New("invalid IPv6 cooldown or attempt limit")
	}
	raw, err := json.Marshal(c.SourceIPs)
	if err != nil {
		return err
	}
	result, err := db.conn.ExecContext(ctx, `UPDATE ipv6_egress_config SET enabled=$1,source_ips=$2,cooldown_seconds=$3,max_attempts=$4,retry_5xx=$5,revision=revision+1 WHERE id=1 AND revision=$6`, c.Enabled, string(raw), c.CooldownSeconds, c.MaxAttempts, c.Retry5xx, c.Revision)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err == nil && n == 0 {
		return ErrIPv6ConfigConflict
	}
	return err
}

// A single database lock serializes claims across workers and blue/green instances.
// expectedIP fences delayed failures so they cannot rotate a newer assignment.
func (db *DB) ClaimIPv6Egress(ctx context.Context, accountID int64, localIPs []string, expectedIP, reason string, now int64) (IPv6Binding, IPv6EgressConfig, error) {
	var b IPv6Binding
	var c IPv6EgressConfig
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return b, c, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `UPDATE ipv6_egress_config SET revision=revision WHERE id=1`); err != nil {
		return b, c, err
	}
	c, err = scanIPv6Config(tx.QueryRowContext(ctx, ipv6ConfigSelect))
	if err != nil || !c.Enabled {
		return b, c, err
	}
	err = tx.QueryRowContext(ctx, `SELECT account_id,ip,changed_at,reason,rotations FROM ipv6_egress_bindings WHERE account_id=$1`, accountID).Scan(&b.AccountID, &b.IP, &b.ChangedAt, &b.Reason, &b.Rotations)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return b, c, err
	}
	candidates := make([]string, 0, len(localIPs))
	for _, ip := range localIPs {
		if len(c.SourceIPs) == 0 || slices.Contains(c.SourceIPs, ip) {
			candidates = append(candidates, ip)
		}
	}
	cooldowns := map[string]int64{}
	rows, err := tx.QueryContext(ctx, `SELECT ip,until_time FROM ipv6_egress_cooldowns WHERE until_time>$1`, now)
	if err != nil {
		return b, c, err
	}
	for rows.Next() {
		var ip string
		var until int64
		if err = rows.Scan(&ip, &until); err != nil {
			rows.Close()
			return b, c, err
		}
		cooldowns[ip] = until
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return b, c, err
	}
	if expectedIP != "" && b.IP == expectedIP {
		until := now + int64(c.CooldownSeconds)
		_, err = tx.ExecContext(ctx, `INSERT INTO ipv6_egress_cooldowns(ip,until_time) VALUES($1,$2) ON CONFLICT(ip) DO UPDATE SET until_time=$2`, b.IP, until)
		if err != nil {
			return b, c, err
		}
		cooldowns[b.IP] = until
	}
	if b.IP != "" && slices.Contains(candidates, b.IP) && cooldowns[b.IP] <= now {
		return b, c, tx.Commit()
	}
	used := map[string]bool{}
	rows, err = tx.QueryContext(ctx, `SELECT ip FROM ipv6_egress_bindings WHERE account_id<>$1`, accountID)
	if err != nil {
		return b, c, err
	}
	for rows.Next() {
		var ip string
		if err = rows.Scan(&ip); err != nil {
			rows.Close()
			return b, c, err
		}
		used[ip] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return b, c, err
	}
	next := ""
	for _, ip := range candidates {
		if !used[ip] && cooldowns[ip] <= now {
			next = ip
			break
		}
	}
	if next == "" {
		if err = tx.Commit(); err != nil {
			return b, c, err
		}
		return b, c, ErrIPv6PoolExhausted
	}
	if b.IP != "" {
		b.Rotations++
	} else {
		reason = "assigned"
	}
	b.AccountID = accountID
	b.IP = next
	b.ChangedAt = now
	b.Reason = reason
	_, err = tx.ExecContext(ctx, `INSERT INTO ipv6_egress_bindings(account_id,ip,changed_at,reason,rotations) VALUES($1,$2,$3,$4,$5) ON CONFLICT(account_id) DO UPDATE SET ip=$2,changed_at=$3,reason=$4,rotations=$5`, b.AccountID, b.IP, b.ChangedAt, b.Reason, b.Rotations)
	if err != nil {
		return b, c, err
	}
	return b, c, tx.Commit()
}

func (db *DB) IPv6EgressStatus(ctx context.Context, now int64) ([]IPv6Binding, []IPv6Cooldown, error) {
	bindings := []IPv6Binding{}
	cooldowns := []IPv6Cooldown{}
	rows, err := db.conn.QueryContext(ctx, `SELECT account_id,ip,changed_at,reason,rotations FROM ipv6_egress_bindings ORDER BY account_id`)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var b IPv6Binding
		if err = rows.Scan(&b.AccountID, &b.IP, &b.ChangedAt, &b.Reason, &b.Rotations); err != nil {
			rows.Close()
			return nil, nil, err
		}
		bindings = append(bindings, b)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, nil, err
	}
	rows, err = db.conn.QueryContext(ctx, `SELECT ip,until_time FROM ipv6_egress_cooldowns WHERE until_time>$1 ORDER BY until_time`, now)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var c IPv6Cooldown
		if err = rows.Scan(&c.IP, &c.Until); err != nil {
			rows.Close()
			return nil, nil, err
		}
		cooldowns = append(cooldowns, c)
	}
	err = rows.Err()
	rows.Close()
	return bindings, cooldowns, err
}

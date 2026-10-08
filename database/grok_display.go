package database

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Presentation only; never used for authorization or written into credentials.
type GrokPlanDisplay struct {
	Plan       string     `json:"plan"`
	Source     string     `json:"source"`
	Status     string     `json:"status"`
	ObservedAt *time.Time `json:"observed_at,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
}
type GrokModelSummary struct {
	Models    []string   `json:"models"`
	Status    string     `json:"status"`
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
}
type grokDisplayFact struct {
	Plan, Kind, Status    string
	HTTPStatus            int
	ObservedAt, ExpiresAt time.Time
}

func chooseGrokPlanDisplay(historical string, facts map[string]grokDisplayFact, now time.Time) *GrokPlanDisplay {
	for _, kind := range []string{GrokFactUser, GrokFactSettings} {
		f := facts[kind]
		if f.Plan != "" && f.Status == "ok" && f.HTTPStatus >= 200 && f.HTTPStatus < 300 && now.Before(f.ExpiresAt) {
			return &GrokPlanDisplay{Plan: f.Plan, Source: kind, Status: "fresh", ObservedAt: &f.ObservedAt, ExpiresAt: &f.ExpiresAt}
		}
	}
	// Failed reads are not evidence of Free. Retained payload is historical only.
	for _, kind := range []string{GrokFactUser, GrokFactSettings} {
		f := facts[kind]
		if f.Plan != "" {
			return &GrokPlanDisplay{Plan: f.Plan, Source: kind, Status: "stale", ObservedAt: &f.ObservedAt, ExpiresAt: &f.ExpiresAt}
		}
	}
	status := "unknown"
	if strings.TrimSpace(historical) != "" {
		status = "stale"
	}
	return &GrokPlanDisplay{Plan: historical, Source: "historical", Status: status}
}

// HydrateGrokDisplay batches sanitized facts, including disabled accounts.
// No tokens or complete credential documents are selected.
func (db *DB) HydrateGrokDisplay(ctx context.Context, accounts []*AccountRow) error {
	byID := map[int64]*AccountRow{}
	args := []any{}
	placeholders := []string{}
	for _, a := range accounts {
		if a != nil && strings.EqualFold(a.GetCredential("upstream_type"), "grok") {
			byID[a.ID] = a
			a.GrokModels = &GrokModelSummary{Models: []string{}, Status: "unknown"}
			args = append(args, a.ID)
			placeholders = append(placeholders, fmt.Sprintf("$%d", len(args)))
		}
	}
	if len(byID) == 0 {
		return nil
	}
	// Full-list snapshots scan once rather than issuing an N+1 query per account.
	filter := ""
	if len(args) <= 500 {
		filter = " AND s.account_id IN (" + strings.Join(placeholders, ",") + ")"
	} else {
		args = nil
	}
	planExpr := `COALESCE(NULLIF(BTRIM(s.payload_json->>'subscriptionTier'),''),NULLIF(BTRIM(s.payload_json->>'subscription_tier'),''),NULLIF(BTRIM(s.payload_json->>'subscription_tier_display'),''),'')`
	if db.isSQLite() {
		planExpr = `COALESCE(NULLIF(TRIM(json_extract(s.payload_json,'$.subscriptionTier')),''),NULLIF(TRIM(json_extract(s.payload_json,'$.subscription_tier')),''),NULLIF(TRIM(json_extract(s.payload_json,'$.subscription_tier_display')),''),'')`
	}
	rows, err := db.conn.QueryContext(ctx, `SELECT s.account_id,s.credential_generation,s.fact_kind,s.status,s.http_status,`+planExpr+`,s.observed_at,s.expires_at FROM grok_account_fact_snapshots s WHERE s.fact_kind IN ('user','settings')`+filter, args...)
	if err != nil {
		return err
	}
	facts := map[int64]map[string]grokDisplayFact{}
	for rows.Next() {
		var id, generation int64
		var f grokDisplayFact
		var observed, expires any
		if err = rows.Scan(&id, &generation, &f.Kind, &f.Status, &f.HTTPStatus, &f.Plan, &observed, &expires); err != nil {
			rows.Close()
			return err
		}
		a := byID[id]
		if a == nil || max(a.CredentialGeneration, 1) != generation {
			continue
		}
		f.ObservedAt, err = parseDBTimeValue(observed)
		if err != nil {
			rows.Close()
			return err
		}
		f.ExpiresAt, err = parseDBTimeValue(expires)
		if err != nil {
			rows.Close()
			return err
		}
		if facts[id] == nil {
			facts[id] = map[string]grokDisplayFact{}
		}
		facts[id][f.Kind] = f
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	now := time.Now()
	for id, a := range byID {
		a.GrokPlanDisplay = chooseGrokPlanDisplay(a.GetCredential("plan_type"), facts[id], now)
		if a.GetCredential("api_key") != "" {
			a.GrokPlanDisplay = &GrokPlanDisplay{Plan: "api", Source: "credential_kind", Status: "fresh"}
		}
	}
	rows, err = db.conn.QueryContext(ctx, `SELECT s.account_id,s.credential_generation,s.status,s.observed_at,s.expires_at,COALESCE(i.model_id,''),COALESCE(i.hidden,false),
 COALESCE(i.supported_in_api,false),COALESCE(i.field_presence_json,'{}')
 FROM grok_model_catalog_snapshots s LEFT JOIN grok_model_catalog_items i ON i.account_id=s.account_id AND i.origin=s.origin AND i.credential_generation=s.credential_generation
 WHERE 1=1`+filter, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	seen := map[int64]map[string]bool{}
	for rows.Next() {
		var id, generation int64
		var status, model string
		var observed, expires, presenceRaw any
		var hidden, supported bool
		if err = rows.Scan(&id, &generation, &status, &observed, &expires, &model, &hidden, &supported, &presenceRaw); err != nil {
			return err
		}
		a := byID[id]
		if a == nil || max(a.CredentialGeneration, 1) != generation {
			continue
		}
		at, e := parseDBTimeValue(observed)
		if e != nil {
			return e
		}
		until, e := parseDBTimeValue(expires)
		if e != nil {
			return e
		}
		summary := a.GrokModels
		if summary.UpdatedAt == nil || at.After(*summary.UpdatedAt) {
			summary.UpdatedAt = &at
		}
		if summary.Status == "unknown" {
			summary.Status = "fresh"
		}
		if status != "ok" || !now.Before(until) {
			summary.Status = "stale"
		}
		presence := decodeCredentials(presenceRaw)
		if a.GetCredential("api_key") != "" && presence["supported_in_api"] == "value" && !supported {
			continue
		}
		if model != "" && !hidden {
			if seen[id] == nil {
				seen[id] = map[string]bool{}
			}
			if !seen[id][model] {
				summary.Models = append(summary.Models, model)
				seen[id][model] = true
			}
		}
	}
	for _, a := range byID {
		sort.Strings(a.GrokModels.Models)
	}
	return rows.Err()
}

package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

const daybreakManifestLimit = 8 << 20

// ParseDaybreakManifest 只接受逐账号返回的显式权限，不从模型名称推测。
func ParseDaybreakManifest(body []byte) (map[string][]string, error) {
	var catalog struct {
		Models []struct {
			Slug       string `json:"slug"`
			Visibility string `json:"visibility"`
			Specialty  string `json:"model_specialty"`
			Access     *struct {
				Cyber []string `json:"cyber"`
			} `json:"available_access_programs"`
		} `json:"models"`
	}
	if len(body) > daybreakManifestLimit {
		return nil, fmt.Errorf("model manifest too large")
	}
	if err := json.Unmarshal(body, &catalog); err != nil {
		return nil, err
	}
	if catalog.Models == nil {
		return nil, fmt.Errorf("missing models array")
	}
	models := make(map[string][]string)
	for _, item := range catalog.Models {
		model := strings.ToLower(strings.TrimSpace(item.Slug))
		// 仅给选单中的普通模型派生切换别名；Auto Review 是内部审查模型。
		if model == "" || item.Visibility != "list" || model == "codex-auto-review" ||
			strings.EqualFold(item.Specialty, "cyber") || item.Access == nil {
			continue
		}
		programs := knownDaybreakPrograms(item.Access.Cyber)
		if len(programs) > 0 {
			models[model] = programs
		}
	}
	return models, nil
}

func knownDaybreakPrograms(values []string) []string {
	var programs []string
	for _, known := range []string{auth.DaybreakBlue, auth.DaybreakRed} {
		for _, value := range values {
			if value == known {
				programs = append(programs, known)
				break
			}
		}
	}
	return programs
}

type DaybreakObservation struct {
	Account  *auth.Account
	Snapshot database.DaybreakSnapshot
	Body     []byte
}

// Save 写盘成功后才发布；调用方在发起请求前捕获身份与时间。
func (observation DaybreakObservation) Save(ctx context.Context, db *database.DB) error {
	account, snapshot := observation.Account, observation.Snapshot
	models, err := ParseDaybreakManifest(observation.Body)
	if err != nil {
		return err
	}
	if db == nil || account.DaybreakIdentity() != snapshot.Identity {
		return fmt.Errorf("account identity changed")
	}
	snapshot.Models = models
	snapshot.CheckedAt = snapshot.ObservedAt
	if err := db.SaveDaybreakSnapshot(ctx, account.ID(), snapshot); err != nil {
		return err
	}
	stored, err := db.LoadDaybreakSnapshot(ctx, account.ID())
	if err == nil {
		account.ApplyDaybreakSnapshot(stored)
	}
	return err
}

func DaybreakModelIDs(accounts []*auth.Account, baseModels []string) []string {
	allowed := make(map[string]bool, len(baseModels))
	for _, model := range baseModels {
		allowed[strings.ToLower(model)] = true
	}
	seen := make(map[string]bool)
	for _, account := range accounts {
		if account == nil || !account.DaybreakCatalogEligible() {
			continue
		}
		for _, alias := range account.DaybreakAliases() {
			base, _ := auth.ParseDaybreakAlias(alias)
			if allowed[base] {
				seen[alias] = true
			}
		}
	}
	models := make([]string, 0, len(seen))
	for alias := range seen {
		models = append(models, alias)
	}
	sort.Strings(models)
	return models
}

func addDaybreakScopedModels(records map[string]*scopedModelRecord, account *auth.Account, catalog []ModelInfo) {
	allowed := make(map[string]bool)
	for _, item := range catalog {
		allowed[item.ID] = item.Enabled && (!item.ProOnly || isSparkPlanCandidate(account.GetPlanType()))
	}
	for _, alias := range account.DaybreakAliases() {
		base, _ := auth.ParseDaybreakAlias(alias)
		if !allowed[base] {
			continue
		}
		addScopedModel(records, alias, modelBackingCodex, time.Time{}, true)
	}
}

package admin

import (
	"context"
	"log"
	"time"

	"github.com/codex2api/proxy"
)

func (h *Handler) refreshImportedDaybreak(ctx context.Context, id int64) {
	account := h.store.FindByID(id)
	if account == nil || !isCodexOAuthAccount(account) || account.IsCodexAgentIdentity() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	snapshot := account.BeginDaybreakObservation()
	manifest, err := proxy.FetchCodexModelsManifest(ctx, account, h.store.ResolveProxyForAccount(account), "", "", nil)
	if err == nil {
		err = (proxy.DaybreakObservation{Account: account, Snapshot: snapshot, Body: manifest.Body}).Save(ctx, h.db)
		if err == nil {
			_, _ = proxy.LearnModelsFromManifest(ctx, h.db, manifest.Body, time.Now())
		}
	}
	if err != nil {
		log.Printf("[账号 %d] Daybreak 能力检查失败，保留已有结果: %v", id, err)
	}
}

func (h *Handler) codexPricingModelIDs(ctx context.Context) []string {
	models := proxy.SupportedModelIDs(ctx, h.db)
	if h.store != nil {
		models = append(models, proxy.DaybreakModelIDs(h.store.Accounts(), models)...)
	}
	return models
}

func (h *Handler) addDaybreakCatalogModels(catalog *proxy.ModelCatalog) {
	if h.store == nil || catalog == nil {
		return
	}
	aliases := proxy.DaybreakModelIDs(h.store.Accounts(), catalog.Models)
	known := make(map[string]bool, len(catalog.Items))
	for _, item := range catalog.Items {
		known[item.ID] = true
	}
	for _, alias := range aliases {
		if known[alias] {
			continue
		}
		catalog.Models = append(catalog.Models, alias)
		catalog.Items = append(catalog.Items, proxy.ModelInfo{
			ID: alias, Enabled: true, Category: "codex", Source: "daybreak",
		})
	}
}

package admin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

// The shape-selectable candy benchmark used by Sub2API quality operations.
// Nine round candies and twelve stars guarantee a matching pair; blind draws
// are a different problem. The explicit selection rule removes that ambiguity.
const modelQualityPrompt = `一个不透明袋子中有苹果味、桃子味、西瓜味糖果，各有圆形和五角星形。数量如下：
             苹果味 桃子味 西瓜味
圆形           7      9      8
五角星形       7      6      4
你能用手感区分形状，并可按形状选择摸取，但取出前无法辨别口味。取出后不放回。
你需要事先确定圆形和五角星形各取多少颗，保证取出的糖果中至少有一颗苹果味和一颗桃子味，且这两颗形状不同（圆形苹果与星形桃子，或星形苹果与圆形桃子均可）。
采用最优取法，最少总共需要取多少颗？只返回 {"answer":整数}。`

var modelQualityAnswer = regexp.MustCompile(`^\s*\{\s*"answer"\s*:\s*(-?(?:0|[1-9][0-9]*))\s*\}\s*$`)

func gradeModelQuality(output string, complete bool, probeError string) (string, string) {
	if probeError != "" {
		return "error", probeError
	}
	if !complete {
		return "error", "检测未完整结束，保留上次结果"
	}
	output = strings.TrimSpace(output)
	if strings.HasPrefix(output, "```json\n") && strings.HasSuffix(output, "```") {
		output = strings.TrimSuffix(strings.TrimPrefix(output, "```json\n"), "```")
	}
	match := modelQualityAnswer.FindStringSubmatch(output)
	if match == nil {
		return "error", "无法可靠解析答案，保留上次结果"
	}
	answer, err := strconv.Atoi(match[1])
	if err != nil {
		return "error", "答案超出可解析范围，保留上次结果"
	}
	if answer == 21 {
		return "pass", "糖果题通过：21"
	}
	return "fail", fmt.Sprintf("糖果题未通过：回答 %d，标准答案 21", answer)
}

type modelQualityRunner struct {
	h     *Handler
	mu    sync.Mutex // serializes publishing snapshots with administrator changes
	wg    sync.WaitGroup
	wake  chan struct{}
	probe func(context.Context, *auth.Account, string) (string, string)
}

func (h *Handler) StartModelQuality(ctx context.Context) error {
	r := &modelQualityRunner{h: h, wake: make(chan struct{}, 1)}
	r.probe = h.probeModelQuality
	h.modelQuality = r
	if _, _, err := r.refresh(ctx); err != nil {
		return err
	}
	r.wg.Add(1)
	go func() { defer r.wg.Done(); r.run(ctx) }()
	return nil
}

func (h *Handler) WaitModelQuality() {
	if h.modelQuality != nil {
		h.modelQuality.wg.Wait()
	}
}

func (r *modelQualityRunner) refresh(ctx context.Context) (database.ModelQualityConfig, []database.ModelQualityState, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.refreshLocked(ctx)
}

func (r *modelQualityRunner) refreshLocked(ctx context.Context) (database.ModelQualityConfig, []database.ModelQualityState, error) {
	cfg, states, err := r.h.db.ModelQualitySnapshot(ctx, time.Now().Unix())
	if err == nil {
		r.h.store.ApplyModelQualitySnapshot(cfg, states)
	}
	return cfg, states, err
}

func qualityAccountGeneration(a *auth.Account) int64 {
	a.Mu().RLock()
	defer a.Mu().RUnlock()
	return a.CredentialGeneration
}
func qualityAccountEligible(a *auth.Account) bool {
	return a != nil && !a.IsRelayStyle() && !a.IsClaudeOAuth() && !a.IsAntigravityAPI()
}
func qualityStateKey(id int64, model string) string { return strconv.FormatInt(id, 10) + ":" + model }

func (r *modelQualityRunner) run(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	active := make(map[int64]bool)
	done := make(chan int64, 4)
	var workers sync.WaitGroup
	defer workers.Wait()
	for {
		if ctx.Err() != nil {
			return
		}
		cfg, states, err := r.refresh(ctx)
		if err != nil && ctx.Err() == nil {
			log.Printf("[model-quality] snapshot failed: %v", err)
		}
		if err == nil && cfg.Enabled && len(active) < 4 {
			byKey := make(map[string]database.ModelQualityState, len(states))
			for _, s := range states {
				byKey[qualityStateKey(s.AccountID, s.Model)] = s
			}
			accounts := r.h.store.Accounts()
			// Stable order plus next_check_at prevents one account monopolizing the queue.
			sort.Slice(accounts, func(i, j int) bool { return accounts[i].DBID < accounts[j].DBID })
			for _, a := range accounts {
				if len(active) >= 4 {
					break
				}
				if !qualityAccountEligible(a) || active[a.DBID] || !a.IsAvailable() {
					continue
				}
				for _, model := range cfg.Models {
					if !a.SupportsCodexModel(model) || a.IsModelRateLimited(model) {
						continue
					}
					generation := qualityAccountGeneration(a)
					state, exists := byKey[qualityStateKey(a.DBID, model)]
					if exists && state.Generation == generation && (state.NextCheckAt > time.Now().Unix() || state.Running) {
						continue
					}
					if !exists || state.Generation != generation {
						if err := r.h.db.EnsureModelQualityState(ctx, a.DBID, generation, model); err != nil {
							continue
						}
					}
					var nonce [16]byte
					if _, err := rand.Read(nonce[:]); err != nil {
						break
					}
					owner := hex.EncodeToString(nonce[:])
					claimed, err := r.h.db.ClaimModelQuality(ctx, a.DBID, model, owner, cfg.Revision, time.Now().Unix())
					if err != nil || !claimed {
						continue
					}
					if !r.h.store.AcquireModelQualityProbe(a) {
						if err := r.h.db.ReleaseModelQuality(ctx, a.DBID, owner); err != nil {
							log.Printf("[model-quality] release account=%d: %v", a.DBID, err)
						}
						break
					}
					active[a.DBID] = true
					workers.Add(1)
					go func() {
						defer workers.Done()
						defer r.h.store.Release(a)
						defer func() { done <- a.DBID }()
						r.execute(ctx, a, model, owner, cfg.Revision, generation)
					}()
					break
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case id := <-done:
			delete(active, id)
		case <-ticker.C:
		case <-r.wake:
		}
	}
}

func (r *modelQualityRunner) execute(parent context.Context, a *auth.Account, model, owner string, revision, generation int64) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	stop := make(chan struct{})
	watched := make(chan struct{})
	go func() {
		defer close(watched)
		tick := time.NewTicker(5 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-tick.C:
				ok, err := r.h.db.RenewModelQuality(ctx, a.DBID, owner, revision, time.Now().Unix())
				if err != nil || !ok || r.h.store.FindByID(a.DBID) != a || qualityAccountGeneration(a) != generation || !a.IsAvailable() {
					cancel()
					return
				}
			}
		}
	}()
	defer func() {
		close(stop)
		<-watched
		if err := r.h.db.ReleaseModelQuality(context.Background(), a.DBID, owner); err != nil {
			log.Printf("[model-quality] release account=%d: %v", a.DBID, err)
		}
	}()
	outcome, reason := r.probe(ctx, a, model)
	if ctx.Err() != nil {
		return
	}
	state := database.ModelQualityState{AccountID: a.DBID, Model: model, Generation: generation, LastOutcome: outcome, Reason: reason}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := r.h.db.FinishModelQuality(ctx, state, owner, revision, time.Now().Unix()); err != nil {
		log.Printf("[model-quality] save account=%d: %v", a.DBID, err)
		return
	}
	if _, _, err := r.refreshLocked(ctx); err != nil {
		log.Printf("[model-quality] publish failed: %v", err)
	}
}

func (h *Handler) probeModelQuality(ctx context.Context, a *auth.Account, model string) (string, string) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var output strings.Builder
	var probeError string
	var complete bool
	emit := func(e testEvent) {
		switch e.Type {
		case "content":
			if output.Len()+len(e.Text) > 8192 {
				probeError = "检测输出过长，保留上次结果"
				cancel()
			} else {
				output.WriteString(e.Text)
			}
		case "error":
			if probeError == "" {
				probeError = sanitizeCodexTestText(e.Error, codexTestSecrets(a))
			}
		case "test_complete":
			complete = e.Success
		}
	}
	req := qualityTestRequest{Model: model, Prompt: modelQualityPrompt, ReasoningEffort: "high", modelQualityProbe: true}
	router := gin.New()
	router.POST("/accounts/:id/test", func(c *gin.Context) { h.testConnection(c, &req) })
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("/accounts/%d/test", a.DBID), nil)
	if err != nil {
		return "error", "无法创建检测请求"
	}
	router.ServeHTTP(&qualityJobWriter{header: make(http.Header), emit: emit}, request)
	if len([]rune(probeError)) > 300 {
		probeError = string([]rune(probeError)[:300])
	}
	return gradeModelQuality(output.String(), complete, probeError)
}

type modelQualityAccountView struct {
	ID        int64                        `json:"id"`
	Name      string                       `json:"name"`
	Available bool                         `json:"available"`
	States    []database.ModelQualityState `json:"states"`
}

func (h *Handler) GetModelQuality(c *gin.Context) {
	if h.modelQuality == nil {
		c.JSON(503, gin.H{"error": "自动检测服务不可用"})
		return
	}
	cfg, states, err := h.modelQuality.refresh(c.Request.Context())
	if err != nil {
		c.JSON(500, gin.H{"error": "读取检测状态失败"})
		return
	}
	byKey := make(map[string]database.ModelQualityState, len(states))
	for _, s := range states {
		byKey[qualityStateKey(s.AccountID, s.Model)] = s
	}
	models := make(map[string]bool)
	for _, m := range proxy.TextTestModelIDs(c.Request.Context(), h.db) {
		if isTextConnectionModel(m) && !strings.ContainsAny(m, "()") && m != "codex-auto-review" {
			models[m] = true
		}
	}
	for _, m := range cfg.Models {
		models[m] = true
	}
	accounts := h.store.Accounts()
	sort.Slice(accounts, func(i, j int) bool { return accounts[i].DBID > accounts[j].DBID })
	search := strings.ToLower(strings.TrimSpace(c.Query("search")))
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	if page < 1 {
		page = 1
	}
	views := make([]modelQualityAccountView, 0)
	total := 0
	for _, a := range accounts {
		if !qualityAccountEligible(a) {
			continue
		}
		for _, m := range a.CodexModels() {
			if isTextConnectionModel(m) && !strings.ContainsAny(m, "()") && m != "codex-auto-review" {
				models[m] = true
			}
		}
		a.Mu().RLock()
		name := a.Email
		generation := a.CredentialGeneration
		a.Mu().RUnlock()
		if name == "" {
			name = fmt.Sprintf("ID %d", a.DBID)
		}
		if search != "" && !strings.Contains(strings.ToLower(name), search) && !strings.Contains(strconv.FormatInt(a.DBID, 10), search) {
			continue
		}
		total++
		if total <= (page-1)*30 || total > page*30 {
			continue
		}
		view := modelQualityAccountView{ID: a.DBID, Name: name, Available: a.IsAvailable(), States: make([]database.ModelQualityState, 0)}
		for _, m := range cfg.Models {
			s, ok := byKey[qualityStateKey(a.DBID, m)]
			if !ok || s.Generation != generation {
				s = database.ModelQualityState{AccountID: a.DBID, Model: m, Status: "pending"}
			}
			if !a.SupportsCodexModel(m) {
				s.Status = "unsupported"
			}
			view.States = append(view.States, s)
		}
		views = append(views, view)
	}
	options := make([]string, 0, len(models))
	for m := range models {
		options = append(options, m)
	}
	sort.Strings(options)
	c.JSON(200, gin.H{"config": cfg, "models": options, "accounts": views, "total": total, "page": page, "page_size": 30, "interval_seconds": database.ModelQualityIntervalSeconds})
}

func (h *Handler) UpdateModelQuality(c *gin.Context) {
	if h.modelQuality == nil {
		c.JSON(503, gin.H{"error": "自动检测服务不可用"})
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 16384)
	var cfg database.ModelQualityConfig
	if err := c.ShouldBindJSON(&cfg); err != nil {
		c.JSON(400, gin.H{"error": "无效的检测配置"})
		return
	}
	if len(cfg.Models) > 20 || cfg.Enabled && len(cfg.Models) == 0 {
		c.JSON(400, gin.H{"error": "开启前请选择 1–20 个模型"})
		return
	}
	for i, m := range cfg.Models {
		m = strings.ToLower(strings.TrimSpace(m))
		if m == "" || len(m) > 200 || !isTextConnectionModel(m) || strings.ContainsAny(m, "()") || m == "codex-auto-review" {
			c.JSON(400, gin.H{"error": "请选择可用的文本模型"})
			return
		}
		cfg.Models[i] = m
	}
	sort.Strings(cfg.Models)
	cfg.Models = slices.Compact(cfg.Models)
	if cfg.Models == nil {
		cfg.Models = []string{}
	}
	r := h.modelQuality
	r.mu.Lock()
	_, states, err := h.db.ModelQualitySnapshot(c.Request.Context(), time.Now().Unix())
	if err == nil {
		err = h.db.SaveModelQualityConfig(c.Request.Context(), cfg)
	}
	if err == nil {
		cfg.Revision++
		h.store.ApplyModelQualitySnapshot(cfg, states)
	}
	r.mu.Unlock()
	if errors.Is(err, database.ErrModelQualityConflict) {
		c.JSON(409, gin.H{"error": "配置已被修改，请刷新后重试"})
		return
	}
	if err != nil {
		c.JSON(500, gin.H{"error": "保存检测配置失败，请刷新核对状态"})
		return
	}
	select {
	case r.wake <- struct{}{}:
	default:
	}
	c.JSON(200, gin.H{"config": cfg})
}

func (h *Handler) RetestModelQuality(c *gin.Context) {
	if h.modelQuality == nil {
		c.JSON(503, gin.H{"error": "自动检测服务不可用"})
		return
	}
	var req struct {
		AccountID int64  `json:"account_id"`
		Model     string `json:"model"`
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 4096)
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": "无效的检测请求"})
		return
	}
	cfg, _, err := h.modelQuality.refresh(c.Request.Context())
	if err != nil {
		c.JSON(500, gin.H{"error": "读取检测配置失败"})
		return
	}
	a := h.store.FindByID(req.AccountID)
	if !cfg.Enabled || !slices.Contains(cfg.Models, req.Model) || !qualityAccountEligible(a) || !a.SupportsCodexModel(req.Model) {
		c.JSON(400, gin.H{"error": "请开启检测并选择该账号支持的模型"})
		return
	}
	if !a.IsAvailable() || a.IsModelRateLimited(req.Model) {
		c.JSON(409, gin.H{"error": "账号或模型暂不可用，恢复后自动检测"})
		return
	}
	if err = h.db.EnsureModelQualityState(c.Request.Context(), a.DBID, qualityAccountGeneration(a), req.Model); err == nil {
		err = h.db.RetestModelQuality(c.Request.Context(), a.DBID, req.Model)
	}
	if err != nil {
		c.JSON(500, gin.H{"error": "提交复测失败"})
		return
	}
	select {
	case h.modelQuality.wake <- struct{}{}:
	default:
	}
	c.JSON(202, gin.H{"message": "已加入复测队列"})
}

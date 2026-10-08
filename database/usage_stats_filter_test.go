package database

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// 用量页选中某个账号后,顶部区间卡片曾经仍按全局口径显示(接口没有账号维度)。
// 现在区间字段(today_*/rpm/tpm/错误率/分项)跟随维度筛选,累计字段保持全局。
func TestGetUsageStatsFilteredNarrowsRangeFieldsKeepsTotals(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "stats-filter.db"))
	if err != nil {
		t.Fatalf("New(sqlite): %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	logs := []*UsageLogInput{
		{AccountID: 1, APIKeyID: 10, Endpoint: "/v1/responses", Model: "gpt-5.4", StatusCode: 200, TotalTokens: 100},
		{AccountID: 1, APIKeyID: 10, Endpoint: "/v1/responses", Model: "gpt-5.4", StatusCode: 500, TotalTokens: 50},
		{AccountID: 2, APIKeyID: 11, Endpoint: "/v1/responses", Model: "gpt-5.6-sol", StatusCode: 200, TotalTokens: 1000},
		{AccountID: 2, APIKeyID: 11, Endpoint: "/v1/responses", Model: "gpt-5.6-sol", StatusCode: 200, TotalTokens: 1000},
		{AccountID: 2, APIKeyID: 11, Endpoint: "/v1/responses", Model: "gpt-5.6-sol", StatusCode: 200, TotalTokens: 1000},
	}
	for _, input := range logs {
		if err := db.InsertUsageLog(ctx, input); err != nil {
			t.Fatalf("InsertUsageLog: %v", err)
		}
	}
	db.flushLogs()

	now := time.Now()
	start, end := now.Add(-time.Hour), now.Add(time.Hour)

	global, err := db.GetUsageStats(ctx, start, end, "")
	if err != nil {
		t.Fatalf("GetUsageStats: %v", err)
	}
	if global.TodayRequests != 5 || global.TotalRequests != 5 {
		t.Fatalf("global today=%d total=%d, want 5/5", global.TodayRequests, global.TotalRequests)
	}

	accountID := int64(1)
	byAccount, err := db.GetUsageStatsFiltered(ctx, start, end, "", UsageLogFilter{AccountID: &accountID}, true)
	if err != nil {
		t.Fatalf("GetUsageStatsFiltered(account): %v", err)
	}
	if byAccount.TodayRequests != 2 || byAccount.TodayTokens != 150 {
		t.Fatalf("account today=%d tokens=%d, want 2/150", byAccount.TodayRequests, byAccount.TodayTokens)
	}
	if byAccount.ErrorRate < 49.9 || byAccount.ErrorRate > 50.1 {
		t.Fatalf("account error rate = %.2f, want 50", byAccount.ErrorRate)
	}
	if byAccount.TotalRequests != 5 || byAccount.TotalTokens != global.TotalTokens {
		t.Fatalf("account totals = %d/%d, want global 5/%d (累计保持全局)", byAccount.TotalRequests, byAccount.TotalTokens, global.TotalTokens)
	}
	if len(byAccount.ModelStats) != 1 || byAccount.ModelStats[0].Model != "gpt-5.4" {
		t.Fatalf("account model stats = %+v, want only gpt-5.4", byAccount.ModelStats)
	}
	if byAccount.FeatureStats.ErrorRequests != 1 {
		t.Fatalf("account feature error requests = %d, want 1", byAccount.FeatureStats.ErrorRequests)
	}
	if len(byAccount.APIKeyStats) != 1 || byAccount.APIKeyStats[0].APIKeyID != 10 {
		t.Fatalf("account api key stats = %+v, want only key 10", byAccount.APIKeyStats)
	}

	// 搜索词命中模型名也能收窄。
	byQuery, err := db.GetUsageStatsFiltered(ctx, start, end, "", UsageLogFilter{Query: "5.6-sol"}, false)
	if err != nil {
		t.Fatalf("GetUsageStatsFiltered(query): %v", err)
	}
	if byQuery.TodayRequests != 3 || byQuery.TodayTokens != 3000 {
		t.Fatalf("query today=%d tokens=%d, want 3/3000", byQuery.TodayRequests, byQuery.TodayTokens)
	}

	// 状态类条件不是维度:带 ErrorOnly 的筛选与不带等价。
	if (UsageLogFilter{ErrorOnly: true, StatusFamily: "5xx"}).HasDimensionFilter() {
		t.Fatal("status-only filter must not count as dimension filter")
	}
	statusOnly, err := db.GetUsageStatsFiltered(ctx, start, end, "", UsageLogFilter{ErrorOnly: true, StatusFamily: "5xx"}, false)
	if err != nil {
		t.Fatalf("GetUsageStatsFiltered(status): %v", err)
	}
	if statusOnly.TodayRequests != 5 {
		t.Fatalf("status-only today=%d, want 5", statusOnly.TodayRequests)
	}
	if (UsageLogFilter{AccountID: &accountID}).DimensionKey() == (UsageLogFilter{Query: "x"}).DimensionKey() {
		t.Fatal("different dimension filters must have different keys")
	}
}

// 回归:搜索词不应命中账号凭据 JSON(如模型目录)。
// 真实环境里账号 credentials 含 "gpt-5.6-*" 模型目录,旧实现把
// CAST(credentials AS TEXT) 纳入 LIKE,导致搜 "5.6" 时该账号的 gpt-6-*、
// gpt-5.5 等不含搜索词的日志全部被带入搜索结果与统计卡片。
func TestUsageLogSearchIgnoresAccountCredentials(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "search-cred.db"))
	if err != nil {
		t.Fatalf("New(sqlite): %v", err)
	}
	defer db.Close()

	ctx := context.Background()

	// 账号 A 凭据含 5.6 模型目录(旧实现会命中 credentials LIKE),账号 B 凭据干净。
	accA, err := db.InsertAccountWithCredentials(ctx, "acc-a@example.com", map[string]interface{}{
		"models": []string{"codex-auto-review", "gpt-5.5", "gpt-5.6-luna", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-6-astra", "gpt-6-sol"},
	}, "")
	if err != nil {
		t.Fatalf("InsertAccountWithCredentials(A): %v", err)
	}
	accB, err := db.InsertAccountWithCredentials(ctx, "acc-b@example.com", map[string]interface{}{
		"models": []string{"gpt-6-astra", "gpt-6-luna"},
	}, "")
	if err != nil {
		t.Fatalf("InsertAccountWithCredentials(B): %v", err)
	}

	logs := []*UsageLogInput{
		{AccountID: accA, APIKeyID: 10, Endpoint: "/v1/responses", Model: "gpt-5.6-sol", StatusCode: 200, TotalTokens: 1000},
		{AccountID: accA, APIKeyID: 10, Endpoint: "/v1/responses", Model: "gpt-6-sol", StatusCode: 200, TotalTokens: 2000},
		{AccountID: accB, APIKeyID: 11, Endpoint: "/v1/responses", Model: "gpt-6-astra", StatusCode: 200, TotalTokens: 500},
		{AccountID: accB, APIKeyID: 11, Endpoint: "/v1/responses", Model: "gpt-5.5", StatusCode: 200, TotalTokens: 300},
	}
	for _, input := range logs {
		if err := db.InsertUsageLog(ctx, input); err != nil {
			t.Fatalf("InsertUsageLog: %v", err)
		}
	}
	db.flushLogs()

	now := time.Now()
	start, end := now.Add(-time.Hour), now.Add(time.Hour)

	// 搜 "5.6":只应命中 gpt-5.6-sol 一条,不得因账号凭据含 "5.6" 带入 gpt-6-*/gpt-5.5。
	byQuery, err := db.GetUsageStatsFiltered(ctx, start, end, "", UsageLogFilter{Query: "5.6"}, true)
	if err != nil {
		t.Fatalf("GetUsageStatsFiltered(query=5.6): %v", err)
	}
	if byQuery.TodayRequests != 1 || byQuery.TodayTokens != 1000 {
		t.Fatalf("query 5.6 today=%d tokens=%d, want 1/1000", byQuery.TodayRequests, byQuery.TodayTokens)
	}
	if len(byQuery.ModelStats) != 1 || byQuery.ModelStats[0].Model != "gpt-5.6-sol" {
		t.Fatalf("query 5.6 model stats = %+v, want only gpt-5.6-sol", byQuery.ModelStats)
	}

	// 分页日志(用量页列表)同样只返回含 "5.6" 的记录。
	page, err := db.ListUsageLogsByTimeRangePaged(ctx, UsageLogFilter{Start: start, End: end, Query: "5.6", Page: 1, PageSize: 50})
	if err != nil {
		t.Fatalf("ListUsageLogsByTimeRangePaged(query=5.6): %v", err)
	}
	if page.Total != 1 || len(page.Logs) != 1 || page.Logs[0].Model != "gpt-5.6-sol" {
		t.Fatalf("paged q=5.6 total=%d rows=%d first=%q, want 1/1/gpt-5.6-sol",
			page.Total, len(page.Logs), firstUsageLogModel(page.Logs))
	}

	// 按账号名/邮箱搜索仍然有效(name 匹配保留)。
	byName, err := db.GetUsageStatsFiltered(ctx, start, end, "", UsageLogFilter{Query: "acc-a@example.com"}, true)
	if err != nil {
		t.Fatalf("GetUsageStatsFiltered(query=account name): %v", err)
	}
	if byName.TodayRequests != 2 {
		t.Fatalf("query by account name today=%d, want 2", byName.TodayRequests)
	}
}

func firstUsageLogModel(logs []*UsageLog) string {
	if len(logs) > 0 {
		return logs[0].Model
	}
	return ""
}

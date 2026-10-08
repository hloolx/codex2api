package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

const modelTraceBankMaxBytes = 16 << 20

// The bank is fetched in two steps so the stored copy is pinned to an exact
// upstream commit: resolve main to a SHA, then download the bank at that SHA.
var (
	modelTraceLatestCommitURL = "https://api.github.com/repos/xqy2006/ModelTrace/commits/main"
	modelTraceBankURLTemplate = "https://raw.githubusercontent.com/xqy2006/ModelTrace/%s/static/data/unified_bank.json"
	modelTraceRevisionPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// SetModelTraceSourceURLsForTest replaces the upstream endpoints and returns a
// restore function. Production code must not call it.
func SetModelTraceSourceURLsForTest(latestCommitURL, bankURLTemplate string) (restore func()) {
	oldCommit, oldBank := modelTraceLatestCommitURL, modelTraceBankURLTemplate
	modelTraceLatestCommitURL, modelTraceBankURLTemplate = latestCommitURL, bankURLTemplate
	return func() { modelTraceLatestCommitURL, modelTraceBankURLTemplate = oldCommit, oldBank }
}

// ModelTraceEmbeddedBankForTest returns a copy of the embedded bank for tests.
func ModelTraceEmbeddedBankForTest() []byte { return append([]byte(nil), modelTraceBankJSON...) }

// FetchLatestModelTraceRevision returns the commit SHA of upstream main.
func FetchLatestModelTraceRevision(ctx context.Context, proxyURL string) (string, error) {
	body, err := fetchModelTraceURL(ctx, modelTraceLatestCommitURL, "application/vnd.github.sha", proxyURL, 4096)
	if err != nil {
		return "", err
	}
	revision := strings.ToLower(strings.TrimSpace(string(body)))
	if !modelTraceRevisionPattern.MatchString(revision) {
		return "", fmt.Errorf("ModelTrace 上游返回了无效的提交号")
	}
	return revision, nil
}

// FetchModelTraceBank downloads the bank at revision. Callers must still pass
// the bytes through ParseModelTraceBankOverride before using them.
func FetchModelTraceBank(ctx context.Context, revision, proxyURL string) ([]byte, error) {
	if !modelTraceRevisionPattern.MatchString(revision) {
		return nil, fmt.Errorf("无效的 ModelTrace 提交号")
	}
	return fetchModelTraceURL(ctx, fmt.Sprintf(modelTraceBankURLTemplate, revision), "application/json", proxyURL, modelTraceBankMaxBytes)
}

func fetchModelTraceURL(ctx context.Context, endpoint, accept, proxyURL string, limit int64) ([]byte, error) {
	reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("build ModelTrace request: %w", err)
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", "codex2api")
	ApplyGithubAuth(req)

	client, closeClient := newVersionSyncClient(endpoint, GithubProxyOrDefault(endpoint, proxyURL), 40*time.Second)
	defer closeClient()
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ModelTrace request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("ModelTrace upstream status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read ModelTrace response: %w", err)
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("ModelTrace 响应超过 %d 字节上限", limit)
	}
	return body, nil
}

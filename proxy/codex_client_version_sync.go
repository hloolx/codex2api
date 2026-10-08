package proxy

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/database"
)

type CodexAppBuildSyncResult struct {
	FetchedVersion   string                     `json:"fetched_version,omitempty"`
	SyncedVersion    string                     `json:"synced_version,omitempty"`
	EffectiveVersion string                     `json:"effective_version"`
	CLIVersion       string                     `json:"cli_version,omitempty"`
	Source           string                     `json:"source,omitempty"`
	Status           string                     `json:"status"`
	Targets          []CodexClientVersionTarget `json:"targets,omitempty"`
	Updated          bool                       `json:"updated"`
	Error            string                     `json:"error,omitempty"`
}

type CodexClientVersionsSyncResult struct {
	CLI            CodexAppBuildSyncResult `json:"cli"`
	DesktopMac     CodexAppBuildSyncResult `json:"desktop_mac"`
	DesktopWindows CodexAppBuildSyncResult `json:"desktop_windows"`
	VSCode         CodexAppBuildSyncResult `json:"vscode"`
}

type codexClientSyncKey struct {
	db       *database.DB
	proxyURL string
}
type codexClientSyncCall struct {
	done   chan struct{}
	result *CodexClientVersionsSyncResult
	err    error
}

var codexClientSyncMu sync.Mutex
var codexClientSyncCalls = make(map[codexClientSyncKey]*codexClientSyncCall)

// SyncCodexClientVersions 合并并发同步，单个来源失败不丢弃已验证配对。
func SyncCodexClientVersions(ctx context.Context, db *database.DB, proxyURL string) (*CodexClientVersionsSyncResult, error) {
	if db == nil {
		return nil, fmt.Errorf("数据库不可用，无法同步 Codex 客户端版本")
	}
	key := codexClientSyncKey{db: db, proxyURL: proxyURL}
	codexClientSyncMu.Lock()
	call := codexClientSyncCalls[key]
	if call == nil {
		call = &codexClientSyncCall{done: make(chan struct{})}
		codexClientSyncCalls[key] = call
		// 合并后的同步由所有调用者共享，不能随发起者取消而中断，因此脱离调用者 ctx 在后台执行。
		runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), codexClientSyncTimeout)
		go func() {
			defer cancel()
			call.result, call.err = codexClientVersionSyncRun(runCtx, key)
			codexClientSyncMu.Lock()
			delete(codexClientSyncCalls, key)
			close(call.done)
			codexClientSyncMu.Unlock()
		}()
	}
	codexClientSyncMu.Unlock()
	// 发起者与等待者一视同仁：各自按自己的 ctx 停止等待。
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-call.done:
		return call.result, call.err
	}
}

const codexClientSyncTimeout = 15 * time.Minute

var codexClientVersionSyncRun = runCodexClientVersionSync

func runCodexClientVersionSync(ctx context.Context, key codexClientSyncKey) (*CodexClientVersionsSyncResult, error) {
	if err := LoadCodexClientVersionCache(ctx, key.db); err != nil {
		return nil, err
	}
	client, closeClient := codexBuildHTTPClient(codexClientSources.MacARM64, key.proxyURL)
	defer closeClient()
	fetched := fetchCodexClientMetadata(ctx, codexClientMetadataInput{client: client, proxyURL: key.proxyURL})
	result := &CodexClientVersionsSyncResult{CLI: applyCodexIndependentCLI(ctx, key, fetched.cli)}
	candidates := fetched.candidates
	results := make([]codexTargetSyncResult, len(candidates))
	limit := make(chan struct{}, codexClientArtifactConcurrency)
	var wg sync.WaitGroup
	for i, candidate := range candidates {
		wg.Go(func() {
			limit <- struct{}{}
			defer func() { <-limit }()
			pair, err := resolveCodexClientCandidate(ctx, client, candidate)
			results[i] = persistCodexCandidateResult(ctx, key.db, codexCandidateResolution{candidate: candidate, pair: pair, err: err})
		})
	}
	wg.Wait()
	result.DesktopMac = summarizeCodexTargetSync(results[:2], "darwin-arm64")
	result.DesktopWindows = summarizeCodexTargetSync(results[2:4], "win32-x64")
	result.VSCode = summarizeCodexTargetSync(results[4:], "linux-x64")
	return result, nil
}

const codexClientArtifactConcurrency = 2

type codexCandidateResolution struct {
	candidate codexClientCandidate
	pair      CodexClientVersionPair
	err       error
}
type codexTargetSyncResult struct {
	target  CodexClientVersionTarget
	fetched string
	updated bool
}

func persistCodexCandidateResult(ctx context.Context, db *database.DB, resolution codexCandidateResolution) codexTargetSyncResult {
	candidate := resolution.candidate
	previous := codexClientVersionTarget(candidate.Kind, candidate.Target)
	target := newCodexClientVersionTarget(candidate.Kind, candidate.Target)
	result := codexTargetSyncResult{}
	if resolution.err != nil {
		target.Pairs = append([]CodexClientVersionPair(nil), previous.Pairs...)
		target.Error = resolution.err.Error()
		if len(previous.Pairs) > 0 {
			target.Status = "stale"
		}
		result.target = target
		return result
	}
	target.Status, target.Pairs = "verified", []CodexClientVersionPair{resolution.pair}
	result.fetched = resolution.pair.AppVersion
	result.updated = !codexTargetHasArtifact(previous, resolution.pair.ArtifactID)
	if !result.updated {
		target.Status = "cached"
	}
	saved, err := saveCodexClientVersionTarget(ctx, db, target)
	if err != nil {
		result.updated = false
		previous.Status, previous.Error = "error", err.Error()
		previous.ClientKind, previous.TargetPlatform = candidate.Kind, candidate.Target
		result.target = previous
		return result
	}
	result.target = saved
	return result
}

func codexTargetHasArtifact(target CodexClientVersionTarget, artifactID string) bool {
	for _, pair := range target.Pairs {
		if pair.ArtifactID == artifactID {
			return true
		}
	}
	return false
}

func applyCodexIndependentCLI(ctx context.Context, key codexClientSyncKey, fetched codexFetchOutcome) CodexAppBuildSyncResult {
	cli := &CodexCLIVersionSyncResult{BuiltinVersion: latestCodexCLIVersion, EffectiveVersion: effectiveLatestCodexCLIVersion()}
	err := fetched.err
	if err == nil {
		cli, err = applyCodexCLIVersion(ctx, key.db, cli, fetched.version)
	}
	result := CodexAppBuildSyncResult{EffectiveVersion: effectiveLatestCodexCLIVersion(), SyncedVersion: CurrentRuntimeSettings().CodexSyncedCLIVersion, Source: "official_github_release", Status: "verified"}
	result.CLIVersion = result.EffectiveVersion
	if err != nil {
		result.Error, result.Status = err.Error(), "error"
		return result
	}
	result.FetchedVersion, result.Updated = cli.FetchedVersion, cli.Updated
	return result
}

func summarizeCodexTargetSync(items []codexTargetSyncResult, defaultTarget string) CodexAppBuildSyncResult {
	result := CodexAppBuildSyncResult{Status: "error"}
	var failures []string
	for _, item := range items {
		result.Targets = append(result.Targets, item.target)
		result.Updated = result.Updated || item.updated
		if item.target.Error != "" {
			failures = append(failures, item.target.TargetPlatform+": "+item.target.Error)
		}
		if item.target.TargetPlatform != defaultTarget {
			continue
		}
		result.Status, result.FetchedVersion = item.target.Status, item.fetched
		if len(item.target.Pairs) > 0 {
			pair := item.target.Pairs[0]
			result.EffectiveVersion, result.SyncedVersion, result.CLIVersion, result.Source = pair.AppVersion, pair.AppVersion, pair.CLIVersion, pair.Source
		}
	}
	result.Error = strings.Join(failures, "; ")
	return result
}

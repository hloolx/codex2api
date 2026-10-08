package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codex2api/database"
)

type CodexClientVersionPair struct {
	AppVersion     string `json:"app_version"`
	CLIVersion     string `json:"cli_version"`
	PackageVersion string `json:"package_version,omitempty"`
	Source         string `json:"source"`
	ArtifactURL    string `json:"artifact_url,omitempty"`
	ArtifactID     string `json:"artifact_id,omitempty"`
	VerifiedAt     int64  `json:"verified_at"`
}

type CodexClientVersionTarget struct {
	ClientKind     string                   `json:"client_kind"`
	TargetPlatform string                   `json:"target_platform"`
	Status         string                   `json:"status"`
	Error          string                   `json:"error,omitempty"`
	CheckedAt      int64                    `json:"checked_at"`
	Pairs          []CodexClientVersionPair `json:"pairs"`
}

type codexClientVersionSnapshot struct {
	targets map[string]CodexClientVersionTarget
}

var codexClientVersions atomic.Pointer[codexClientVersionSnapshot]
var codexClientVersionsMu sync.Mutex

func codexClientVersionKey(kind, target string) string { return kind + "/" + target }

func codexClientVersionTarget(kind, target string) CodexClientVersionTarget {
	if snapshot := codexClientVersions.Load(); snapshot != nil {
		return snapshot.targets[codexClientVersionKey(kind, target)]
	}
	return CodexClientVersionTarget{}
}

// CurrentCodexClientVersions 返回副本，设置保存不会回滚版本快照。
func CurrentCodexClientVersions() []CodexClientVersionTarget {
	result := make([]CodexClientVersionTarget, 0)
	snapshot := codexClientVersions.Load()
	if snapshot == nil {
		return result
	}
	for _, target := range snapshot.targets {
		target.Pairs = append([]CodexClientVersionPair(nil), target.Pairs...)
		result = append(result, target)
	}
	sort.Slice(result, func(i, j int) bool {
		return codexClientVersionKey(result[i].ClientKind, result[i].TargetPlatform) < codexClientVersionKey(result[j].ClientKind, result[j].TargetPlatform)
	})
	return result
}

func validCodexClientPair(pair CodexClientVersionPair) bool {
	_, validApp := codexBuildParts(pair.AppVersion, 3)
	return validApp && validCodexClientVersionString(pair.CLIVersion) && pair.Source != "" && pair.ArtifactID != ""
}

func decodeCodexClientVersionTarget(raw string) (CodexClientVersionTarget, error) {
	var target CodexClientVersionTarget
	if err := json.Unmarshal([]byte(raw), &target); err != nil {
		return target, err
	}
	for _, pair := range target.Pairs {
		if !validCodexClientPair(pair) {
			return target, fmt.Errorf("invalid cached Codex version pair")
		}
	}
	return target, nil
}

func LoadCodexClientVersionCache(ctx context.Context, db *database.DB) error {
	codexClientVersionsMu.Lock()
	defer codexClientVersionsMu.Unlock()
	rows, err := db.GetCodexClientVersionCache(ctx)
	if err != nil {
		return err
	}
	snapshot := &codexClientVersionSnapshot{targets: make(map[string]CodexClientVersionTarget)}
	for _, row := range rows {
		target, err := decodeCodexClientVersionTarget(row.Payload)
		if err != nil {
			return err
		}
		if target.ClientKind != row.ClientKind || target.TargetPlatform != row.TargetPlatform {
			return fmt.Errorf("Codex cache key mismatch")
		}
		snapshot.targets[codexClientVersionKey(row.ClientKind, row.TargetPlatform)] = target
	}
	codexClientVersions.Store(snapshot)
	refreshCodexClientVersionProjections()
	return nil
}

func mergeCodexClientVersionTarget(current, next CodexClientVersionTarget) CodexClientVersionTarget {
	if current.CheckedAt > next.CheckedAt {
		return current
	}
	next.Pairs = append([]CodexClientVersionPair(nil), next.Pairs[:1]...)
	return next
}

func saveCodexClientVersionTarget(ctx context.Context, db *database.DB, target CodexClientVersionTarget) (CodexClientVersionTarget, error) {
	if target.Error != "" || len(target.Pairs) == 0 {
		return codexClientVersionTarget(target.ClientKind, target.TargetPlatform), nil
	}
	for _, pair := range target.Pairs {
		if !validCodexClientPair(pair) {
			return CodexClientVersionTarget{}, fmt.Errorf("incomplete Codex version pair")
		}
	}
	codexClientVersionsMu.Lock()
	defer codexClientVersionsMu.Unlock()
	key := database.CodexClientVersionCacheKey{ClientKind: target.ClientKind, TargetPlatform: target.TargetPlatform}
	raw, err := db.MutateCodexClientVersionCache(ctx, key, func(raw string) (string, error) {
		current, err := decodeCodexClientVersionTarget(raw)
		if err != nil {
			return "", err
		}
		merged := mergeCodexClientVersionTarget(current, target)
		data, err := json.Marshal(merged)
		return string(data), err
	})
	if err != nil {
		return CodexClientVersionTarget{}, err
	}
	saved, err := decodeCodexClientVersionTarget(raw)
	if err != nil {
		return CodexClientVersionTarget{}, err
	}
	publishCodexClientVersionTarget(saved)
	return saved, nil
}

func publishCodexClientVersionTarget(target CodexClientVersionTarget) {
	target.Pairs = append([]CodexClientVersionPair(nil), target.Pairs...)
	next := &codexClientVersionSnapshot{targets: make(map[string]CodexClientVersionTarget)}
	if current := codexClientVersions.Load(); current != nil {
		for key, value := range current.targets {
			next.targets[key] = value
		}
	}
	next.targets[codexClientVersionKey(target.ClientKind, target.TargetPlatform)] = target
	codexClientVersions.Store(next)
	refreshCodexClientVersionProjections()
}

func refreshCodexClientVersionProjections() {
	UpdateRuntimeSettings(codexRuntimeClientVersionProjections)
}

func codexRuntimeClientVersionProjections(settings RuntimeSettings) RuntimeSettings {
	snapshot := codexClientVersions.Load()
	build := func(kind, platform string) string {
		if snapshot == nil {
			return ""
		}
		target := snapshot.targets[codexClientVersionKey(kind, platform)]
		if len(target.Pairs) == 0 {
			return ""
		}
		return target.Pairs[0].AppVersion
	}
	settings.CodexSyncedDesktopMacBuild = build(string(CodexClientKindDesktop), "darwin-arm64")
	settings.CodexSyncedDesktopWindowsBuild = build(string(CodexClientKindDesktop), "win32-x64")
	settings.CodexSyncedVSCodeBuild = build(string(CodexClientKindVSCode), "linux-x64")
	return settings
}

func newCodexClientVersionTarget(kind, platform string) CodexClientVersionTarget {
	return CodexClientVersionTarget{ClientKind: kind, TargetPlatform: platform, CheckedAt: time.Now().UnixMilli(), Status: "error"}
}

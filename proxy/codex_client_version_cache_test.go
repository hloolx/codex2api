package proxy

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/codex2api/database"
)

func codexTestVersionDB(t *testing.T) *database.DB {
	t.Helper()
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "versions.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	old := codexClientVersions.Load()
	oldRuntime := CurrentRuntimeSettings()
	t.Cleanup(func() { codexClientVersions.Store(old); ApplyRuntimeSettings(oldRuntime) })
	if err := LoadCodexClientVersionCache(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	return db
}

func codexTestPair(build string) CodexClientVersionPair {
	return CodexClientVersionPair{AppVersion: build, CLIVersion: "0.158.0-alpha.2.1", Source: "official_appcast", ArtifactID: build, VerifiedAt: 1}
}

func TestCodexClientVersionCacheRestartAndFailure(t *testing.T) {
	db := codexTestVersionDB(t)
	ctx := context.Background()
	target := newCodexClientVersionTarget("codex-desktop", "darwin-arm64")
	target.Pairs, target.Status = []CodexClientVersionPair{codexTestPair("26.924.22138")}, "verified"
	if _, err := saveCodexClientVersionTarget(ctx, db, target); err != nil {
		t.Fatal(err)
	}
	before, err := db.GetCodexClientVersionCache(ctx)
	if err != nil {
		t.Fatal(err)
	}
	failure := persistCodexCandidateResult(ctx, db, codexCandidateResolution{
		candidate: codexClientCandidate{Kind: target.ClientKind, Target: target.TargetPlatform},
		err:       errors.New("unsupported package layout"),
	})
	if failure.target.Status != "stale" || failure.target.Error == "" || failure.updated {
		t.Fatalf("failure result: %+v", failure)
	}
	after, err := db.GetCodexClientVersionCache(ctx)
	if err != nil || len(after) != 1 || before[0].Payload != after[0].Payload {
		t.Fatalf("failed sync changed database: %+v, %v", after, err)
	}
	codexClientVersions.Store(nil)
	if err := LoadCodexClientVersionCache(ctx, db); err != nil {
		t.Fatal(err)
	}
	view := CurrentCodexClientVersions()
	if len(view) != 1 || len(view[0].Pairs) != 1 || view[0].Pairs[0].CLIVersion != "0.158.0-alpha.2.1" || view[0].Error != "" {
		t.Fatalf("cache lost complete pair: %+v", view)
	}
	view[0].Pairs[0].CLIVersion = "changed"
	if CurrentCodexClientVersions()[0].Pairs[0].CLIVersion == "changed" {
		t.Fatal("snapshot mutable")
	}
}

func TestCodexClientVersionCacheConcurrentReplacementAndFailedSave(t *testing.T) {
	db := codexTestVersionDB(t)
	var wg sync.WaitGroup
	const updates = 25
	for i := range updates {
		wg.Go(func() {
			target := newCodexClientVersionTarget("codex-vscode", "linux-x64")
			target.CheckedAt = int64(i + 1)
			target.Pairs = []CodexClientVersionPair{codexTestPair(fmt.Sprintf("26.928.%d", i))}
			if _, err := saveCodexClientVersionTarget(context.Background(), db, target); err != nil {
				t.Error(err)
			}
			_ = CurrentCodexClientVersions()
		})
	}
	wg.Wait()
	view := CurrentCodexClientVersions()
	if len(view[0].Pairs) != 1 || view[0].Pairs[0].AppVersion != "26.928.24" {
		t.Fatalf("current pair: %+v", view)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	target := newCodexClientVersionTarget("codex-vscode", "linux-x64")
	target.Pairs = []CodexClientVersionPair{codexTestPair("26.929.1")}
	if _, err := saveCodexClientVersionTarget(ctx, db, target); err == nil {
		t.Fatal("expected canceled save")
	}
	if CurrentCodexClientVersions()[0].Pairs[0].AppVersion != "26.928.24" {
		t.Fatal("failed save published")
	}
}

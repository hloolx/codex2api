package proxy

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func codexTestArchive(t *testing.T, files map[string]string) *zip.Reader {
	t.Helper()
	data := codexTestZIP(t, files)
	archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	return archive
}

func codexTestCLIManifest(triple, version string) string {
	entry := "bin/codex"
	if len(triple) > 0 && codexTargetTripleMatches(triple, "win32-x64") {
		entry += ".exe"
	}
	if codexTargetTripleMatches(triple, "win32-arm64") {
		entry = "bin/codex.exe"
	}
	data, _ := json.Marshal(codexCLIManifest{LayoutVersion: 1, Version: version, Target: triple, Variant: "codex", Entrypoint: entry})
	return string(data)
}

func TestCodexMacPairPreservesAlphaAndValidatesBuild(t *testing.T) {
	files := map[string]string{
		"ChatGPT.app/Contents/Info.plist":                             `<plist><dict><key>CFBundleIdentifier</key><string>com.openai.codex</string><key>CFBundleShortVersionString</key><string>26.924.22138</string><key>CFBundleVersion</key><string>12345</string></dict></plist>`,
		"ChatGPT.app/Contents/Resources/codex-cli/codex-package.json": codexTestCLIManifest("aarch64-apple-darwin", "0.158.0-alpha.2.1"),
	}
	candidate := codexClientCandidate{Target: "darwin-arm64", NativeBuild: "12345", Pair: CodexClientVersionPair{AppVersion: "26.924.22138"}}
	pair, err := resolveCodexMacPair(codexTestArchive(t, files), candidate)
	if err != nil || pair.CLIVersion != "0.158.0-alpha.2.1" {
		t.Fatalf("alpha pair: %+v, %v", pair, err)
	}
	candidate.NativeBuild = "different"
	if _, err := resolveCodexMacPair(codexTestArchive(t, files), candidate); err == nil {
		t.Fatal("mismatched native build accepted")
	}
	candidate.NativeBuild, candidate.Target = "12345", "darwin-x64"
	if _, err := resolveCodexMacPair(codexTestArchive(t, files), candidate); err == nil {
		t.Fatal("mismatched architecture accepted")
	}
}

func TestCodexVSIXSelectsNativeCLIInsteadOfWSL(t *testing.T) {
	files := map[string]string{
		"extension/package.json":                          `{"name":"chatgpt","publisher":"openai","version":"26.928.31416"}`,
		"extension/bin/linux-x86_64/codex-package.json":   codexTestCLIManifest("x86_64-unknown-linux-musl", "0.159.1"),
		"extension/bin/windows-x86_64/codex-package.json": codexTestCLIManifest("x86_64-pc-windows-msvc", "0.159.2-alpha.1"),
	}
	candidate := codexClientCandidate{Target: "win32-x64", Pair: CodexClientVersionPair{AppVersion: "26.928.31416"}}
	pair, err := resolveCodexVSIXPair(codexTestArchive(t, files), candidate)
	if err != nil || pair.CLIVersion != "0.159.2-alpha.1" {
		t.Fatalf("native pair: %+v, %v", pair, err)
	}
	candidate.Target = "win32-arm64"
	if _, err := resolveCodexVSIXPair(codexTestArchive(t, files), candidate); err == nil {
		t.Fatal("missing architecture accepted")
	}
}

func TestCodexMarketplaceSkipsExtensionPrerelease(t *testing.T) {
	versions := []codexMarketplaceVersion{{Version: "26.928.31416", TargetPlatform: "linux-x64"}, {Version: "26.5928.31416", TargetPlatform: "linux-x64", Properties: []codexMarketplaceProperty{{Key: "Microsoft.VisualStudio.Code.PreRelease", Value: "true"}}}}
	for i := range versions {
		versions[i].Files = append(versions[i].Files, struct {
			AssetType string `json:"assetType"`
			Source    string `json:"source"`
		}{AssetType: "Microsoft.VisualStudio.Services.VSIXPackage", Source: "https://openai.gallerycdn.vsassets.io/package.vsix"})
	}
	candidate := codexVSCodeCandidate(versions, "linux-x64", nil)
	if candidate.Err != nil || candidate.Pair.AppVersion != "26.928.31416" {
		t.Fatalf("stable candidate: %+v", candidate)
	}
}

func codexTestWindowsMapping() codexWindowsMapping {
	var mapping codexWindowsMapping
	mapping.SchemaVersion = 5
	mapping.Sources.Windows.UpdateManifest = codexWindowsUpdate{PackageIdentity: "OpenAI.Codex", BuildVersion: "26.928.3736.0"}
	mapping.Sources.Windows.Architectures = map[string]codexWindowsMappingArch{"x64": {Architecture: "x64", Version: "26.928.3736.0", AppVersion: "26.928.31416", BackendVersion: "0.159.2-alpha.1", Status: "downloadable", CurrentForCodexVersion: true}}
	return mapping
}

func TestCodexWindowsMappingValidationAndFallback(t *testing.T) {
	mapping := codexTestWindowsMapping()
	candidate := codexWindowsCandidate(mapping.Sources.Windows.UpdateManifest, "x64")
	got := applyCodexWindowsMapping(candidate, mapping, nil)
	if got.Pair.CLIVersion != "0.159.2-alpha.1" || got.Pair.Source != "third_party_windows_mapping" {
		t.Fatalf("mapping: %+v", got)
	}
	for _, kind := range []string{"stale", "arch", "missing", "version", "status"} {
		t.Run(kind, func(t *testing.T) {
			mapping := codexTestWindowsMapping()
			entry := mapping.Sources.Windows.Architectures["x64"]
			switch kind {
			case "stale":
				mapping.Sources.Windows.UpdateManifest.BuildVersion = "26.927.1.0"
			case "arch":
				entry.Architecture = "arm64"
			case "missing":
				entry.CurrentForCodexVersion = false
			case "version":
				entry.BackendVersion = "bad"
			case "status":
				entry.Status = "unavailable"
			}
			mapping.Sources.Windows.Architectures["x64"] = entry
			got := applyCodexWindowsMapping(candidate, mapping, nil)
			if got.Pair.CLIVersion != "" || got.Pair.Source != "official_msix" || got.FallbackReason == "" {
				t.Fatalf("fallback: %+v", got)
			}
		})
	}
}

func codexTestASAR(appVersion string) string {
	pkg := `{"version":"` + appVersion + `"}`
	header := fmt.Sprintf(`{"files":{"package.json":{"offset":"0","size":%d}}}`, len(pkg))
	prefix := make([]byte, 16)
	binary.LittleEndian.PutUint32(prefix[4:], uint32(8+len(header)))
	binary.LittleEndian.PutUint32(prefix[12:], uint32(len(header)))
	return string(prefix) + header + pkg
}

func TestCodexOfficialMSIXFallbackRequiresCompletePair(t *testing.T) {
	files := map[string]string{
		"AppxManifest.xml":       `<Package><Identity Name="OpenAI.Codex" Version="26.928.3736.0" ProcessorArchitecture="x64"/></Package>`,
		"app/resources/app.asar": codexTestASAR("26.928.31416"),
	}
	candidate := codexWindowsCandidate(codexWindowsUpdate{BuildVersion: "26.928.3736.0"}, "x64")
	if _, err := resolveCodexMSIXPair(&codexRemoteArchive{archive: codexTestArchive(t, files)}, candidate); err == nil {
		t.Fatal("legacy MSIX without CLI manifest accepted")
	}
	files["app/resources/codex-cli/codex-package.json"] = codexTestCLIManifest("x86_64-pc-windows-msvc", "0.159.2")
	pair, err := resolveCodexMSIXPair(&codexRemoteArchive{archive: codexTestArchive(t, files)}, candidate)
	if err != nil || pair.AppVersion != "26.928.31416" || pair.CLIVersion != "0.159.2" {
		t.Fatalf("official pair: %+v, %v", pair, err)
	}
}

func TestCodexClientVersionSyncCoalescesConcurrentCalls(t *testing.T) {
	db := codexTestVersionDB(t)
	var cliCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/cli" {
			cliCalls.Add(1)
			time.Sleep(100 * time.Millisecond)
			_, _ = w.Write([]byte(`{"name":"0.160.0","tag_name":"rust-v0.160.0"}`))
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	oldSources, oldCLI, oldRuntime := codexClientSources, codexReleasesLatestURLForTest, CurrentRuntimeSettings()
	t.Cleanup(func() {
		codexClientSources, codexReleasesLatestURLForTest = oldSources, oldCLI
		ApplyRuntimeSettings(oldRuntime)
	})
	codexClientSources = codexClientSourceURLs{MacARM64: server.URL, MacX64: server.URL, WindowsUpdate: server.URL, WindowsMapping: server.URL, Marketplace: server.URL}
	codexReleasesLatestURLForTest = server.URL + "/cli"
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			result, err := SyncCodexClientVersions(context.Background(), db, "")
			if err != nil || result.DesktopMac.Error == "" || len(result.VSCode.Targets) != 6 {
				t.Errorf("sync result: %+v, %v", result, err)
			}
		})
	}
	wg.Wait()
	if cliCalls.Load() != 1 {
		t.Fatalf("expected one coalesced sync, got %d", cliCalls.Load())
	}
}

func TestCodexClientCandidateReusesVerifiedArtifactWithoutDownload(t *testing.T) {
	db := codexTestVersionDB(t)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(http.StatusOK) }))
	defer server.Close()
	previous := newCodexClientVersionTarget("codex-desktop", "win32-x64")
	pair := codexTestPair("26.928.31416")
	pair.Source, pair.ArtifactURL = "official_msix_blockmap", server.URL
	previous.Pairs = []CodexClientVersionPair{pair}
	if _, err := saveCodexClientVersionTarget(context.Background(), db, previous); err != nil {
		t.Fatal(err)
	}
	candidate := codexClientCandidate{Kind: previous.ClientKind, Target: previous.TargetPlatform, Pair: CodexClientVersionPair{ArtifactID: pair.ArtifactID, ArtifactURL: server.URL}}
	resolved, err := resolveCodexClientCandidate(context.Background(), server.Client(), candidate)
	if err != nil || resolved != pair || requests.Load() != 0 {
		t.Fatalf("cached artifact: %+v / %v / %d requests", resolved, err, requests.Load())
	}
}

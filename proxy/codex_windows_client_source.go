package proxy

import (
	"archive/zip"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

type codexWindowsUpdate struct {
	PackageIdentity string `json:"packageIdentity"`
	BuildVersion    string `json:"buildVersion"`
}

type codexWindowsMappingArch struct {
	Architecture           string `json:"architecture"`
	Version                string `json:"version"`
	AppVersion             string `json:"appVersion"`
	BackendVersion         string `json:"backendVersion"`
	Status                 string `json:"status"`
	CurrentForCodexVersion bool   `json:"currentForCodexVersion"`
	ETag                   string `json:"etag"`
}

type codexWindowsMapping struct {
	SchemaVersion int `json:"schemaVersion"`
	Sources       struct {
		Windows struct {
			UpdateManifest codexWindowsUpdate                 `json:"updateManifest"`
			Architectures  map[string]codexWindowsMappingArch `json:"architectures"`
		} `json:"windows"`
	} `json:"sources"`
}

func fetchCodexWindowsCandidates(ctx context.Context, client *http.Client) []codexClientCandidate {
	var update codexWindowsUpdate
	data, updateErr := fetchCodexSmallJSON(ctx, client, codexClientSources.WindowsUpdate)
	if updateErr == nil {
		updateErr = json.Unmarshal(data, &update)
	}
	if _, ok := codexBuildParts(update.BuildVersion, 4); updateErr == nil && (!ok || update.PackageIdentity != "OpenAI.Codex") {
		updateErr = fmt.Errorf("invalid official Windows update manifest")
	}
	var mapping codexWindowsMapping
	var mappingErr error
	if updateErr == nil {
		data, mappingErr = fetchCodexSmallJSON(ctx, client, codexClientSources.WindowsMapping)
		if mappingErr == nil {
			mappingErr = json.Unmarshal(data, &mapping)
		}
	}
	result := make([]codexClientCandidate, 0, 2)
	for _, arch := range []string{"x64", "arm64"} {
		candidate := codexWindowsCandidate(update, arch)
		candidate.Err = updateErr
		if updateErr == nil {
			candidate = applyCodexWindowsMapping(candidate, mapping, mappingErr)
		}
		result = append(result, candidate)
	}
	return result
}

func codexWindowsCandidate(update codexWindowsUpdate, arch string) codexClientCandidate {
	url := "https://persistent.oaistatic.com/codex-app-prod/releases/" + update.BuildVersion + "/ChatGPT-" + arch + ".msix"
	return codexClientCandidate{Kind: string(CodexClientKindDesktop), Target: "win32-" + arch,
		Pair: CodexClientVersionPair{PackageVersion: update.BuildVersion, Source: "official_msix", ArtifactURL: url, ArtifactID: codexArtifactIdentity(url, update.BuildVersion)}}
}

func applyCodexWindowsMapping(candidate codexClientCandidate, mapping codexWindowsMapping, fetchErr error) codexClientCandidate {
	if fetchErr != nil {
		candidate.FallbackReason = fetchErr.Error()
		return candidate
	}
	arch := strings.TrimPrefix(candidate.Target, "win32-")
	entry, ok := mapping.Sources.Windows.Architectures[arch]
	if err := validateCodexWindowsMapping(mapping, entry, candidate); !ok || err != nil {
		candidate.FallbackReason = "missing, stale or invalid architecture mapping"
		return candidate
	}
	candidate.Pair.AppVersion, candidate.Pair.CLIVersion = entry.AppVersion, entry.BackendVersion
	candidate.Pair.Source = "third_party_windows_mapping"
	candidate.Pair.ArtifactID = codexArtifactIdentity(candidate.Target, entry.Version, entry.AppVersion, entry.BackendVersion, entry.ETag)
	return candidate
}

func validateCodexWindowsMapping(mapping codexWindowsMapping, entry codexWindowsMappingArch, candidate codexClientCandidate) error {
	update := mapping.Sources.Windows.UpdateManifest
	if mapping.SchemaVersion <= 0 || update.PackageIdentity != "OpenAI.Codex" || update.BuildVersion != candidate.Pair.PackageVersion {
		return fmt.Errorf("stale Windows mapping")
	}
	if err := validateCodexWindowsMappingArch(entry, candidate); err != nil {
		return err
	}
	return validateCodexWindowsMappingVersions(entry)
}

func validateCodexWindowsMappingArch(entry codexWindowsMappingArch, candidate codexClientCandidate) error {
	if entry.Architecture != strings.TrimPrefix(candidate.Target, "win32-") || entry.Version != candidate.Pair.PackageVersion || entry.Status != "downloadable" || !entry.CurrentForCodexVersion {
		return fmt.Errorf("invalid Windows architecture mapping")
	}
	return nil
}

func validateCodexWindowsMappingVersions(entry codexWindowsMappingArch) error {
	app, validApp := codexBuildParts(entry.AppVersion, 3)
	pkg, validPackage := codexBuildParts(entry.Version, 4)
	if !validApp || !validPackage || app[0] != pkg[0] || app[1] != pkg[1] || !validCodexClientVersionString(entry.BackendVersion) {
		return fmt.Errorf("invalid Windows application/backend versions")
	}
	return nil
}

func validateCodexMSIXIdentity(archive *zip.Reader, candidate codexClientCandidate) (string, error) {
	data, err := codexArchiveFile(archive, "AppxManifest.xml")
	if err != nil {
		return "", err
	}
	var manifest struct {
		Identity struct {
			Name    string `xml:"Name,attr"`
			Version string `xml:"Version,attr"`
			Arch    string `xml:"ProcessorArchitecture,attr"`
		} `xml:"Identity"`
	}
	if err := xml.Unmarshal(data, &manifest); err != nil {
		return "", err
	}
	identity := manifest.Identity
	if identity.Name != "OpenAI.Codex" || identity.Arch != strings.TrimPrefix(candidate.Target, "win32-") {
		return "", fmt.Errorf("MSIX package identity/architecture mismatch")
	}
	if !codexMSIXPackageMatchesCandidate(identity.Version, candidate) {
		return "", fmt.Errorf("MSIX package version mismatch")
	}
	return identity.Version, nil
}

func codexMSIXPackageMatchesCandidate(version string, candidate codexClientCandidate) bool {
	actual, validActual := codexBuildParts(version, 4)
	store, validStore := codexBuildParts(candidate.Pair.PackageVersion, 4)
	if !validActual || !validStore {
		return false
	}
	if candidate.LatestMSIX {
		return codexWindowsBuildMatchesStore(actual, store, true)
	}
	return version == candidate.Pair.PackageVersion
}

// 版本化包缺失时保留上游的最新官方包回退，并用 ETag 区分可变地址。
func resolveCodexWindowsArchive(ctx context.Context, client *http.Client, candidate codexClientCandidate) (CodexClientVersionPair, error) {
	ranges, err := codexWindowsCandidateRanges(ctx, client, &candidate)
	if err != nil {
		return candidate.Pair, err
	}
	if candidate.LatestMSIX && ranges.etag != "" {
		if pair, ok := cachedCodexCandidatePair(candidate); ok {
			if !codexMSIXPackageMatchesCandidate(pair.PackageVersion, candidate) {
				return candidate.Pair, fmt.Errorf("cached MSIX package version mismatch")
			}
			return pair, nil
		}
	}
	archive, err := zip.NewReader(ranges, ranges.size)
	if err != nil {
		return candidate.Pair, err
	}
	return resolveCodexMSIXPair(&codexRemoteArchive{archive: archive, ranges: ranges}, candidate)
}

func codexWindowsCandidateRanges(ctx context.Context, client *http.Client, candidate *codexClientCandidate) (*codexRangeReader, error) {
	ranges, err := newCodexRangeReader(ctx, client, candidate.Pair.ArtifactURL)
	if !errors.Is(err, errCodexMSIXNotFound) {
		return ranges, err
	}
	candidate.LatestMSIX = true
	candidate.Pair.ArtifactURL = codexMSIXBaseURL + "ChatGPT-" + strings.TrimPrefix(candidate.Target, "win32-") + ".msix"
	ranges, err = newCodexRangeReader(ctx, client, candidate.Pair.ArtifactURL)
	if err != nil {
		return nil, err
	}
	candidate.Pair.ArtifactID = codexArtifactIdentity(candidate.Pair.ArtifactURL,
		ranges.etag, ranges.modified, strconv.FormatInt(ranges.size, 10))
	return ranges, nil
}

func codexMSIXAppVersion(archive *zip.Reader) (string, error) {
	for _, file := range archive.File {
		if file.Name != "app/resources/app.asar" {
			continue
		}
		stream, err := file.Open()
		if err != nil {
			return "", err
		}
		defer stream.Close()
		return codexASARPackageVersion(stream)
	}
	return "", fmt.Errorf("Desktop app.asar missing in MSIX")
}

func resolveCodexMSIXPair(remote *codexRemoteArchive, candidate codexClientCandidate) (CodexClientVersionPair, error) {
	pair := candidate.Pair
	packageVersion, err := validateCodexMSIXIdentity(remote.archive, candidate)
	if err != nil {
		return pair, err
	}
	pair.PackageVersion = packageVersion
	app, cli, err := codexMSIXVersions(remote, candidate.Target)
	if err != nil {
		return pair, err
	}
	appParts, validApp := codexBuildParts(app, 3)
	pkgParts, validPackage := codexBuildParts(pair.PackageVersion, 4)
	if !validApp || !validPackage || appParts[0] != pkgParts[0] || appParts[1] != pkgParts[1] {
		return pair, fmt.Errorf("MSIX application version mismatch")
	}
	pair.AppVersion, pair.CLIVersion = app, cli
	if !codexMSIXHasCLIManifest(remote.archive) {
		pair.Source = "official_msix_blockmap"
	}
	return pair, nil
}

func codexMSIXHasCLIManifest(archive *zip.Reader) bool {
	for _, file := range archive.File {
		if codexCLIManifestFileName(file.Name, "app/") {
			return true
		}
	}
	return false
}

func codexMSIXVersions(remote *codexRemoteArchive, target string) (string, string, error) {
	if codexMSIXHasCLIManifest(remote.archive) {
		cli, err := codexArchiveCLI(remote.archive, target, "app/")
		if err != nil {
			return "", "", err
		}
		app, err := codexMSIXAppVersion(remote.archive)
		return app, cli, err
	}
	blocks, err := newCodexMSIXBlockMap(remote)
	if err != nil {
		return "", "", err
	}
	app, err := codexMSIXBlockAppVersion(blocks)
	if err != nil {
		return "", "", err
	}
	cli, err := codexMSIXEmbeddedCLI(blocks, target)
	return app, cli, err
}

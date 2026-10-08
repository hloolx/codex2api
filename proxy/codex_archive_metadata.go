package proxy

import (
	"archive/zip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

type codexCLIManifest struct {
	LayoutVersion int    `json:"layoutVersion"`
	Version       string `json:"version"`
	Target        string `json:"target"`
	Variant       string `json:"variant"`
	Entrypoint    string `json:"entrypoint"`
}

func codexTargetTripleMatches(triple, target string) bool {
	triples := map[string][]string{
		"darwin-arm64": {"aarch64-apple-darwin"},
		"darwin-x64":   {"x86_64-apple-darwin"},
		"win32-arm64":  {"aarch64-pc-windows-msvc"},
		"win32-x64":    {"x86_64-pc-windows-msvc"},
		"linux-arm64":  {"aarch64-unknown-linux-musl", "aarch64-unknown-linux-gnu"},
		"linux-x64":    {"x86_64-unknown-linux-musl", "x86_64-unknown-linux-gnu"},
	}
	for _, value := range triples[target] {
		if triple == value {
			return true
		}
	}
	return false
}

func openCodexVersionArchive(ctx context.Context, client *http.Client, url string) (*zip.Reader, error) {
	remote, err := openCodexRemoteArchive(ctx, client, url)
	if err != nil {
		return nil, err
	}
	return remote.archive, nil
}

type codexRemoteArchive struct {
	archive *zip.Reader
	ranges  *codexRangeReader
}

func openCodexRemoteArchive(ctx context.Context, client *http.Client, url string) (*codexRemoteArchive, error) {
	ranges, err := newCodexRangeReader(ctx, client, url)
	if err != nil {
		return nil, err
	}
	archive, err := zip.NewReader(ranges, ranges.size)
	if err != nil {
		return nil, err
	}
	return &codexRemoteArchive{archive: archive, ranges: ranges}, nil
}

func codexArchiveFile(archive *zip.Reader, name string) ([]byte, error) {
	return codexArchiveFileLimited(archive, name, codexPackageJSONMax)
}

func codexArchiveFileLimited(archive *zip.Reader, name string, limit int64) ([]byte, error) {
	var found *zip.File
	for _, file := range archive.File {
		if file.Name != name {
			continue
		}
		if found != nil {
			return nil, fmt.Errorf("duplicate archive metadata %s", name)
		}
		found = file
	}
	if found == nil || found.UncompressedSize64 > uint64(limit) {
		return nil, fmt.Errorf("missing or oversized archive metadata %s", name)
	}
	stream, err := found.Open()
	if err != nil {
		return nil, err
	}
	defer stream.Close()
	data, err := io.ReadAll(io.LimitReader(stream, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("archive metadata exceeds limit")
	}
	return data, nil
}

func codexArchiveCLI(archive *zip.Reader, target, prefix string) (string, error) {
	version := ""
	for _, file := range archive.File {
		if !codexCLIManifestFileName(file.Name, prefix) {
			continue
		}
		data, err := codexArchiveFile(archive, file.Name)
		if err != nil {
			return "", err
		}
		var manifest codexCLIManifest
		if err := json.Unmarshal(data, &manifest); err != nil {
			return "", err
		}
		if !codexTargetTripleMatches(manifest.Target, target) {
			continue
		}
		if err := validateCodexCLIManifest(manifest); err != nil {
			return "", err
		}
		if version != "" && version != manifest.Version {
			return "", fmt.Errorf("conflicting CLI manifests for %s", target)
		}
		version = manifest.Version
	}
	if version == "" {
		return "", fmt.Errorf("unsupported package layout: no CLI manifest for %s", target)
	}
	return version, nil
}

func codexCLIManifestFileName(name, prefix string) bool {
	return strings.HasPrefix(name, prefix) && strings.HasSuffix(name, "/codex-package.json")
}

func validateCodexCLIManifest(manifest codexCLIManifest) error {
	if manifest.LayoutVersion != 1 || manifest.Variant != "codex" || !validCodexClientVersionString(manifest.Version) {
		return fmt.Errorf("invalid embedded CLI manifest")
	}
	entrypoint := "bin/codex"
	if strings.Contains(manifest.Target, "windows") {
		entrypoint += ".exe"
	}
	if manifest.Entrypoint != entrypoint {
		return fmt.Errorf("invalid CLI entrypoint")
	}
	return nil
}

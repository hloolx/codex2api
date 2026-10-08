package proxy

import (
	"archive/zip"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// 65554 = 最新稳定版和预览版 + 文件 + 属性；避免拉取整个历史版本清单。
const codexLatestMarketplaceQuery = `{"filters":[{"criteria":[{"filterType":7,"value":"openai.chatgpt"}]}],"flags":65554}`

var codexVSCodeTargets = []string{"darwin-arm64", "darwin-x64", "win32-arm64", "win32-x64", "linux-arm64", "linux-x64"}

type codexMarketplaceVersion struct {
	Version        string                     `json:"version"`
	TargetPlatform string                     `json:"targetPlatform"`
	Properties     []codexMarketplaceProperty `json:"properties"`
	Files          []struct {
		AssetType string `json:"assetType"`
		Source    string `json:"source"`
	} `json:"files"`
}

func codexMarketplaceVersions(data []byte) ([]codexMarketplaceVersion, error) {
	var payload struct {
		Results []struct {
			Extensions []struct {
				Publisher struct {
					PublisherName string `json:"publisherName"`
				} `json:"publisher"`
				ExtensionName string                    `json:"extensionName"`
				Versions      []codexMarketplaceVersion `json:"versions"`
			} `json:"extensions"`
		} `json:"results"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, err
	}
	var versions []codexMarketplaceVersion
	for _, result := range payload.Results {
		for _, extension := range result.Extensions {
			if extension.Publisher.PublisherName == "openai" && extension.ExtensionName == "chatgpt" {
				versions = append(versions, extension.Versions...)
			}
		}
	}
	return versions, nil
}

func fetchCodexVSCodeCandidates(ctx context.Context, client *http.Client) []codexClientCandidate {
	resp, err := codexBuildRequest(ctx, client, codexBuildRequestSpec{Method: http.MethodPost, URL: codexClientSources.Marketplace, Body: strings.NewReader(codexLatestMarketplaceQuery)})
	var versions []codexMarketplaceVersion
	if err == nil {
		var data []byte
		data, err = codexReadSmallResponse(resp, codexClientMetadataMax)
		if err == nil {
			versions, err = codexMarketplaceVersions(data)
		}
	}
	candidates := make([]codexClientCandidate, 0, len(codexVSCodeTargets))
	for _, target := range codexVSCodeTargets {
		candidates = append(candidates, codexVSCodeCandidate(versions, target, err))
	}
	return candidates
}

func codexVSCodeCandidate(versions []codexMarketplaceVersion, target string, fetchErr error) codexClientCandidate {
	candidate := codexClientCandidate{Kind: string(CodexClientKindVSCode), Target: target, Err: fetchErr}
	if fetchErr != nil {
		return candidate
	}
	for _, version := range versions {
		if version.TargetPlatform != target || codexMarketplacePrerelease(version.Properties) || !codexBuildIsNewer(version.Version, candidate.Pair.AppVersion) {
			continue
		}
		for _, file := range version.Files {
			if file.AssetType == "Microsoft.VisualStudio.Services.VSIXPackage" && codexTrustedArtifactURL(file.Source) {
				candidate.Pair = CodexClientVersionPair{AppVersion: version.Version, Source: "official_marketplace", ArtifactURL: file.Source, ArtifactID: codexArtifactIdentity(target, version.Version, file.Source)}
			}
		}
	}
	if candidate.Pair.AppVersion == "" {
		candidate.Err = fmt.Errorf("no stable native VSIX for %s", target)
	}
	return candidate
}

func resolveCodexVSIXPair(archive *zip.Reader, candidate codexClientCandidate) (CodexClientVersionPair, error) {
	pair := candidate.Pair
	data, err := codexArchiveFile(archive, "extension/package.json")
	if err != nil {
		return pair, err
	}
	var extension struct {
		Name      string `json:"name"`
		Publisher string `json:"publisher"`
		Version   string `json:"version"`
	}
	if err := json.Unmarshal(data, &extension); err != nil {
		return pair, err
	}
	if extension.Name != "chatgpt" || extension.Publisher != "openai" || extension.Version != pair.AppVersion {
		return pair, fmt.Errorf("VSIX extension identity/version mismatch")
	}
	nativeDirs := map[string]string{
		"darwin-arm64": "macos-aarch64", "darwin-x64": "macos-x86_64",
		"win32-arm64": "windows-aarch64", "win32-x64": "windows-x86_64",
		"linux-arm64": "linux-aarch64", "linux-x64": "linux-x86_64",
	}
	pair.CLIVersion, err = codexArchiveCLI(archive, candidate.Target, "extension/bin/"+nativeDirs[candidate.Target]+"/")
	return pair, err
}

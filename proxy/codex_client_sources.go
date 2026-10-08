package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	codexMacX64AppcastURL  = "https://persistent.oaistatic.com/codex-app-prod/appcast-x64.xml"
	codexWindowsMappingURL = "https://codexapp.agentsmirror.com/latest/manifest"
	codexClientMetadataMax = 1 << 20
)

type codexClientSourceURLs struct {
	MacARM64       string
	MacX64         string
	WindowsUpdate  string
	WindowsMapping string
	Marketplace    string
}

var codexClientSources = codexClientSourceURLs{
	MacARM64: codexMacAppcastURL, MacX64: codexMacX64AppcastURL,
	WindowsUpdate: codexWindowsUpdateURL, WindowsMapping: codexWindowsMappingURL,
	Marketplace: codexMarketplaceURL,
}

type codexClientCandidate struct {
	Kind           string
	Target         string
	Pair           CodexClientVersionPair
	NativeBuild    string
	Err            error
	FallbackReason string
	LatestMSIX     bool
}

func codexArtifactIdentity(parts ...string) string {
	hash := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(hash[:])
}

func codexTrustedArtifactURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Fragment != "" {
		return false
	}
	return u.Hostname() == "persistent.oaistatic.com" || u.Hostname() == "openai.gallerycdn.vsassets.io"
}

func fetchCodexSmallJSON(ctx context.Context, client *http.Client, endpoint string) ([]byte, error) {
	resp, err := codexBuildRequest(ctx, client, codexBuildRequestSpec{Method: http.MethodGet, URL: endpoint})
	if err != nil {
		return nil, err
	}
	return codexReadSmallResponse(resp, codexClientMetadataMax)
}

func codexVersionSourceError(err error) error {
	if requestError, ok := err.(*url.Error); ok {
		u, parseErr := url.Parse(requestError.URL)
		if parseErr == nil {
			return fmt.Errorf("%s %s%s: %w", requestError.Op, u.Hostname(), u.Path, requestError.Err)
		}
	}
	return err
}

func resolveCodexClientCandidate(ctx context.Context, client *http.Client, candidate codexClientCandidate) (CodexClientVersionPair, error) {
	if candidate.Err != nil {
		return CodexClientVersionPair{}, candidate.Err
	}
	if pair, ok := cachedCodexCandidatePair(candidate); ok {
		return pair, nil
	}
	pair := candidate.Pair
	if pair.CLIVersion == "" {
		var err error
		pair, err = resolveCodexCandidateArchive(ctx, client, candidate)
		if err != nil {
			return pair, fmt.Errorf("%s%s", codexFallbackPrefix(candidate), err)
		}
	}
	if !validCodexClientPair(pair) {
		return pair, fmt.Errorf("incomplete Codex version pair")
	}
	pair.VerifiedAt = time.Now().UnixMilli()
	return pair, nil
}

func cachedCodexCandidatePair(candidate codexClientCandidate) (CodexClientVersionPair, bool) {
	for _, pair := range codexClientVersionTarget(candidate.Kind, candidate.Target).Pairs {
		if pair.ArtifactID == candidate.Pair.ArtifactID {
			return pair, true
		}
	}
	return CodexClientVersionPair{}, false
}

func resolveCodexCandidateArchive(ctx context.Context, client *http.Client, candidate codexClientCandidate) (CodexClientVersionPair, error) {
	if candidate.Kind == string(CodexClientKindDesktop) && strings.HasPrefix(candidate.Target, "win32-") {
		return resolveCodexWindowsArchive(ctx, client, candidate)
	}
	remote, err := openCodexRemoteArchive(ctx, client, candidate.Pair.ArtifactURL)
	if err != nil {
		return candidate.Pair, err
	}
	if candidate.Kind == string(CodexClientKindVSCode) {
		return resolveCodexVSIXPair(remote.archive, candidate)
	}
	return resolveCodexMacPair(remote.archive, candidate)
}

func codexFallbackPrefix(candidate codexClientCandidate) string {
	if candidate.FallbackReason == "" {
		return ""
	}
	return "Windows mapping unavailable (" + candidate.FallbackReason + "); official fallback: "
}

package proxy

import (
	"context"
	"net/http"
	"sync"
)

type codexFetchOutcome struct {
	version string
	err     error
}

type codexClientMetadataInput struct {
	client   *http.Client
	proxyURL string
}

type codexClientMetadataResult struct {
	candidates []codexClientCandidate
	cli        codexFetchOutcome
}

// 保持上游各来源并发拉取的语义；网络完成后再持久化。
func fetchCodexClientMetadata(ctx context.Context, input codexClientMetadataInput) codexClientMetadataResult {
	fetchers := []func() []codexClientCandidate{
		func() []codexClientCandidate {
			return []codexClientCandidate{fetchCodexMacCandidate(ctx, input.client, "darwin-arm64")}
		},
		func() []codexClientCandidate {
			return []codexClientCandidate{fetchCodexMacCandidate(ctx, input.client, "darwin-x64")}
		},
		func() []codexClientCandidate { return fetchCodexWindowsCandidates(ctx, input.client) },
		func() []codexClientCandidate { return fetchCodexVSCodeCandidates(ctx, input.client) },
	}
	outcomes := make([][]codexClientCandidate, len(fetchers))
	result := codexClientMetadataResult{}
	var wg sync.WaitGroup
	wg.Go(func() {
		result.cli.version, result.cli.err = FetchLatestCodexCLIVersion(ctx, input.proxyURL)
	})
	for index, fetch := range fetchers {
		wg.Go(func() { outcomes[index] = fetch() })
	}
	wg.Wait()
	for _, candidates := range outcomes {
		result.candidates = append(result.candidates, candidates...)
	}
	return result
}

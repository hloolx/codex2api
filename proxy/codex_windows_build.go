package proxy

import (
	"archive/zip"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
)

const (
	codexASARHeaderMax  = 8 << 20
	codexASAROutputMax  = 64 << 20
	codexPackageJSONMax = 64 << 10
	codexMSIXBaseURL    = "https://persistent.oaistatic.com/codex-app-prod/"
)

var errCodexMSIXNotFound = errors.New("MSIX not found")

func codexASARPackageLocation(source io.Reader) (int64, int64, int64, error) {
	var prefix [16]byte
	if _, err := io.ReadFull(source, prefix[:]); err != nil {
		return 0, 0, 0, err
	}
	headerSize := int64(binary.LittleEndian.Uint32(prefix[4:8]))
	jsonSize := int64(binary.LittleEndian.Uint32(prefix[12:16]))
	if headerSize < 8 || headerSize > codexASARHeaderMax || jsonSize <= 0 || jsonSize > headerSize-8 {
		return 0, 0, 0, fmt.Errorf("invalid ASAR header size")
	}
	header := make([]byte, jsonSize)
	if _, err := io.ReadFull(source, header); err != nil {
		return 0, 0, 0, err
	}
	var metadata struct {
		Files map[string]struct {
			Offset string `json:"offset"`
			Size   int64  `json:"size"`
		} `json:"files"`
	}
	if err := json.Unmarshal(header, &metadata); err != nil {
		return 0, 0, 0, err
	}
	entry, ok := metadata.Files["package.json"]
	if !ok || entry.Size <= 0 || entry.Size > codexPackageJSONMax {
		return 0, 0, 0, fmt.Errorf("ASAR root package.json missing or oversized")
	}
	offset, err := strconv.ParseInt(entry.Offset, 10, 64)
	if err != nil || offset < 0 {
		return 0, 0, 0, fmt.Errorf("invalid ASAR package.json offset")
	}
	target := 8 + headerSize + offset
	consumed := int64(len(prefix)) + jsonSize
	if target < consumed || target+entry.Size > codexASAROutputMax {
		return 0, 0, 0, fmt.Errorf("ASAR package.json exceeds prefix limit")
	}
	return target, entry.Size, consumed, nil
}

func codexASARPackageVersion(source io.Reader) (string, error) {
	target, size, consumed, err := codexASARPackageLocation(source)
	if err != nil {
		return "", err
	}
	if _, err := io.CopyN(io.Discard, source, target-consumed); err != nil {
		return "", err
	}
	data := make([]byte, size)
	if _, err := io.ReadFull(source, data); err != nil {
		return "", err
	}
	return codexDesktopPackageVersion(data)
}

func codexDesktopPackageVersion(data []byte) (string, error) {
	var pkg struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		return "", err
	}
	if _, ok := codexBuildParts(pkg.Version, 3); !ok {
		return "", fmt.Errorf("invalid Desktop build in package.json")
	}
	return pkg.Version, nil
}

func codexWindowsPackageVersion(ctx context.Context, client *http.Client) (string, []int64, error) {
	resp, err := codexBuildRequest(ctx, client, codexBuildRequestSpec{Method: http.MethodGet, URL: codexWindowsUpdateURL})
	if err != nil {
		return "", nil, err
	}
	data, err := codexReadSmallResponse(resp, 64<<10)
	if err != nil {
		return "", nil, err
	}
	var update struct {
		PackageIdentity string `json:"packageIdentity"`
		BuildVersion    string `json:"buildVersion"`
	}
	if err := json.Unmarshal(data, &update); err != nil {
		return "", nil, err
	}
	parts, valid := codexBuildParts(update.BuildVersion, 4)
	if !valid || update.PackageIdentity != "OpenAI.Codex" {
		return "", nil, fmt.Errorf("invalid Windows update manifest")
	}
	return update.BuildVersion, parts, nil
}

// FetchCodexDesktopWindowsBuild 以 Range 分片读取 MSIX 内 ASAR 的压缩前缀。
func FetchCodexDesktopWindowsBuild(ctx context.Context, proxyURL string) (string, error) {
	client, closeClient := codexBuildHTTPClient(codexWindowsUpdateURL, proxyURL)
	defer closeClient()
	packageVersion, parts, err := codexWindowsPackageVersion(ctx, client)
	if err != nil {
		return "", err
	}
	// 版本化路径并非每个 Store 版本都会上传，缺失时回退到最新包。
	var ranges *codexRangeReader
	fallback := false
	for i, url := range []string{
		codexMSIXBaseURL + "releases/" + packageVersion + "/ChatGPT-x64.msix",
		codexMSIXBaseURL + "ChatGPT-x64.msix",
	} {
		ranges, err = newCodexRangeReader(ctx, client, url)
		fallback = i > 0
		if !errors.Is(err, errCodexMSIXNotFound) {
			break
		}
	}
	if err != nil {
		return "", err
	}
	cacheKey := ""
	if ranges.etag != "" {
		cacheKey = fmt.Sprintf("%s|%s|%d", ranges.url, ranges.etag, ranges.size)
	}
	version, cached := codexWindowsBuildCache.get(cacheKey)
	if !cached {
		if version, err = codexMSIXPackageVersion(ranges); err != nil {
			return "", err
		}
		codexWindowsBuildCache.put(cacheKey, version)
	}
	buildParts, ok := codexBuildParts(version, 3)
	if !ok || !codexWindowsBuildMatchesStore(buildParts, parts, fallback) {
		return "", fmt.Errorf("Desktop package version %s does not match Store build %s", version, packageVersion)
	}
	return version, nil
}

// codexWindowsBuildMatchesStore:版本化包必须与清单主次版本一致;回退的最新包常落后于
// 清单几天(Store 先发布、包后上传),只拒绝比清单还新的异常包。
func codexWindowsBuildMatchesStore(build, store []int64, fallback bool) bool {
	if !fallback {
		return build[0] == store[0] && build[1] == store[1]
	}
	return build[0] < store[0] || build[0] == store[0] && build[1] <= store[1]
}

// codexWindowsBuildCache 以 MSIX URL+ETag+大小为键缓存解析结果:包不变时省掉数十次 Range 请求。
var codexWindowsBuildCache codexWindowsBuildMemo

type codexWindowsBuildMemo struct {
	mu      sync.Mutex
	key     string
	version string
}

func (m *codexWindowsBuildMemo) get(key string) (string, bool) {
	if key == "" {
		return "", false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.version, m.key == key
}

func (m *codexWindowsBuildMemo) put(key, version string) {
	if key == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.key, m.version = key, version
}

func codexMSIXPackageVersion(ranges *codexRangeReader) (string, error) {
	archive, err := zip.NewReader(ranges, ranges.size)
	if err != nil {
		return "", err
	}
	for _, file := range archive.File {
		if file.Name != "app/resources/app.asar" {
			continue
		}
		if file.Method != zip.Deflate {
			return "", fmt.Errorf("unsupported ASAR ZIP compression %d", file.Method)
		}
		stream, err := file.Open()
		if err != nil {
			return "", err
		}
		version, parseErr := codexASARPackageVersion(stream)
		_ = stream.Close()
		return version, parseErr
	}
	return "", fmt.Errorf("Desktop app.asar not found in MSIX")
}

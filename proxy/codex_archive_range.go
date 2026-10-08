package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

const (
	codexMSIXChunkBytes    = 256 << 10
	codexMSIXBudgetBytes   = 12 << 20
	codexRangeRequestLimit = 64
)

var codexContentRangePattern = regexp.MustCompile(`^bytes ([0-9]+)-([0-9]+)/([0-9]+)$`)

type codexRangeReader struct {
	ctx      context.Context
	client   *http.Client
	url      string
	size     int64
	etag     string
	modified string
	used     int64
	requests int
	cache    map[int64][]byte
}

func codexRangeMetadata(resp *http.Response) (start, end, total int64, err error) {
	if resp.StatusCode != http.StatusPartialContent {
		return 0, 0, 0, fmt.Errorf("Range request returned HTTP %d", resp.StatusCode)
	}
	parts := codexContentRangePattern.FindStringSubmatch(resp.Header.Get("Content-Range"))
	if len(parts) != 4 {
		return 0, 0, 0, fmt.Errorf("invalid Content-Range")
	}
	values := make([]int64, 3)
	for i := range values {
		values[i], err = strconv.ParseInt(parts[i+1], 10, 64)
		if err != nil {
			return 0, 0, 0, err
		}
	}
	start, end, total = values[0], values[1], values[2]
	if end < start || total <= end {
		return 0, 0, 0, fmt.Errorf("invalid Content-Range interval")
	}
	return start, end, total, nil
}

func newCodexRangeReader(ctx context.Context, client *http.Client, url string) (*codexRangeReader, error) {
	r := &codexRangeReader{ctx: ctx, client: client, url: url, cache: make(map[int64][]byte)}
	if _, err := r.readRange(0, 0); err != nil {
		return nil, err
	}
	return r, nil
}

func codexStrongETag(etag string) bool {
	return len(etag) >= 2 && strings.HasPrefix(etag, `"`) && strings.HasSuffix(etag, `"`) && !strings.ContainsAny(etag, "\r\n")
}

func (r *codexRangeReader) readRange(start, end int64) ([]byte, error) {
	count := end - start + 1
	if count <= 0 || r.requests >= codexRangeRequestLimit || count > codexMSIXBudgetBytes-r.used {
		return nil, fmt.Errorf("archive Range budget exceeded")
	}
	r.requests++
	r.used += count
	req, err := http.NewRequestWithContext(r.ctx, http.MethodGet, r.url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	req.Header.Set("Accept-Encoding", "identity")
	if codexStrongETag(r.etag) {
		req.Header.Set("If-Range", r.etag)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, codexVersionSourceError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("archive Range returned HTTP 404: %w", errCodexMSIXNotFound)
	}
	if err := r.validateRange(resp, start, end); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, count+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) != count {
		return nil, fmt.Errorf("archive Range body length mismatch")
	}
	return data, nil
}

func (r *codexRangeReader) validateRange(resp *http.Response, start, end int64) error {
	gotStart, gotEnd, total, err := codexRangeMetadata(resp)
	if err != nil {
		return err
	}
	if gotStart != start || gotEnd != end {
		return fmt.Errorf("archive Range interval changed")
	}
	if r.size != 0 && total != r.size {
		return fmt.Errorf("archive size changed during Range read")
	}
	if err := codexRangeBodyMetadata(resp, end-start+1); err != nil {
		return err
	}
	return r.validateArtifact(total, resp.Header)
}

func (r *codexRangeReader) validateArtifact(total int64, header http.Header) error {
	if r.size == 0 {
		r.size, r.etag, r.modified = total, header.Get("ETag"), header.Get("Last-Modified")
		if r.etag == "" && r.modified == "" {
			return fmt.Errorf("archive Range validator missing")
		}
		return nil
	}
	if header.Get("ETag") != r.etag || header.Get("Last-Modified") != r.modified {
		return fmt.Errorf("archive changed during Range read")
	}
	return nil
}

func (r *codexRangeReader) fetch(start int64) error {
	end := min(start+codexMSIXChunkBytes-1, r.size-1)
	data, err := r.readRange(start, end)
	if err != nil {
		return err
	}
	r.cache[start] = data
	return nil
}

func (r *codexRangeReader) ReadAt(p []byte, offset int64) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if offset < 0 || offset >= r.size {
		return 0, io.EOF
	}
	n := 0
	for len(p) > 0 && offset < r.size {
		start := offset / codexMSIXChunkBytes * codexMSIXChunkBytes
		if _, ok := r.cache[start]; !ok {
			if err := r.fetch(start); err != nil {
				return n, err
			}
		}
		copied := copy(p, r.cache[start][offset-start:])
		p, offset, n = p[copied:], offset+int64(copied), n+copied
	}
	if len(p) > 0 {
		return n, io.EOF
	}
	return n, nil
}

func codexRangeBodyMetadata(resp *http.Response, count int64) error {
	if resp.Header.Get("Content-Encoding") != "" && resp.Header.Get("Content-Encoding") != "identity" {
		return fmt.Errorf("encoded archive Range response")
	}
	if resp.ContentLength >= 0 && resp.ContentLength != count {
		return fmt.Errorf("archive Range content length mismatch")
	}
	return nil
}

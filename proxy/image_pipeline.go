package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codex2api/database"
	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// This optional pipeline only applies to durable jobs. A job keeps its in-flight
// slot while waiting, but releases the shared memory-heavy slot before network
// waiting. Both retries and response processing reacquire the memory slot.
type imagePipelineKey struct{}
type ImagePipeline struct {
	slots             chan struct{}
	dir               string
	held              bool
	mu                sync.Mutex
	jobID             int64
	Output            func(context.Context, QueuedImageResult) error
	responseReadError error
}
type imagePipelineGate struct{ slots chan struct{} }

var processImagePipeline atomic.Pointer[imagePipelineGate]

func ConfigureImagePipeline(limit int) {
	if limit <= 0 {
		processImagePipeline.Store(nil)
		return
	}
	processImagePipeline.Store(&imagePipelineGate{slots: make(chan struct{}, limit)})
}
func NewImagePipeline(ctx context.Context, root string, jobID int64) (context.Context, *ImagePipeline, error) {
	gate := processImagePipeline.Load()
	if gate == nil {
		return ctx, nil, nil
	}
	root = filepath.Join(root, "pipeline-tmp")
	if err := os.MkdirAll(root, 0700); err != nil {
		return ctx, nil, err
	}
	dir, err := os.MkdirTemp(root, "job-")
	if err != nil {
		return ctx, nil, err
	}
	p := &ImagePipeline{slots: gate.slots, dir: dir, jobID: jobID}
	return context.WithValue(ctx, imagePipelineKey{}, p), p, nil
}
func pipelineFromContext(ctx context.Context) *ImagePipeline {
	p, _ := ctx.Value(imagePipelineKey{}).(*ImagePipeline)
	return p
}
func (p *ImagePipeline) Acquire(ctx context.Context) error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.held {
		return ctx.Err()
	}
	select {
	case p.slots <- struct{}{}:
		p.held = true
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (p *ImagePipeline) Release() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.held {
		<-p.slots
		p.held = false
	}
}
func (p *ImagePipeline) Close() {
	if p != nil {
		p.Release()
		_ = os.RemoveAll(p.dir)
	}
}
func (p *ImagePipeline) spool(data []byte) (*os.File, error) {
	f, err := os.CreateTemp(p.dir, "body-")
	if err != nil {
		return nil, err
	}
	if _, err = f.Write(data); err == nil {
		_, err = f.Seek(0, io.SeekStart)
	}
	if err != nil {
		f.Close()
		os.Remove(f.Name())
		return nil, err
	}
	return f, nil
}
func removePipelineFile(f *os.File) {
	if f != nil {
		f.Close()
		os.Remove(f.Name())
	}
}

// build runs only after reserving the memory budget. No caller holds a Base64
// buffer across this call; the synthetic HTTP body is backed by a private file.
func (h *Handler) GenerateQueuedImage(ctx context.Context, build func() ([]byte, error), edit bool, key *database.APIKeyRow) ([]byte, int, error) {
	p := pipelineFromContext(ctx)
	if p == nil {
		return nil, 0, fmt.Errorf("image pipeline missing")
	}
	if err := p.Acquire(ctx); err != nil {
		return nil, 0, err
	}
	file, err := buildPipelineInput(p, build)
	if err != nil {
		return nil, 0, err
	}
	defer removePipelineFile(file)
	recorder := newAdminImageResponseRecorder()
	c, _ := gin.CreateTestContext(recorder)
	endpoint := "/v1/images/generations"
	if edit {
		endpoint = "/v1/images/edits"
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, file)
	if err != nil {
		return nil, 0, err
	}
	request.Header.Set("Content-Type", "application/json")
	if key != nil {
		request.Header.Set("Authorization", "Bearer "+key.Key)
		c.Set(contextAPIKeyRow, key)
		c.Set(contextAPIKeyID, key.ID)
		c.Set(contextAPIKeyName, key.Name)
		c.Set(contextAPIKeyMasked, security.MaskAPIKey(key.Key))
	}
	c.Set(contextAPIKeyConcurrencyInherited, true)
	c.Request = request
	if status, msg := h.applyAdminScopeBudget(ctx, c, key); status != 0 {
		return nil, status, fmt.Errorf("%s", msg)
	}
	if edit {
		h.ImagesEdits(c)
	} else {
		h.ImagesGenerations(c)
	}
	body := recorder.Body.Bytes()
	if recorder.Code < 200 || recorder.Code >= 300 {
		message := extractAdminImageErrorMessage(body)
		// Preserve the existing PNG-to-JPEG fallback for upstream failures, but
		// never regenerate because local spooling or saving failed.
		if strings.Contains(message, "cannot save generated image") || strings.Contains(message, "spool") {
			return body, recorder.Code, fmt.Errorf("%s", message)
		}
		return nil, recorder.Code, fmt.Errorf("%s", message)
	}
	return body, recorder.Code, nil
}
func buildPipelineInput(p *ImagePipeline, build func() ([]byte, error)) (*os.File, error) {
	body, err := build()
	if err != nil {
		return nil, err
	}
	return p.spool(body)
}

// The internal queue request has already passed prompt checks. Retain its text
// and small metadata for later policy/usage logging, not embedded image bytes.
func compactPipelineIngress(c *gin.Context) {
	if pipelineFromContext(c.Request.Context()) == nil {
		return
	}
	body, _ := rawRequestBodyFromContext(c)
	compact, _ := sjson.DeleteBytes(body, "images")
	compact, _ = sjson.DeleteBytes(compact, "mask")
	compact = bytes.Clone(compact)
	c.Set("raw_body", compact)
	c.Set(ingressRequestBodyContextKey, compact)
	if state := promptRequestSecurityState(c); state != nil {
		state.digestBody = nil
	}
	_ = c.Request.Body.Close()
	c.Request.Body = http.NoBody
	c.Request.GetBody = nil
}

func executePipelineImage(ctx context.Context, file *os.File, model string, execute func([]byte) (*http.Response, error)) (*http.Response, error) {
	p := pipelineFromContext(ctx)
	if err := p.Acquire(ctx); err != nil {
		return nil, err
	}
	body, err := os.ReadFile(file.Name())
	if err != nil {
		return nil, err
	}
	body, err = sjson.SetBytes(body, "model", model)
	if err != nil {
		return nil, err
	}
	return execute(body)
}

var pipelineTerminalType = regexp.MustCompile(`^data:\s*\{\s*"type"\s*:\s*"(?:response\.(?:completed|failed|incomplete)|error)"`)

const pipelineResponseLimit int64 = 128 << 20

// Read SSE in 32 KiB fragments, including multi-megabyte data lines. We only
// inspect bounded prefixes to detect terminal events; parsing and image decoding
// happen later under the shared memory budget. Local I/O failures never retry GPT.
func (p *ImagePipeline) collectResponse(ctx context.Context, body io.ReadCloser) (*os.File, error) {
	defer body.Close()
	p.Release()
	p.responseReadError = nil
	f, err := os.CreateTemp(p.dir, "response-")
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			removePipelineFile(f)
		}
	}()
	r := bufio.NewReaderSize(body, 32<<10)
	var total int64
	var prefix []byte
	terminal := false
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		part, readErr := r.ReadSlice('\n')
		total += int64(len(part))
		if total > pipelineResponseLimit {
			return nil, fmt.Errorf("image response exceeds 128 MiB")
		}
		if _, err = f.Write(part); err != nil {
			return nil, err
		}
		if len(prefix) < 4096 {
			n := 4096 - len(prefix)
			if n > len(part) {
				n = len(part)
			}
			prefix = append(prefix, part[:n]...)
		}
		if readErr != bufio.ErrBufferFull {
			line := bytes.TrimSpace(prefix)
			if bytes.HasPrefix(line, []byte("event:")) {
				name := strings.TrimSpace(string(line[6:]))
				terminal = terminal || name == "response.completed" || name == "response.failed" || name == "response.incomplete" || name == "error"
			}
			if bytes.HasPrefix(line, []byte("data:")) && pipelineTerminalType.Match(line) {
				terminal = true
			}
			if len(line) == 0 && terminal {
				break
			}
			prefix = prefix[:0]
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil && readErr != bufio.ErrBufferFull {
			p.responseReadError = readErr
			break
		}
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	if err = p.Acquire(ctx); err != nil {
		return nil, err
	}
	log.Printf("[image-pipeline] job=%d stage=result_processing response_bytes=%d", p.jobID, total)
	ok = true
	return f, nil
}

// Keep only small model metadata in the retry coordinator. The full immutable
// request is read from disk for each attempt and then spooled after transforms.
func pipelineRetryMetadata(body []byte) []byte {
	b, _ := sjson.SetBytes([]byte(`{}`), "model", gjson.GetBytes(body, "model").String())
	return b
}

// Per-preparation bounded encoder: the generic shared encoder otherwise keeps
// large history windows for many CPU slots even after requests finish.
func compressPipelineRequestBody(body []byte) ([]byte, string) {
	if len(body) == 0 || !codexRequestCompressionEnabled() {
		return body, ""
	}
	encoder, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault), zstd.WithEncoderConcurrency(1), zstd.WithWindowSize(1<<20))
	if err != nil {
		return body, ""
	}
	defer encoder.Close()
	return encoder.EncodeAll(body, nil), "zstd"
}

type QueuedImageResult struct {
	File                                        *os.File
	Bytes                                       int64
	Width, Height                               int
	Model, Size, Quality, Format, RevisedPrompt string
}
type pipelineOutputError struct{ err error }

func (e *pipelineOutputError) Error() string { return "cannot save generated image: " + e.err.Error() }
func (e *pipelineOutputError) Unwrap() error { return e.err }
func isPipelineOutputError(err error) bool {
	var target *pipelineOutputError
	return errors.As(err, &target)
}

// Decode only the Base64 transport encoding into a file. No pixel decode or
// aggregate Base64 response is needed to transfer an image to the job store.
func (p *ImagePipeline) saveResults(ctx context.Context, results []imageCallResult) ([]byte, error) {
	data := make([]map[string]any, 0, len(results))
	for i := range results {
		result := &results[i]
		item, err := p.decodeResult(result)
		if err == nil {
			err = p.Output(ctx, item)
			removePipelineFile(item.File)
		}
		if err != nil {
			return nil, &pipelineOutputError{err}
		}
		result.Result = ""
		data = append(data, map[string]any{"saved": true, "bytes": item.Bytes, "width": item.Width, "height": item.Height})
	}
	return json.Marshal(map[string]any{"data": data})
}
func (p *ImagePipeline) decodeResult(result *imageCallResult) (QueuedImageResult, error) {
	var item QueuedImageResult
	encoded := strings.TrimSpace(result.Result)
	if strings.HasPrefix(encoded, "data:") {
		if i := strings.IndexByte(encoded, ','); i >= 0 {
			encoded = encoded[i+1:]
		}
	}
	if len(encoded) == 0 || len(encoded) > 100<<20 {
		return item, fmt.Errorf("invalid image output size")
	}
	encoding := base64.StdEncoding
	if strings.ContainsAny(encoded, "-_") {
		encoding = base64.URLEncoding
	}
	if !strings.HasSuffix(encoded, "=") && len(encoded)%4 != 0 {
		encoding = encoding.WithPadding(base64.NoPadding)
	}
	f, err := os.CreateTemp(p.dir, "image-")
	if err != nil {
		return item, err
	}
	ok := false
	defer func() {
		if !ok {
			removePipelineFile(f)
		}
	}()
	n, err := io.Copy(f, base64.NewDecoder(encoding, strings.NewReader(encoded)))
	if err != nil {
		return item, err
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		return item, err
	}
	cfg, format, err := image.DecodeConfig(f)
	if err != nil {
		return item, err
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > 40_000_000 {
		return item, fmt.Errorf("generated image exceeds 40 megapixels")
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		return item, err
	}
	result.ByteSize = int(n)
	result.Width = cfg.Width
	result.Height = cfg.Height
	item = QueuedImageResult{File: f, Bytes: n, Width: cfg.Width, Height: cfg.Height, Model: result.Model, Size: result.Size, Quality: result.Quality, Format: format, RevisedPrompt: result.RevisedPrompt}
	ok = true
	return item, nil
}

type pipelineResponseReader struct {
	*os.File
	err error
}

func (r *pipelineResponseReader) Read(b []byte) (int, error) {
	n, err := r.File.Read(b)
	if err == io.EOF && r.err != nil {
		err = r.err
		r.err = nil
	}
	return n, err
}

func CleanupImagePipelineFiles(ctx context.Context, root string, cutoff time.Time) (int, error) {
	root = filepath.Join(root, "pipeline-tmp")
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	count := 0
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return count, err
		}
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || !strings.HasPrefix(entry.Name(), "job-") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return count, err
		}
		if !info.ModTime().Before(cutoff) {
			continue
		}
		path := filepath.Join(root, entry.Name())
		if filepath.Dir(path) != filepath.Clean(root) {
			continue
		}
		if err = os.RemoveAll(path); err != nil {
			return count, err
		}
		count++
		if count >= 1000 {
			break
		}
	}
	return count, nil
}

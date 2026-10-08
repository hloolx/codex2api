package proxy

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func codexTestRangeServer(t *testing.T, data []byte) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var start, end int
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil || start < 0 || end >= len(data) {
			http.Error(w, "invalid range", http.StatusRequestedRangeNotSatisfiable)
			return
		}
		w.Header().Set("ETag", "0x8DF1F5162265A18")
		if r.Header.Get("If-Range") != "" {
			t.Error("unquoted ETag sent as If-Range")
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(data[start : end+1])
	}))
	t.Cleanup(server.Close)
	return server
}

func codexTestZIP(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var data bytes.Buffer
	w := zip.NewWriter(&data)
	for name, content := range files {
		file, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}

func TestCodexArchiveRangeReadsAndCachesDirectory(t *testing.T) {
	data := codexTestZIP(t, map[string]string{"extension/package.json": `{"version":"26.928.31416"}`})
	server := codexTestRangeServer(t, data)
	r, err := newCodexRangeReader(context.Background(), server.Client(), server.URL)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		archive, err := zip.NewReader(r, r.size)
		if err != nil || len(archive.File) != 1 {
			t.Fatalf("ZIP: %v", err)
		}
	}
	if r.requests != 2 {
		t.Fatalf("expected probe plus cached chunk, got %d", r.requests)
	}
	if r.used > int64(len(data)+1) {
		t.Fatalf("unexpected traffic: %d", r.used)
	}
}

func TestCodexArchiveRangeRejectsInvalidResponses(t *testing.T) {
	for _, kind := range []string{"full", "interval", "size", "etag", "short", "encoding", "validator"} {
		t.Run(kind, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if kind == "full" {
					w.WriteHeader(http.StatusOK)
					return
				}
				var start, end int
				_, _ = fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end)
				total := 2
				if calls > 1 && kind == "size" {
					total++
				}
				if kind == "interval" {
					start++
				}
				w.Header().Set("ETag", `"first"`)
				if kind == "validator" {
					w.Header().Del("ETag")
				}
				if calls > 1 && kind == "etag" {
					w.Header().Set("ETag", `"second"`)
				}
				if kind == "encoding" {
					w.Header().Set("Content-Encoding", "gzip")
				}
				w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, total))
				w.WriteHeader(http.StatusPartialContent)
				if kind != "short" {
					_, _ = w.Write(bytes.Repeat([]byte{0}, end-start+1))
				}
			}))
			defer server.Close()
			r, err := newCodexRangeReader(context.Background(), server.Client(), server.URL)
			if err == nil {
				_, err = r.ReadAt(make([]byte, 2), 0)
			}
			if err == nil {
				t.Fatal("expected invalid range rejection")
			}
		})
	}
}

func TestCodexArchiveRangeBudgetsBeforeRequest(t *testing.T) {
	server := codexTestRangeServer(t, []byte{1, 2})
	r, err := newCodexRangeReader(context.Background(), server.Client(), server.URL)
	if err != nil {
		t.Fatal(err)
	}
	r.used = codexMSIXBudgetBytes
	if _, err := r.readRange(0, 0); err == nil {
		t.Fatal("byte budget not enforced")
	}
	r.used, r.requests = 0, codexRangeRequestLimit
	if _, err := r.readRange(0, 0); err == nil {
		t.Fatal("request budget not enforced")
	}
}

func TestCodexArchiveRangeZIP64(t *testing.T) {
	data := codexTestZIP(t, map[string]string{"metadata.json": `{}`})
	end := append([]byte(nil), data[len(data)-22:]...)
	offset := len(data) - 22
	record := make([]byte, 56)
	binary.LittleEndian.PutUint32(record, 0x06064b50)
	binary.LittleEndian.PutUint64(record[4:], 44)
	binary.LittleEndian.PutUint16(record[12:], 45)
	binary.LittleEndian.PutUint16(record[14:], 45)
	binary.LittleEndian.PutUint64(record[24:], 1)
	binary.LittleEndian.PutUint64(record[32:], 1)
	binary.LittleEndian.PutUint64(record[40:], uint64(binary.LittleEndian.Uint32(end[12:])))
	binary.LittleEndian.PutUint64(record[48:], uint64(binary.LittleEndian.Uint32(end[16:])))
	locator := make([]byte, 20)
	binary.LittleEndian.PutUint32(locator, 0x07064b50)
	binary.LittleEndian.PutUint64(locator[8:], uint64(offset))
	binary.LittleEndian.PutUint32(locator[16:], 1)
	binary.LittleEndian.PutUint16(end[8:], 0xffff)
	binary.LittleEndian.PutUint16(end[10:], 0xffff)
	binary.LittleEndian.PutUint32(end[12:], 0xffffffff)
	binary.LittleEndian.PutUint32(end[16:], 0xffffffff)
	data = append(append(append(data[:offset], record...), locator...), end...)
	server := codexTestRangeServer(t, data)
	archive, err := openCodexVersionArchive(context.Background(), server.Client(), server.URL)
	if err != nil || len(archive.File) != 1 {
		t.Fatalf("ZIP64 directory: %v", err)
	}
}

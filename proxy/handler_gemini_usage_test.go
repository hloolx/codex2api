package proxy

import (
	"context"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const (
	geminiUsageTestMetadata = `{"promptTokenCount":100,"candidatesTokenCount":20,"thoughtsTokenCount":5,"cachedContentTokenCount":30,"totalTokenCount":125}`
	geminiUsageTestTerminal = `{"response":{"candidates":[{"content":{"parts":[{"text":"answer"}]},"finishReason":"STOP"}],"usageMetadata":` + geminiUsageTestMetadata + `}}`
	geminiUsageTestStopOnly = `{"response":{"candidates":[{"content":{"parts":[{"text":"answer"}]},"finishReason":"STOP"}]}}`
	geminiUsageTestTail     = `{"response":{"usageMetadata":` + geminiUsageTestMetadata + `}}`
)

func TestGeminiNativeUsageLogging(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stream bool
		body   string
	}{
		{"nonstream", false, geminiUsageTestTerminal},
		{"stream_terminal_usage", true, "data: " + geminiUsageTestTerminal + "\n\n"},
		{"stream_usage_after_stop", true, "data: " + geminiUsageTestStopOnly + "\n\ndata: " + geminiUsageTestTail + "\n\n"},
		{"stream_repeated_snapshot", true, "data: " + geminiUsageTestTail + "\n\ndata: " + geminiUsageTestTerminal + "\n\ndata: " + geminiUsageTestTail + "\n\n"},
		{"stream_progress_snapshot", true, "data: " + `{"response":{"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":2,"thoughtsTokenCount":1,"cachedContentTokenCount":30,"totalTokenCount":103}}}` + "\n\ndata: " + geminiUsageTestTerminal + "\n\n"},
		{"stream_no_trailing_separator", true, "data: " + geminiUsageTestTerminal},
		{"nonstream_cpa_metadata", false, strings.Replace(geminiUsageTestTerminal, "usageMetadata", "cpaUsageMetadata", 1)},
		{"stream_cpa_metadata", true, "data: " + strings.Replace(geminiUsageTestTerminal, "usageMetadata", "cpaUsageMetadata", 1) + "\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			router, db := newGeminiStreamTestHandler(t, func(w http.ResponseWriter, _ *http.Request) {
				if tc.stream {
					w.Header().Set("Content-Type", "text/event-stream")
				} else {
					w.Header().Set("Content-Type", "application/json")
				}
				_, _ = io.WriteString(w, tc.body)
			})
			method := "generateContent"
			if tc.stream {
				method = "streamGenerateContent"
			}
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, geminiStreamTestHTTPRequest(context.Background(), method))
			if recorder.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			// Accounting must not replace the native wire counts with our internal
			// output total (which includes thinking), or alter thought/tool content.
			if !strings.Contains(recorder.Body.String(), `"candidatesTokenCount":20`) || !strings.Contains(recorder.Body.String(), `"cachedContentTokenCount":30`) {
				t.Fatalf("native usage was changed or dropped: %s", recorder.Body.String())
			}
			entry := assertGeminiStreamUsageStatus(t, db, http.StatusOK, tc.stream)
			assertGeminiNativeUsage(t, entry, 100, 25, 5, 30, 125)
			if entry.Endpoint != geminiInboundEndpoint(tc.stream) || entry.InboundEndpoint != entry.Endpoint {
				t.Fatalf("unexpected usage endpoint: %q / %q", entry.Endpoint, entry.InboundEndpoint)
			}
			if tc.stream && entry.FirstTokenMs <= 0 {
				t.Fatal("stream did not record first content time")
			}
			wantCost := database.CalculateCostBreakdown(100, 25, 30, entry.EffectiveModel, "")
			if math.Abs(entry.TotalCost-wantCost.TotalCost) > 1e-12 || math.Abs(entry.CacheReadCost-wantCost.CacheReadCost) > 1e-12 {
				t.Fatalf("cost=%g cache_cost=%g, want %g / %g", entry.TotalCost, entry.CacheReadCost, wantCost.TotalCost, wantCost.CacheReadCost)
			}
		})
	}
}

func assertGeminiNativeUsage(t *testing.T, entry database.UsageLog, input, output, reasoning, cached, total int) {
	t.Helper()
	if entry.InputTokens != input || entry.OutputTokens != output || entry.ReasoningTokens != reasoning || entry.CachedTokens != cached || entry.TotalTokens != total {
		t.Fatalf("usage input/output/reasoning/cache/total=%d/%d/%d/%d/%d, want %d/%d/%d/%d/%d", entry.InputTokens, entry.OutputTokens, entry.ReasoningTokens, entry.CachedTokens, entry.TotalTokens, input, output, reasoning, cached, total)
	}
	if entry.PromptTokens != input || entry.CompletionTokens != output {
		t.Fatalf("legacy prompt/completion=%d/%d, want %d/%d", entry.PromptTokens, entry.CompletionTokens, input, output)
	}
}

func TestGeminiNativeStreamSparseUsage(t *testing.T) {
	const input = `{"response":{"usageMetadata":{"promptTokenCount":100,"cachedContentTokenCount":30}}}`
	const output = `{"response":{"usageMetadata":{"candidatesTokenCount":20,"thoughtsTokenCount":5}}}`
	for _, tc := range []struct {
		name  string
		tail  string
		total int
	}{
		{"empty_metadata", `{"response":{"usageMetadata":{}}}`, 125},
		{"keepalive", `{"response":{}}`, 125},
		{"total_only", `{"response":{"usageMetadata":{"totalTokenCount":125}}}`, 125},
		{"larger_upstream_total", `{"response":{"usageMetadata":{"totalTokenCount":130}}}`, 130},
		{"inconsistent_upstream_total", `{"response":{"usageMetadata":{"totalTokenCount":1}}}`, 125},
	} {
		t.Run(tc.name, func(t *testing.T) {
			router, db := newGeminiStreamTestHandler(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: "+input+"\n\ndata: "+geminiUsageTestStopOnly+"\n\ndata: "+output+"\n\ndata: "+tc.tail+"\n\n")
			})
			router.ServeHTTP(httptest.NewRecorder(), geminiStreamTestHTTPRequest(context.Background(), "streamGenerateContent"))
			assertGeminiNativeUsage(t, assertGeminiStreamUsageStatus(t, db, http.StatusOK, true), 100, 25, 5, 30, tc.total)
		})
	}
}

func TestGeminiNativeUsageMatchesResponsesAccounting(t *testing.T) {
	for _, raw := range []string{
		geminiUsageTestMetadata,
		`{"promptTokenCount":100,"candidatesTokenCount":20,"thoughtsTokenCount":5,"cachedContentTokenCount":30}`,
		`{"promptTokenCount":100,"thoughtsTokenCount":5}`,
	} {
		t.Run(raw, func(t *testing.T) {
			native := geminiNativeUsageFromBody([]byte(`{"usageMetadata":` + raw + `}`))
			metadata := gjson.Parse(raw).Value().(map[string]any)
			response := gjson.Parse(antigravityJSON(antigravityUsage(metadata)))
			converted := extractUsageFromResult(response)
			if native.InputTokens != converted.InputTokens || native.OutputTokens != converted.OutputTokens || native.ReasoningTokens != converted.ReasoningTokens || native.CachedTokens != converted.CachedTokens || native.TotalTokens != int(response.Get("total_tokens").Int()) {
				t.Fatalf("native=%+v Responses=%s", native, response.Raw)
			}
		})
	}
}

func TestGeminiNativeMissingUsageDoesNotEstimateTokens(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(geminiInboundEndpoint(stream), func(t *testing.T) {
			router, db := newGeminiStreamTestHandler(t, func(w http.ResponseWriter, _ *http.Request) {
				body := geminiUsageTestStopOnly
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					body = "data: " + body + "\n\n"
				}
				_, _ = io.WriteString(w, body)
			})
			method := "generateContent"
			if stream {
				method = "streamGenerateContent"
			}
			router.ServeHTTP(httptest.NewRecorder(), geminiStreamTestHTTPRequest(context.Background(), method))
			assertGeminiNativeUsage(t, assertGeminiStreamUsageStatus(t, db, http.StatusOK, stream), 0, 0, 0, 0, 0)
		})
	}
}

func TestGeminiNativeStreamRetainsUsageOnReadFailure(t *testing.T) {
	for _, truncated := range []bool{false, true} {
		name := "missing_terminal"
		if truncated {
			name = "transport_error_after_terminal"
		}
		t.Run(name, func(t *testing.T) {
			router, db := newGeminiStreamTestHandler(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				body := geminiUsageTestTail
				if truncated {
					w.Header().Set("Content-Length", "10000")
					body = geminiUsageTestTerminal
				}
				_, _ = io.WriteString(w, "data: "+body+"\n\n")
			})
			router.ServeHTTP(httptest.NewRecorder(), geminiStreamTestHTTPRequest(context.Background(), "streamGenerateContent"))
			assertGeminiNativeUsage(t, assertGeminiStreamUsageStatus(t, db, logStatusUpstreamStreamBreak, true), 100, 25, 5, 30, 125)
		})
	}
}

func TestGeminiNativeStreamRetainsUsageOnDisconnect(t *testing.T) {
	for _, fail := range []bool{false, true} {
		name := "client_cancel"
		if fail {
			name = "write_error"
		}
		t.Run(name, func(t *testing.T) {
			router, db := newGeminiStreamTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: "+geminiUsageTestTail+"\n\n")
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			})
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			writer := &geminiStreamDisconnectWriter{ResponseRecorder: httptest.NewRecorder(), cancel: cancel, fail: fail}
			router.ServeHTTP(writer, geminiStreamTestHTTPRequest(ctx, "streamGenerateContent"))
			entry := assertGeminiStreamUsageStatus(t, db, logStatusClientClosed, true)
			assertGeminiNativeUsage(t, entry, 100, 25, 5, 30, 125)
			if entry.FirstTokenMs != 0 {
				t.Fatal("usage-only frame was counted as content")
			}
		})
	}
}

func TestGeminiNativeStreamFirstTokenRequiresContent(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload string
		content bool
	}{
		{"keepalive", `{}`, false},
		{"usage", `{"usageMetadata":` + geminiUsageTestMetadata + `}`, false},
		{"empty_part", `{"candidates":[{"content":{"parts":[{"text":""}]}}]}`, false},
		{"signature", `{"candidates":[{"content":{"parts":[{"thoughtSignature":"sig"}]}}]}`, false},
		{"stop", `{"candidates":[{"finishReason":"STOP"}]}`, false},
		{"text", `{"candidates":[{"content":{"parts":[{"text":"answer"}]}}]}`, true},
		{"thought", `{"candidates":[{"content":{"parts":[{"text":"thinking","thought":true}]}}]}`, true},
		{"tool", `{"candidates":[{"content":{"parts":[{"functionCall":{"name":"lookup","args":{}}}]}}]}`, true},
		{"image", `{"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"image/png","data":"test"}}]}}]}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini:streamGenerateContent", nil)
			wire := "data: " + tc.payload + "\n\n"
			result := forwardGeminiNativeStream(c, strings.NewReader(wire), time.Now().Add(-time.Second))
			if result.readErr != nil || result.writeErr != nil {
				t.Fatalf("forward errors: %v / %v", result.readErr, result.writeErr)
			}
			if (result.firstTokenMs > 0) != tc.content || (tc.content && result.firstTokenMs < 1000) {
				t.Fatalf("first_token_ms=%d content=%v", result.firstTokenMs, tc.content)
			}
			if recorder.Body.String() != wire || !gjson.Valid(tc.payload) {
				t.Fatalf("wire changed: %s", recorder.Body.String())
			}
		})
	}
}

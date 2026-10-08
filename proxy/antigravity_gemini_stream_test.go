package proxy

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

const antigravityNativeGeminiThoughtEvent = `{"response":{"candidates":[{"content":{"parts":[{"text":"thinking","thought":true,"thoughtSignature":"signature"}]}}]}}`
const antigravityNativeGeminiStopEvent = `{"response":{"candidates":[{"content":{"parts":[{"text":"answer"}]},"finishReason":"STOP"}]}}`

func TestAntigravityNativeGeminiSSEBodyRequiresTerminal(t *testing.T) {
	for _, tc := range []struct {
		name string
		data string
	}{
		{"empty", ""},
		{"thought_only", antigravityNativeGeminiThoughtEvent},
		{"partial_text", `{"candidates":[{"content":{"parts":[{"text":"partial"}]}}]}`},
		{"tool_without_terminal", `{"candidates":[{"content":{"parts":[{"functionCall":{"name":"lookup","args":{}}}]}}]}`},
		{"usage_only", `{"usageMetadata":{"totalTokenCount":5}}`},
		{"unspecified_finish", `{"candidates":[{"finishReason":"FINISH_REASON_UNSPECIFIED"}]}`},
		{"unspecified_block", `{"promptFeedback":{"blockReason":"BLOCK_REASON_UNSPECIFIED"}}`},
	} {
		for _, ending := range []string{"EOF", "DONE"} {
			t.Run(tc.name+"/"+ending, func(t *testing.T) {
				var input string
				if tc.data != "" {
					input = "data: " + tc.data + "\n\n"
				}
				if ending == "DONE" {
					input += "data: [DONE]\n\n"
				}
				body := newAntigravityNativeGeminiSSEResponseBody(io.NopCloser(strings.NewReader(input)), nil)
				defer body.Close()
				out, err := io.ReadAll(body)
				if !errors.Is(err, io.ErrUnexpectedEOF) {
					t.Fatalf("missing terminal error = %v; output = %s", err, out)
				}
				if strings.Contains(string(out), `"finishReason":"STOP"`) {
					t.Fatalf("stream synthesized success: %s", out)
				}
				if tc.data != "" && len(out) == 0 {
					t.Fatal("valid partial event was lost")
				}
			})
		}
	}
}

func TestAntigravityNativeGeminiSSEBodyPreservesTerminalResponses(t *testing.T) {
	for _, tc := range []struct {
		name string
		data string
	}{
		{"stop", antigravityNativeGeminiStopEvent},
		{"max_tokens", `{"candidates":[{"content":{"parts":[{"text":"partial"}]},"finishReason":"MAX_TOKENS"}]}`},
		{"safety", `{"candidates":[{"finishReason":"SAFETY"}]}`},
		{"blocked_prompt", `{"promptFeedback":{"blockReason":"SAFETY"}}`},
	} {
		for _, ending := range []string{"", "\n", "\r\n\r\n", "\n\ndata: [DONE]\n\n"} {
			t.Run(tc.name+"/"+ending, func(t *testing.T) {
				body := newAntigravityNativeGeminiSSEResponseBody(io.NopCloser(strings.NewReader("data: "+tc.data+ending)), nil)
				defer body.Close()
				out, err := io.ReadAll(body)
				if err != nil {
					t.Fatalf("read terminal response: %v", err)
				}
				want := "data: " + string(unwrapAntigravityNativeGeminiChunk([]byte(tc.data))) + "\n\n"
				if string(out) != want {
					t.Fatalf("terminal event changed:\n got: %s\nwant: %s", out, want)
				}
			})
		}
	}
}

// Return the error only once, together with the last bytes. A reader must not
// replace it with the EOF from the next Read or discard the accompanying event.
type antigravityNativeGeminiFinalErrorReader struct {
	data []byte
	err  error
}

func (r *antigravityNativeGeminiFinalErrorReader) Read(p []byte) (int, error) {
	if len(r.data) > 0 {
		n := copy(p, r.data)
		r.data = r.data[n:]
		if len(r.data) > 0 {
			return n, nil
		}
		err := r.err
		r.err = nil
		return n, err
	}
	if r.err != nil {
		err := r.err
		r.err = nil
		return 0, err
	}
	return 0, io.EOF
}

func TestAntigravityNativeGeminiSSEBodyPreservesTransportErrors(t *testing.T) {
	for _, sourceErr := range []error{context.Canceled, context.DeadlineExceeded, io.ErrUnexpectedEOF} {
		for _, tc := range []struct {
			name string
			data string
		}{
			{"empty", ""},
			{"thought", antigravityNativeGeminiThoughtEvent},
			{"terminal", antigravityNativeGeminiStopEvent},
		} {
			for _, ending := range []string{"", "\n\n"} {
				t.Run(sourceErr.Error()+"/"+tc.name+"/"+ending, func(t *testing.T) {
					input := ""
					if tc.data != "" {
						input = "data: " + tc.data + ending
					}
					source := &antigravityNativeGeminiFinalErrorReader{data: []byte(input), err: sourceErr}
					body := newAntigravityNativeGeminiSSEResponseBody(io.NopCloser(source), nil)
					defer body.Close()
					// Small reads also ensure a pending error cannot hide queued bytes.
					var out strings.Builder
					buf := make([]byte, 7)
					for {
						n, err := body.Read(buf)
						out.Write(buf[:n])
						if err != nil {
							if !errors.Is(err, sourceErr) {
								t.Fatalf("read error = %v, want %v", err, sourceErr)
							}
							break
						}
					}
					want := ""
					if tc.data != "" {
						want = "data: " + string(unwrapAntigravityNativeGeminiChunk([]byte(tc.data))) + "\n\n"
					}
					if out.String() != want {
						t.Fatalf("data before error changed:\n got: %s\nwant: %s", out.String(), want)
					}
				})
			}
		}
	}
}

func TestAntigravityNativeGeminiSSEBodyPreservesThoughtTextToolsAndUsage(t *testing.T) {
	events := []string{
		antigravityNativeGeminiThoughtEvent,
		`{"response":{"candidates":[{"content":{"parts":[{"text":"first "}]}}]}}`,
		`{"response":{"candidates":[{"content":{"parts":[{"functionCall":{"name":"mcp_server_read","args":{"path":"file"}},"thoughtSignature":"tool-signature"}]}}]}}`,
		antigravityNativeGeminiStopEvent,
		`{"response":{"cpaUsageMetadata":{"totalTokenCount":12}}}`,
	}
	var input strings.Builder
	var want strings.Builder
	reverseNameMap := map[string]string{"mcp_server_read": "mcp/server/read"}
	for _, event := range events {
		input.WriteString("data: " + event + "\n\n")
		chunk := unwrapAntigravityNativeGeminiChunk([]byte(event))
		chunk = antigravityRestoreNativeGeminiResponseNames(chunk, reverseNameMap)
		want.WriteString("data: " + string(chunk) + "\n\n")
	}
	input.WriteString("data: [DONE]\n\n")
	body := newAntigravityNativeGeminiSSEResponseBody(io.NopCloser(strings.NewReader(input.String())), reverseNameMap)
	defer body.Close()
	out, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != want.String() {
		t.Fatalf("stream lost or changed events:\n got: %s\nwant: %s", out, want.String())
	}
	for _, fragment := range []string{`"thought":true`, `"text":"first "`, `"name":"mcp/server/read"`, `"thoughtSignature":"tool-signature"`, `"finishReason":"STOP"`, `"usageMetadata":{"totalTokenCount":12}`} {
		if !strings.Contains(string(out), fragment) {
			t.Fatalf("stream omitted %s: %s", fragment, out)
		}
	}
}

func TestAntigravityNativeGeminiSSEBodyRejectsInvalidAndErrorEvents(t *testing.T) {
	for _, tc := range []struct {
		name string
		data string
		want string
	}{
		{"malformed", `{"candidates":`, "decode antigravity Gemini stream event"},
		{"array", `[]`, "decode antigravity Gemini stream event"},
		{"null", `null`, "must be a JSON object"},
		{"error", `{"error":{"code":503,"message":"upstream unavailable"}}`, "upstream unavailable"},
		{"wrapped_error", `{"response":{"error":{"message":"upstream unavailable"}}}`, "upstream unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := "data: " + antigravityNativeGeminiThoughtEvent + "\n\ndata: " + tc.data + "\n\n"
			body := newAntigravityNativeGeminiSSEResponseBody(io.NopCloser(strings.NewReader(input)), nil)
			defer body.Close()
			out, err := io.ReadAll(body)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("read error = %v, want %q", err, tc.want)
			}
			want := "data: " + string(unwrapAntigravityNativeGeminiChunk([]byte(antigravityNativeGeminiThoughtEvent))) + "\n\n"
			if string(out) != want {
				t.Fatalf("invalid event was forwarded or valid event was lost: %s", out)
			}
		})
	}
}

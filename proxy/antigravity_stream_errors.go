package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
)

// Cloud Code can accept streamGenerateContent with HTTP 200 and report a
// backend failure later in the same body, either as an SSE data event or as a
// bare, often pretty-printed JSON document outside SSE framing:
//
//	data: {"response":{"candidates":[...partial text...]}}
//
//	{
//	  "error": {"code": 503, "message": "No capacity available for model ...", "status": "UNAVAILABLE"}
//	}
//
// The status and Google's structured quota details have to survive the stream
// converters so a mid-stream failure gets the same quota/capacity policy as the
// equivalent HTTP error, instead of a generic stream break charged to the
// account.

const (
	antigravityStreamErrorMessageLimit = 2048
	antigravityStreamErrorDetailsLimit = 8
)

var errAntigravityStreamEventTooLarge = errors.New("antigravity stream event exceeded the safe size limit")

// antigravitySSEReader returns one JSON payload per call: the joined data
// lines of an SSE event, or a bare JSON document written without SSE framing.
type antigravitySSEReader struct {
	reader *bufio.Reader
	// pending holds a bare-document line that ended the previous payload
	// without the blank-line separator.
	pending []byte
	// err is the read error that ended the stream. A payload read together
	// with it is returned first; every later call reports the error itself, so
	// a transport failure is not mistaken for a clean end of stream.
	err error
}

func newAntigravitySSEReader(reader *bufio.Reader) *antigravitySSEReader {
	return &antigravitySSEReader{reader: reader}
}

func (r *antigravitySSEReader) next() ([]byte, error) {
	var buf []byte
	// bare marks a document written without SSE framing; continued marks a
	// data payload whose JSON carries on in unprefixed lines.
	bare, continued := false, false
	checkedLen, checkedValid := -1, false
	complete := func() bool {
		if checkedLen != len(buf) {
			checkedLen, checkedValid = len(buf), json.Valid(buf)
		}
		return checkedValid
	}
	appendLine := func(line []byte) {
		if len(buf) > 0 {
			buf = append(buf, '\n')
		}
		buf = append(buf, line...)
	}
	// stash ends the current payload at a line that belongs to the next one.
	stash := func(line []byte, err error) ([]byte, error) {
		r.pending = append([]byte(nil), line...)
		if err != nil {
			r.err = err
		}
		return buf, nil
	}
	for {
		var line []byte
		var err error
		switch {
		case r.pending != nil:
			line, r.pending = r.pending, nil
		case r.err != nil:
			err = r.err
		default:
			line, err = r.reader.ReadBytes('\n')
		}
		if len(line) > 0 {
			line = bytes.TrimRight(line, "\r\n")
			trimmed := bytes.TrimSpace(line)
			switch {
			case bytes.HasPrefix(line, []byte("data:")):
				if bare {
					// The bare document ended without a blank line.
					return stash(line, err)
				}
				appendLine(bytes.TrimSpace(line[len("data:"):]))
			case len(trimmed) == 0:
				if len(buf) > 0 && (!bare || complete()) {
					return buf, nil
				}
			case antigravitySSEFieldLine(line):
				// event:/id:/retry: fields and comments carry no payload.
			case bare:
				appendLine(trimmed)
				// Only an unindented closing line can end a pretty-printed
				// document, so the buffer is validated about once per document
				// rather than once per line.
				if antigravityTopLevelClosingLine(line) && complete() {
					return buf, nil
				}
			case continued:
				appendLine(trimmed)
			case len(buf) > 0 && !complete():
				// A data payload split across unprefixed lines; the event still
				// ends at its blank line.
				continued = true
				appendLine(trimmed)
			case trimmed[0] == '{' || trimmed[0] == '[':
				if len(buf) > 0 {
					// A complete payload followed by a bare document with no
					// blank line between them: finish the payload first.
					return stash(line, err)
				}
				bare = true
				appendLine(trimmed)
				if antigravityTopLevelClosingLine(line) && complete() {
					return buf, nil
				}
			}
			limit := antigravityResponseBodyLimit
			if bare {
				limit = antigravityErrorBodyLimit
			}
			if len(buf) > limit {
				r.err = errAntigravityStreamEventTooLarge
				return nil, r.err
			}
		}
		if err != nil {
			r.err = err
			if len(buf) > 0 {
				return buf, nil
			}
			return nil, err
		}
	}
}

func antigravitySSEFieldLine(line []byte) bool {
	if len(line) > 0 && line[0] == ':' {
		return true
	}
	for _, field := range []string{"event:", "id:", "retry:"} {
		if bytes.HasPrefix(line, []byte(field)) {
			return true
		}
	}
	return false
}

// antigravityTopLevelClosingLine reports a line that can close a bare JSON
// document: unindented and ending in } or ]. Nested closing lines of a
// pretty-printed document are indented.
func antigravityTopLevelClosingLine(line []byte) bool {
	if len(line) == 0 || line[0] == ' ' || line[0] == '\t' {
		return false
	}
	trimmed := bytes.TrimSpace(line)
	last := trimmed[len(trimmed)-1]
	return last == '}' || last == ']'
}

// decodeAntigravityStreamEnvelope decodes a stream payload into the object the
// converters inspect. Google wraps standalone error documents in a JSON array
// ([{"error": {...}}]); the first element carrying an error is used.
func decodeAntigravityStreamEnvelope(data []byte) (map[string]any, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		var items []any
		if err := json.Unmarshal(trimmed, &items); err != nil {
			return nil, err
		}
		for _, item := range items {
			if object, ok := item.(map[string]any); ok {
				if _, hasError := object["error"]; hasError {
					return object, nil
				}
			}
		}
		return nil, fmt.Errorf("antigravity stream payload is an array without an error object")
	}
	var envelope map[string]any
	if err := json.Unmarshal(trimmed, &envelope); err != nil {
		return nil, err
	}
	if envelope == nil {
		return nil, fmt.Errorf("antigravity stream payload must be a JSON object")
	}
	return envelope, nil
}

// antigravityUpstreamErrorFields converts a Google error object into the
// Responses error value. status_code drives retry/penalty classification, and
// the RPC status plus the quota details (ErrorInfo reason/model/reset delay,
// RetryInfo delay) let applyAntigravityCooldown size the cooldown exactly as
// it does for an HTTP error body. Other details are dropped so consumer or
// project metadata is not forwarded to downstream clients.
func antigravityUpstreamErrorFields(value any) (int, map[string]any) {
	detail, _ := value.(map[string]any)
	message, _ := detail["message"].(string)
	message = truncateAntigravityStreamErrorMessage(strings.TrimSpace(message))
	if message == "" {
		message = "antigravity upstream error"
	}
	rpcStatus, _ := detail["status"].(string)
	rpcStatus = strings.ToUpper(strings.TrimSpace(rpcStatus))
	statusCode := antigravityStreamErrorStatusCode(detail["code"], rpcStatus)
	fields := map[string]any{
		"code":        ErrorCodeUpstreamError,
		"message":     message,
		"status_code": statusCode,
	}
	if rpcStatus != "" {
		fields["status"] = rpcStatus
	}
	if details := antigravityQuotaErrorDetails(detail["details"]); len(details) > 0 {
		fields["details"] = details
	}
	return statusCode, fields
}

func antigravityStreamErrorStatusCode(code any, rpcStatus string) int {
	var numeric int
	switch v := code.(type) {
	case float64:
		numeric = int(v)
	case json.Number:
		parsed, _ := v.Int64()
		numeric = int(parsed)
	case string:
		numeric, _ = strconv.Atoi(strings.TrimSpace(v))
	}
	if numeric >= 400 && numeric <= 599 {
		return numeric
	}
	switch rpcStatus {
	case "INVALID_ARGUMENT", "FAILED_PRECONDITION", "OUT_OF_RANGE":
		return http.StatusBadRequest
	case "UNAUTHENTICATED":
		return http.StatusUnauthorized
	case "PERMISSION_DENIED":
		return http.StatusForbidden
	case "NOT_FOUND":
		return http.StatusNotFound
	case "ALREADY_EXISTS", "ABORTED":
		return http.StatusConflict
	case "RESOURCE_EXHAUSTED":
		return http.StatusTooManyRequests
	case "UNIMPLEMENTED":
		return http.StatusNotImplemented
	case "INTERNAL", "UNKNOWN", "DATA_LOSS":
		return http.StatusInternalServerError
	case "UNAVAILABLE":
		return http.StatusServiceUnavailable
	case "DEADLINE_EXCEEDED":
		return http.StatusGatewayTimeout
	default:
		// CANCELLED and unknown statuses stay a gateway failure; 499 is
		// reserved for the downstream client closing the request.
		return http.StatusBadGateway
	}
}

func antigravityQuotaErrorDetails(value any) []any {
	items, _ := value.([]any)
	out := make([]any, 0, len(items))
	for _, item := range items {
		if len(out) >= antigravityStreamErrorDetailsLimit {
			break
		}
		detail, ok := item.(map[string]any)
		if !ok {
			continue
		}
		typeName, _ := detail["@type"].(string)
		switch {
		case strings.HasSuffix(typeName, "google.rpc.ErrorInfo"):
			kept := map[string]any{"@type": typeName}
			if reason, ok := detail["reason"].(string); ok {
				kept["reason"] = reason
			}
			if metadata, ok := detail["metadata"].(map[string]any); ok {
				keptMetadata := map[string]any{}
				for _, key := range []string{"model", "quotaResetDelay", "quotaResetTimeStamp"} {
					if v, ok := metadata[key]; ok {
						keptMetadata[key] = v
					}
				}
				if len(keptMetadata) > 0 {
					kept["metadata"] = keptMetadata
				}
			}
			out = append(out, kept)
		case strings.HasSuffix(typeName, "google.rpc.RetryInfo"):
			kept := map[string]any{"@type": typeName}
			if delay, ok := detail["retryDelay"]; ok {
				kept["retryDelay"] = delay
			}
			out = append(out, kept)
		}
	}
	return out
}

func truncateAntigravityStreamErrorMessage(message string) string {
	if len(message) <= antigravityStreamErrorMessageLimit {
		return message
	}
	cut := antigravityStreamErrorMessageLimit
	for cut > 0 && !utf8.RuneStart(message[cut]) {
		cut--
	}
	return message[:cut]
}

// antigravityStreamError carries a backend error document from a native
// Gemini stream to the handler, which applies the same cooldown/penalty
// policy as for an HTTP error with that status and body.
type antigravityStreamError struct {
	StatusCode int
	// Body is the normalized {"error": {...}} document.
	Body []byte
}

func (e *antigravityStreamError) Error() string {
	message := strings.TrimSpace(gjson.GetBytes(e.Body, "error.message").String())
	if message == "" {
		return fmt.Sprintf("antigravity Gemini upstream returned an error (HTTP %d)", e.StatusCode)
	}
	return "antigravity Gemini upstream returned an error: " + message
}

func newAntigravityStreamError(value any) *antigravityStreamError {
	statusCode, fields := antigravityUpstreamErrorFields(value)
	body, _ := json.Marshal(map[string]any{"error": fields})
	return &antigravityStreamError{StatusCode: statusCode, Body: body}
}

// antigravityStreamErrorOutcome classifies a backend error document reported
// inside a 200 native Gemini stream like the equivalent HTTP error: status and
// message for the usage log, the Cloud Code quota cooldown, and no account
// penalty for a shared capacity shortage (see reportStreamOutcomeFailure).
func (h *Handler) antigravityStreamErrorOutcome(account *auth.Account, streamErr *antigravityStreamError, resp *http.Response, model string) streamOutcome {
	outcome := classifyResponseFailedOutcome(streamErr.Body)
	decision := h.applyResponseFailedCooldown(account, streamErr.Body, resp, model)
	return applyResponseFailedDecisionKind(outcome, streamErr.Body, decision)
}

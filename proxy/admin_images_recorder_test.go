package proxy

import (
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestAdminImageRecorderKeepsFinalStatusAfterInformationalResponses(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusBadRequest, http.StatusTooManyRequests, http.StatusBadGateway} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			recorder := newAdminImageResponseRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			// The production keepalive writes directly to Gin's underlying writer.
			writer := ctx.Writer.(interface{ Unwrap() http.ResponseWriter }).Unwrap()
			writer.WriteHeader(http.StatusProcessing)
			writer.WriteHeader(http.StatusProcessing)
			writer.WriteHeader(http.StatusEarlyHints)
			body := `{"error":{"message":"upstream failed"}}`
			if status == http.StatusOK {
				body = `{"data":[{"b64_json":"` + strings.Repeat("AQID", 2048) + `"}]}`
			}
			ctx.Data(status, "application/json", []byte(body))
			if recorder.Code != status || recorder.Body.String() != body {
				t.Fatalf("status=%d want=%d bytes=%d want=%d", recorder.Code, status, recorder.Body.Len(), len(body))
			}
		})
	}
}

func TestAdminImageRecorderDoesNotReplaceFinalStatus(t *testing.T) {
	recorder := newAdminImageResponseRecorder()
	recorder.WriteHeader(http.StatusForbidden)
	recorder.WriteHeader(http.StatusProcessing)
	recorder.WriteHeader(http.StatusOK)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("final error was replaced: %d", recorder.Code)
	}
}

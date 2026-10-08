package proxy

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

func TestResolveContinuity(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name         string
		ctx          context.Context
		account      *auth.Account
		req          Request
		opts         Options
		wantSource   string
		wantNonEmpty bool
	}{
		{
			name: "prompt_cache_key in body",
			req: Request{
				Payload: []byte(`{"prompt_cache_key": "test-cache-key-123"}`),
				Headers: http.Header{},
			},
			opts:         Options{},
			wantSource:   "prompt_cache_key",
			wantNonEmpty: true,
		},
		{
			name: "prompt_cache_key with whitespace",
			req: Request{
				Payload: []byte(`{"prompt_cache_key": "  test-key-456  "}`),
				Headers: http.Header{},
			},
			opts:         Options{},
			wantSource:   "prompt_cache_key",
			wantNonEmpty: true,
		},
		{
			name: "execution_session in metadata",
			req: Request{
				Payload: []byte(`{}`),
				Headers: http.Header{},
			},
			opts: Options{
				Metadata: map[string]any{
					ExecutionSessionMetadataKey: "exec-session-789",
				},
			},
			wantSource:   "execution_session",
			wantNonEmpty: true,
		},
		{
			name: "idempotency_key in header",
			req: Request{
				Payload: []byte(`{}`),
				Headers: func() http.Header {
					h := http.Header{}
					h.Set("Idempotency-Key", "idem-key-abc")
					return h
				}(),
			},
			opts:         Options{},
			wantSource:   "idempotency_key",
			wantNonEmpty: true,
		},
		{
			name: "idempotency_key with whitespace",
			req: Request{
				Payload: []byte(`{}`),
				Headers: func() http.Header {
					h := http.Header{}
					h.Set("Idempotency-Key", "  idem-key-def  ")
					return h
				}(),
			},
			opts:         Options{},
			wantSource:   "idempotency_key",
			wantNonEmpty: true,
		},
		{
			name: "priority: prompt_cache_key over execution_session",
			req: Request{
				Payload: []byte(`{"prompt_cache_key": "high-priority-key"}`),
				Headers: func() http.Header {
					h := http.Header{}
					h.Set("Idempotency-Key", "low-priority-key")
					return h
				}(),
			},
			opts: Options{
				Metadata: map[string]any{
					ExecutionSessionMetadataKey: "mid-priority-key",
				},
			},
			wantSource:   "prompt_cache_key",
			wantNonEmpty: true,
		},
		{
			name: "empty payload",
			req: Request{
				Payload: []byte(`{}`),
				Headers: http.Header{},
			},
			opts:         Options{},
			wantSource:   "",
			wantNonEmpty: false,
		},
		{
			name: "whitespace only prompt_cache_key",
			req: Request{
				Payload: []byte(`{"prompt_cache_key": "   "}`),
				Headers: http.Header{},
			},
			opts:         Options{},
			wantSource:   "",
			wantNonEmpty: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 对于需要 account 的测试，创建一个模拟 account
			account := tt.account
			if account == nil && tt.wantSource == "auth_id" {
				// 只有明确测试 auth_id 分支时才创建 account
				account = &auth.Account{}
			}

			result := ResolveContinuity(tt.ctx, account, tt.req, tt.opts)

			if result.Source != tt.wantSource {
				t.Errorf("ResolveContinuity() Source = %v, want %v", result.Source, tt.wantSource)
			}

			if tt.wantNonEmpty && result.Key == "" {
				t.Errorf("ResolveContinuity() Key should not be empty")
			}

			if !tt.wantNonEmpty && result.Key != "" {
				t.Errorf("ResolveContinuity() Key should be empty, got %v", result.Key)
			}
		})
	}
}

func TestResolveContinuityWithAuthID(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// 测试带 Email 的 account 会触发 auth_id 分支
	t.Run("account with email", func(t *testing.T) {
		account := &auth.Account{
			Email: "test@example.com",
		}
		req := Request{
			Payload: []byte(`{}`),
			Headers: http.Header{},
		}
		opts := Options{}

		result := ResolveContinuity(context.Background(), account, req, opts)
		if result.Source != "auth_id" {
			t.Errorf("Expected source 'auth_id', got %v", result.Source)
		}
		if result.Key == "" {
			t.Error("Expected non-empty key for account with email")
		}
	})

	// 测试带 DBID 的 account 会触发 auth_id 分支
	t.Run("account with DBID", func(t *testing.T) {
		// 由于 DBID 是私有字段，通过创建 Account 后内部设置
		account := &auth.Account{}
		// 使用反射或直接设置字段（如果可能）或通过 store 创建
		// 这里我们使用 Email 为空，DBID 需要 > 0 才能生成 auth_id
		// 由于无法直接设置私有字段 DBID，这个测试验证空 Email 且 DBID=0 时不生成 key
		req := Request{
			Payload: []byte(`{}`),
			Headers: http.Header{},
		}
		opts := Options{}

		result := ResolveContinuity(context.Background(), account, req, opts)
		// 空 account 应该返回空
		if result.Key != "" {
			t.Errorf("Expected empty key for empty account, got %v", result.Key)
		}
	})

	// 测试 nil account
	t.Run("nil account", func(t *testing.T) {
		req := Request{
			Payload: []byte(`{}`),
			Headers: http.Header{},
		}
		opts := Options{}

		result := ResolveContinuity(context.Background(), nil, req, opts)
		if result.Key != "" {
			t.Errorf("Expected empty key for nil account, got %v", result.Key)
		}
	})
}

func TestApplyContinuityBody(t *testing.T) {
	tests := []struct {
		name         string
		body         []byte
		continuity   Continuity
		wantContains string
	}{
		{
			name:         "apply continuity with key",
			body:         []byte(`{"model": "gpt-4"}`),
			continuity:   Continuity{Key: "test-key-123", Source: "prompt_cache_key"},
			wantContains: `"prompt_cache_key":"test-key-123"`,
		},
		{
			name:         "empty continuity key",
			body:         []byte(`{"model": "gpt-4"}`),
			continuity:   Continuity{Key: "", Source: ""},
			wantContains: `{"model": "gpt-4"}`,
		},
		{
			name:         "override existing prompt_cache_key",
			body:         []byte(`{"prompt_cache_key": "old-key", "model": "gpt-4"}`),
			continuity:   Continuity{Key: "new-key-456", Source: "execution_session"},
			wantContains: `"prompt_cache_key": "new-key-456"`,
		},
		{
			name:         "empty body",
			body:         []byte(`{}`),
			continuity:   Continuity{Key: "test-key", Source: "test"},
			wantContains: `"prompt_cache_key":"test-key"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ApplyContinuityBody(tt.body, tt.continuity)
			gotKey := gjson.GetBytes(result, "prompt_cache_key").String()
			wantKey := gjson.GetBytes([]byte(tt.wantContains), "prompt_cache_key").String()

			// 如果 wantContains 包含 prompt_cache_key，验证其值
			if wantKey != "" {
				if gotKey != wantKey {
					t.Errorf("ApplyContinuityBody() prompt_cache_key = %v, want %v", gotKey, wantKey)
				}
				return
			}

			// 否则检查原始 body 是否保持不变
			if !strings.Contains(string(result), tt.wantContains) {
				t.Errorf("ApplyContinuityBody() = %v, want to contain %v", string(result), tt.wantContains)
			}
		})
	}
}

func TestApplyContinuityHeaders(t *testing.T) {
	tests := []struct {
		name       string
		headers    http.Header
		continuity Continuity
		wantValue  string
	}{
		{
			name:       "apply session_id header",
			headers:    http.Header{},
			continuity: Continuity{Key: "session-123", Source: "prompt_cache_key"},
			wantValue:  "session-123",
		},
		{
			name:       "empty continuity key",
			headers:    http.Header{},
			continuity: Continuity{Key: "", Source: ""},
			wantValue:  "",
		},
		{
			name:       "nil headers",
			headers:    nil,
			continuity: Continuity{Key: "session-456", Source: "test"},
			wantValue:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ApplyContinuityHeaders(tt.headers, tt.continuity)

			if tt.headers == nil {
				return // nil headers should not panic
			}

			got := tt.headers.Get("session_id")
			if got != tt.wantValue {
				t.Errorf("ApplyContinuityHeaders() session_id = %v, want %v", got, tt.wantValue)
			}
		})
	}
}

func TestContinuityHelpers(t *testing.T) {
	t.Run("GetSessionID", func(t *testing.T) {
		c := Continuity{Key: "test-session", Source: "test"}
		if c.GetSessionID() != "test-session" {
			t.Errorf("GetSessionID() = %v, want %v", c.GetSessionID(), "test-session")
		}
	})

	t.Run("IsEmpty", func(t *testing.T) {
		empty := Continuity{}
		if !empty.IsEmpty() {
			t.Error("IsEmpty() should return true for empty continuity")
		}

		nonEmpty := Continuity{Key: "key", Source: "source"}
		if nonEmpty.IsEmpty() {
			t.Error("IsEmpty() should return false for non-empty continuity")
		}
	})
}

func TestMetadataString(t *testing.T) {
	tests := []struct {
		name string
		meta map[string]any
		key  string
		want string
	}{
		{
			name: "string value",
			meta: map[string]any{"key": "value"},
			key:  "key",
			want: "value",
		},
		{
			name: "byte slice value",
			meta: map[string]any{"key": []byte("value")},
			key:  "key",
			want: "value",
		},
		{
			name: "whitespace trimmed",
			meta: map[string]any{"key": "  value  "},
			key:  "key",
			want: "value",
		},
		{
			name: "missing key",
			meta: map[string]any{"other": "value"},
			key:  "key",
			want: "",
		},
		{
			name: "nil meta",
			meta: nil,
			key:  "key",
			want: "",
		},
		{
			name: "nil value",
			meta: map[string]any{"key": nil},
			key:  "key",
			want: "",
		},
		{
			name: "int value (ignored)",
			meta: map[string]any{"key": 123},
			key:  "key",
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := metadataString(tt.meta, tt.key)
			if got != tt.want {
				t.Errorf("metadataString() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPrincipalString(t *testing.T) {
	tests := []struct {
		name string
		raw  any
		want string
	}{
		{
			name: "string",
			raw:  "value",
			want: "value",
		},
		{
			name: "string with whitespace",
			raw:  "  value  ",
			want: "value",
		},
		{
			name: "int",
			raw:  123,
			want: "123",
		},
		{
			name: "nil",
			raw:  nil,
			want: "<nil>",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := principalString(tt.raw)
			if got != tt.want {
				t.Errorf("principalString() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestUUIDGeneration(t *testing.T) {
	// 测试 UUID 生成是确定性的
	t.Run("consistent UUID generation", func(t *testing.T) {
		input := "test-input-123"
		uuid1 := uuid.NewSHA1(uuid.NameSpaceOID, []byte("codex2api:prompt-cache:"+input))
		uuid2 := uuid.NewSHA1(uuid.NameSpaceOID, []byte("codex2api:prompt-cache:"+input))

		if uuid1.String() != uuid2.String() {
			t.Error("UUID generation should be deterministic")
		}
	})

	t.Run("different inputs produce different UUIDs", func(t *testing.T) {
		input1 := "input-1"
		input2 := "input-2"
		uuid1 := uuid.NewSHA1(uuid.NameSpaceOID, []byte("codex2api:prompt-cache:"+input1))
		uuid2 := uuid.NewSHA1(uuid.NameSpaceOID, []byte("codex2api:prompt-cache:"+input2))

		if uuid1.String() == uuid2.String() {
			t.Error("Different inputs should produce different UUIDs")
		}
	})
}

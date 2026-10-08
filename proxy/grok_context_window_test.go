package proxy

import (
	"net/http"
	"testing"

	"github.com/codex2api/auth"
)

func TestParseGrokPositiveHeader(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  int64
	}{
		{"正常值", "500000", 500000},
		{"带空白", "  500000  ", 500000},
		{"缺失", "", 0},
		{"零", "0", 0},
		{"负数", "-1", 0},
		{"非数字", "many", 0},
		{"小数", "5e5", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			header := http.Header{}
			if c.value != "" {
				header.Set("x-grok-context-window", c.value)
			}
			if got := parseGrokPositiveHeader(header, "x-grok-context-window"); got != c.want {
				t.Fatalf("= %d, want %d", got, c.want)
			}
		})
	}
}

// TestGrokCompactionAtDerivation 守护关键性质：观测到实抓的 500k 窗口时，
// 推导结果必须与硬编码默认值一致（行为不变）；只有上游改窗口才跟随变化。
func TestGrokCompactionAtDerivation(t *testing.T) {
	if grokCompactionAtOverride != "" {
		t.Skip("GROK_COMPACTION_AT 已被环境变量覆盖")
	}
	account := &auth.Account{}

	if got := grokCompactionAtForAccount(account); got != grokCompactionAtDefault {
		t.Fatalf("未观测到窗口时应回落默认值，got %q want %q", got, grokCompactionAtDefault)
	}

	// cli-chat-proxy 对 grok-4.7-build-fast 固定回 256000，但官方 CLI 仍发 400000。
	// 比默认阈值更小的观测不能把 x-compaction-at 压下去。
	account.SetGrokContextWindow(256000)
	if got := grokCompactionAtForAccount(account); got != grokCompactionAtDefault {
		t.Fatalf("256k 窗口不应把压缩阈值压到默认值以下，got %q", got)
	}

	// 实抓值：context_window=500000、auto_compact_threshold_percent=80 → 400000。
	account.SetGrokContextWindow(500000)
	if got := grokCompactionAtForAccount(account); got != "400000" {
		t.Fatalf("500k 窗口应推出 400000（与默认值一致），got %q", got)
	}
	if got := grokCompactionAtForAccount(account); got != grokCompactionAtDefault {
		t.Fatalf("500k 窗口的推导结果应与默认值相同，got %q want %q", got, grokCompactionAtDefault)
	}

	// 上游改窗口后自动跟上。
	account.SetGrokContextWindow(1_000_000)
	if got := grokCompactionAtForAccount(account); got != "800000" {
		t.Fatalf("1M 窗口应推出 800000，got %q", got)
	}

	// 非法观测不覆盖既有值。
	account.SetGrokContextWindow(0)
	account.SetGrokContextWindow(-5)
	if got := grokCompactionAtForAccount(account); got != "800000" {
		t.Fatalf("非法观测不应覆盖既有窗口，got %q", got)
	}
}

// TestRecordGrokUpstreamObservationsIndependent 配额头与上下文窗口互不依赖：
// 只带一种头的响应也要被正确采集。
func TestRecordGrokUpstreamObservationsIndependent(t *testing.T) {
	t.Run("只有上下文窗口", func(t *testing.T) {
		account := &auth.Account{}
		header := http.Header{}
		header.Set("x-grok-context-window", "500000")
		recordGrokUpstreamObservations(account, header)

		if got := account.GetGrokContextWindow(); got != 500000 {
			t.Fatalf("上下文窗口 = %d, want 500000", got)
		}
		if _, ok := account.GetGrokRateLimitSnapshot(); ok {
			t.Fatalf("没有配额头时不应写入配额快照")
		}
	})

	t.Run("只有配额头", func(t *testing.T) {
		account := &auth.Account{}
		header := http.Header{}
		header.Set("x-ratelimit-limit-tokens", "53000000")
		header.Set("x-ratelimit-remaining-tokens", "52610399")
		header.Set("x-ratelimit-limit-requests", "8300")
		header.Set("x-ratelimit-remaining-requests", "8298")
		recordGrokUpstreamObservations(account, header)

		snap, ok := account.GetGrokRateLimitSnapshot()
		if !ok {
			t.Fatalf("配额快照未写入")
		}
		if snap.RemainingTokens != 52610399 || snap.RemainingRequests != 8298 {
			t.Fatalf("配额快照不符: %+v", snap)
		}
		if got := account.GetGrokContextWindow(); got != 0 {
			t.Fatalf("没有窗口头时应保持未观测，got %d", got)
		}
	})

	t.Run("两者都有", func(t *testing.T) {
		account := &auth.Account{}
		header := http.Header{}
		header.Set("x-grok-context-window", "500000")
		header.Set("x-ratelimit-limit-tokens", "53000000")
		header.Set("x-ratelimit-remaining-tokens", "52610399")
		header.Set("x-ratelimit-limit-requests", "8300")
		header.Set("x-ratelimit-remaining-requests", "8298")
		recordGrokUpstreamObservations(account, header)

		if got := account.GetGrokContextWindow(); got != 500000 {
			t.Fatalf("上下文窗口 = %d, want 500000", got)
		}
		if _, ok := account.GetGrokRateLimitSnapshot(); !ok {
			t.Fatalf("配额快照未写入")
		}
	})

	t.Run("空账号与空头不 panic", func(t *testing.T) {
		recordGrokUpstreamObservations(nil, http.Header{})
		recordGrokUpstreamObservations(&auth.Account{}, nil)
	})
}

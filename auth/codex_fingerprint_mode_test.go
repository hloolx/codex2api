package auth

import "testing"

func TestNormalizeCodexFingerprintMode(t *testing.T) {
	cases := map[string]string{
		"":                              CodexFingerprintModeOff,
		"off":                           CodexFingerprintModeOff,
		"unknown":                       CodexFingerprintModeOff,
		"DEVICE":                        CodexFingerprintModeDevice,
		" session ":                     CodexFingerprintModeSession,
		"Full":                          CodexFingerprintModeFull,
		" Single_Machine_Multi_Window ": CodexFingerprintModeSingleMachineMultiWindow,
	}
	for input, want := range cases {
		if got := NormalizeCodexFingerprintMode(input); got != want {
			t.Errorf("NormalizeCodexFingerprintMode(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestIsValidCodexFingerprintMode(t *testing.T) {
	for _, value := range []string{CodexFingerprintModeOff, CodexFingerprintModeDevice, CodexFingerprintModeSession, CodexFingerprintModeFull, CodexFingerprintModeSingleMachineMultiWindow, " FULL "} {
		if !IsValidCodexFingerprintMode(value) {
			t.Errorf("IsValidCodexFingerprintMode(%q) = false, want true", value)
		}
	}
	for _, value := range []string{"", "converge", "device-only"} {
		if IsValidCodexFingerprintMode(value) {
			t.Errorf("IsValidCodexFingerprintMode(%q) = true, want false", value)
		}
	}
}

func TestEffectiveCodexFingerprintMode(t *testing.T) {
	if got := (*Account)(nil).EffectiveCodexFingerprintMode(); got != CodexFingerprintModeOff {
		t.Errorf("nil account mode = %q, want %q", got, CodexFingerprintModeOff)
	}

	// 未配置的既有账号必须保持 off，升级不改变出站行为。
	if got := (&Account{DBID: 1}).EffectiveCodexFingerprintMode(); got != CodexFingerprintModeOff {
		t.Errorf("unconfigured account mode = %q, want %q", got, CodexFingerprintModeOff)
	}

	codex := &Account{DBID: 1, CodexFingerprintMode: CodexFingerprintModeSession}
	if got := codex.EffectiveCodexFingerprintMode(); got != CodexFingerprintModeSession {
		t.Errorf("codex account mode = %q, want %q", got, CodexFingerprintModeSession)
	}

	relay := &Account{
		DBID:                 1,
		UpstreamType:         UpstreamOpenAIResponses,
		BaseURL:              "https://relay.example.com",
		APIKey:               "sk-relay",
		CodexFingerprintMode: CodexFingerprintModeSession,
	}
	if got := relay.EffectiveCodexFingerprintMode(); got != CodexFingerprintModeOff {
		t.Errorf("relay account mode = %q, want %q (relays do not use the Codex outbound path)", got, CodexFingerprintModeOff)
	}

	// Grok 账号有独立的上游执行器，Codex 指纹收敛对它无意义，即使凭据里配了档位也必须为 off。
	grokAPIKey := &Account{
		DBID:                 1,
		UpstreamType:         UpstreamGrok,
		APIKey:               "xai-key",
		CodexFingerprintMode: CodexFingerprintModeFull,
	}
	if got := grokAPIKey.EffectiveCodexFingerprintMode(); got != CodexFingerprintModeOff {
		t.Errorf("grok api-key account mode = %q, want %q", got, CodexFingerprintModeOff)
	}
	grokOAuth := &Account{
		DBID:                 1,
		UpstreamType:         UpstreamGrok,
		RefreshToken:         "grok-rt",
		CodexFingerprintMode: CodexFingerprintModeSession,
	}
	if got := grokOAuth.EffectiveCodexFingerprintMode(); got != CodexFingerprintModeOff {
		t.Errorf("grok oauth account mode = %q, want %q", got, CodexFingerprintModeOff)
	}
}

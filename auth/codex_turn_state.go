package auth

import (
	"fmt"
	"strings"
	"time"
)

// Credential-level X-Codex-Turn-State injection overrides client echoes and
// custom headers on HTTP and WebSocket requests. It does not change account
// identity or scheduling; the echo guard still handles cross-account reuse.
const (
	CodexTurnStateCredentialKey       = "codex_turn_state"
	CodexTurnStateModelsCredentialKey = "codex_turn_state_models"
	// CodexTurnStateSetAtCredentialKey stores the last value-change timestamp.
	// Scope-only edits keep the existing countdown origin.
	CodexTurnStateSetAtCredentialKey = "codex_turn_state_set_at"

	// Observed values are roughly 300 bytes; reject oversized values intact.
	maxCodexTurnStateBytes       = 4096
	maxCodexTurnStateModelsBytes = 1024
)

// ValidateCodexTurnState accepts visible single-line ASCII header values.
// Reject oversized values instead of truncating an upstream-issued token.
func ValidateCodexTurnState(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	if len(value) > maxCodexTurnStateBytes {
		return fmt.Errorf("codex_turn_state 长度不能超过 %d 字节", maxCodexTurnStateBytes)
	}
	for i := 0; i < len(value); i++ {
		if value[i] < 0x20 || value[i] > 0x7e {
			return fmt.Errorf("codex_turn_state 只能包含单行 ASCII 可见字符")
		}
	}
	return nil
}

// NormalizeCodexTurnStateModels normalizes a comma-separated model scope.
// An empty scope allows every model.
func NormalizeCodexTurnStateModels(value string) string {
	seen := make(map[string]struct{})
	entries := make([]string, 0, 4)
	for _, entry := range strings.Split(value, ",") {
		entry = strings.ToLower(strings.TrimSpace(entry))
		if entry == "" {
			continue
		}
		if _, dup := seen[entry]; dup {
			continue
		}
		seen[entry] = struct{}{}
		entries = append(entries, entry)
	}
	return strings.Join(entries, ", ")
}

func ValidateCodexTurnStateModels(value string) error {
	if len(value) > maxCodexTurnStateModelsBytes {
		return fmt.Errorf("codex_turn_state_models 长度不能超过 %d 字节", maxCodexTurnStateModelsBytes)
	}
	for i := 0; i < len(value); i++ {
		if value[i] < 0x20 || value[i] > 0x7e {
			return fmt.Errorf("codex_turn_state_models 只能包含 ASCII 可见字符")
		}
	}
	return nil
}

// CodexTurnStateModelsMatch matches client or upstream model names without
// case sensitivity. A trailing asterisk matches a prefix. Empty scopes or
// requests without model names do not restrict injection.
func CodexTurnStateModelsMatch(scope string, models ...string) bool {
	entries := make([]string, 0, 4)
	for _, entry := range strings.Split(scope, ",") {
		if entry = strings.TrimSpace(entry); entry != "" {
			entries = append(entries, entry)
		}
	}
	if len(entries) == 0 {
		return true
	}
	known := false
	for _, model := range models {
		if strings.TrimSpace(model) != "" {
			known = true
			break
		}
	}
	if !known {
		return true
	}
	for _, entry := range entries {
		prefix, wildcard := strings.CutSuffix(entry, "*")
		for _, model := range models {
			model = strings.TrimSpace(model)
			if model == "" {
				continue
			}
			if wildcard && strings.HasPrefix(strings.ToLower(model), strings.ToLower(prefix)) {
				return true
			}
			if !wildcard && strings.EqualFold(model, entry) {
				return true
			}
		}
	}
	return false
}

// ParseCodexTurnStateSetAt returns zero for empty or invalid timestamps.
func ParseCodexTurnStateSetAt(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	if ts, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return ts
	}
	if ts, err := time.Parse(time.RFC3339, raw); err == nil {
		return ts
	}
	return time.Time{}
}

// CodexTurnStateInjection returns the scoped value shared by dispatch and audit.
func (a *Account) CodexTurnStateInjection(models ...string) string {
	if a == nil {
		return ""
	}
	a.mu.RLock()
	value, scope := a.CodexTurnState, a.CodexTurnStateModels
	a.mu.RUnlock()
	value = strings.TrimSpace(value)
	if value == "" || !CodexTurnStateModelsMatch(scope, models...) {
		return ""
	}
	return value
}

// CodexTurnStateConfig returns a snapshot of the value, scope and timestamp.
func (a *Account) CodexTurnStateConfig() (value, models string, setAt time.Time) {
	if a == nil {
		return "", "", time.Time{}
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.CodexTurnState, a.CodexTurnStateModels, a.CodexTurnStateSetAt
}

func (a *Account) setCodexTurnStateFromRowLocked(row interface {
	GetCredential(string) string
}) {
	a.CodexTurnState = strings.TrimSpace(row.GetCredential(CodexTurnStateCredentialKey))
	a.CodexTurnStateModels = NormalizeCodexTurnStateModels(row.GetCredential(CodexTurnStateModelsCredentialKey))
	a.CodexTurnStateSetAt = ParseCodexTurnStateSetAt(row.GetCredential(CodexTurnStateSetAtCredentialKey))
}

// ApplyAccountCodexTurnState publishes saved injection settings to runtime.
func (s *Store) ApplyAccountCodexTurnState(id int64, value, models string, setAt time.Time) {
	if a := s.FindByID(id); a != nil {
		a.mu.Lock()
		a.CodexTurnState = strings.TrimSpace(value)
		a.CodexTurnStateModels = NormalizeCodexTurnStateModels(models)
		a.CodexTurnStateSetAt = setAt
		a.mu.Unlock()
	}
}

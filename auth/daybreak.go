package auth

import (
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/codex2api/database"
)

const DaybreakBlue = "daybreak_blue"
const DaybreakRed = "daybreak_red"

var daybreakObservationClock atomic.Int64

// 单调版本避免系统时钟回拨或同一时钟刻度内的结果互相覆盖。
func (a *Account) BeginDaybreakObservation() database.DaybreakSnapshot {
	a.mu.RLock()
	defer a.mu.RUnlock()
	for {
		previous := daybreakObservationClock.Load()
		next := max(time.Now().UnixNano(), previous+1, a.daybreak.ObservedAt+1)
		if daybreakObservationClock.CompareAndSwap(previous, next) {
			return database.DaybreakSnapshot{Identity: a.daybreakIdentityLocked(), ObservedAt: next}
		}
	}
}

func DaybreakAlias(model, program string) string {
	switch program {
	case DaybreakBlue:
		return model + "-daybreak-blue"
	case DaybreakRed:
		return model + "-daybreak-red"
	default:
		return ""
	}
}

func ParseDaybreakAlias(model string) (string, string) {
	model = strings.ToLower(strings.TrimSpace(model))
	for _, program := range []string{DaybreakBlue, DaybreakRed} {
		suffix := DaybreakAlias("", program)
		if strings.HasSuffix(model, suffix) {
			return strings.TrimSuffix(model, suffix), program
		}
	}
	return model, ""
}

func (a *Account) daybreakIdentityLocked() string {
	return database.DaybreakIdentityOf(database.DaybreakPrincipal{Token: a.AccessToken, Workspace: a.AccountID, Email: a.Email, Provider: a.UpstreamType, Headers: a.CustomHeaders})
}

func (a *Account) DaybreakIdentity() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.daybreakIdentityLocked()
}

func (a *Account) ApplyDaybreakSnapshot(snapshot database.DaybreakSnapshot) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if snapshot.Identity != a.daybreakIdentityLocked() || snapshot.ObservedAt < a.daybreak.ObservedAt {
		return
	}
	a.daybreak = snapshot
}

func (a *Account) DaybreakSnapshot() database.DaybreakSnapshot {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.daybreak.Identity != a.daybreakIdentityLocked() {
		a.daybreak = database.DaybreakSnapshot{}
		return database.DaybreakSnapshot{}
	}
	copy := a.daybreak
	copy.Models = make(map[string][]string, len(a.daybreak.Models))
	for model, programs := range a.daybreak.Models {
		copy.Models[model] = append([]string(nil), programs...)
	}
	return copy
}

func (a *Account) SupportsDaybreak(model, program string) bool {
	if a == nil || a.IsRelayStyle() || a.IsAntigravityAPI() || a.IsClaudeOAuth() || a.IsGrokAPI() {
		return false
	}
	for _, available := range a.DaybreakSnapshot().Models[strings.ToLower(strings.TrimSpace(model))] {
		if program == available {
			return a.SupportsCodexModel(model)
		}
	}
	return false
}

func (a *Account) DaybreakAliases() []string {
	if a == nil || a.IsRelayStyle() || a.IsAntigravityAPI() || a.IsClaudeOAuth() || a.IsGrokAPI() {
		return nil
	}
	var aliases []string
	for model, programs := range a.DaybreakSnapshot().Models {
		if !a.SupportsCodexModel(model) {
			continue
		}
		for _, program := range programs {
			aliases = append(aliases, DaybreakAlias(model, program))
		}
	}
	sort.Strings(aliases)
	return aliases
}

// Daybreak 目录只检查持久禁用状态，暂时无额度仍保留模型名称。
func (a *Account) DaybreakCatalogEligible() bool {
	if a == nil || atomic.LoadInt32(&a.Disabled) != 0 || atomic.LoadInt32(&a.DispatchPaused) != 0 {
		return false
	}
	if len(a.DaybreakAliases()) == 0 {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.Status != StatusError && a.healthTierLocked() != HealthTierBanned && a.hasDispatchCredentialLocked()
}

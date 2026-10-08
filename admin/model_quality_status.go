package admin

import (
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

// Verdicts describe benchmark answers; execution describes why work is waiting.
type modelQualityStateView struct {
	database.ModelQualityState
	Execution      string `json:"execution"`
	AccountStatus  string `json:"account_status,omitempty"`
	CooldownReason string `json:"cooldown_reason,omitempty"`
	ResumeAt       int64  `json:"resume_at,omitempty"`
	Occupied       int64  `json:"occupied"`
	Capacity       int64  `json:"capacity"`
	CanRetest      bool   `json:"can_retest"`
}

func (h *Handler) modelQualityView(a *auth.Account, state database.ModelQualityState, enabled, accountRunning bool, now time.Time) modelQualityStateView {
	v := modelQualityStateView{ModelQualityState: state, AccountStatus: a.RuntimeStatus()}
	v.Occupied, v.Capacity = h.store.ModelQualityCapacity(a)
	for _, cd := range a.ActiveModelCooldowns() {
		if cd.Model == state.Model {
			v.CooldownReason, v.ResumeAt = cd.Reason, cd.ResetAt.Unix()
			break
		}
	}
	a.Mu().RLock()
	if a.CooldownUtil.After(now) && a.CooldownUtil.Unix() > v.ResumeAt {
		v.ResumeAt = a.CooldownUtil.Unix()
	}
	a.Mu().RUnlock()
	v.CanRetest = enabled && a.SupportsCodexModel(state.Model) && !state.Running
	switch {
	case !enabled:
		v.Execution = "disabled"
	case state.Running:
		v.Execution = "running"
	case !a.SupportsCodexModel(state.Model):
		v.Execution = "unsupported"
	case !a.IsAvailable():
		v.Execution = "account_unavailable"
	case v.CooldownReason != "":
		v.Execution = "model_cooldown"
	case state.Generation == qualityAccountGeneration(a) && state.NextCheckAt > now.Unix():
		v.Execution = "scheduled"
	case accountRunning:
		v.Execution = "waiting_account"
	case v.Capacity <= 0 || v.Occupied >= v.Capacity:
		v.Execution = "waiting_capacity"
	default:
		v.Execution = "queued"
	}
	return v
}

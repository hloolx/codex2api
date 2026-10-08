package proxy

import (
	"context"
	"fmt"
	"log"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// 最后一跳在 payload 规则之后固定程序，并再次验证账号与 Key 权限。
func guardDaybreakUpstream(ctx context.Context, account *auth.Account, body []byte) ([]byte, error) {
	guard, _ := ctx.Value(daybreakContextKey{}).(daybreakGuard)
	request := guard.Request
	if request.Program != "" {
		body, _ = sjson.SetBytes(body, "model", request.Model)
		body, _ = sjson.SetBytes(body, "access_programs.cyber", request.Program)
	}
	model := gjson.GetBytes(body, "model").String()
	program := gjson.GetBytes(body, "access_programs.cyber").String()
	if program != auth.DaybreakBlue && program != auth.DaybreakRed {
		return body, nil
	}
	if !account.SupportsDaybreak(model, program) {
		return nil, fmt.Errorf("account does not support requested Daybreak model")
	}
	if msg := checkAPIKeyModel(auth.DaybreakAlias(model, program), guard.Limits); msg != "" {
		return nil, fmt.Errorf("%s", msg)
	}
	log.Printf("[daybreak] account=%d requested=%q model=%q program=%s", account.ID(), request.Requested, model, program)
	return body, nil
}

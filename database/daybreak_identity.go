package database

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/codex2api/internal/openaiidentity"
)

type DaybreakPrincipal struct {
	Token     string
	Workspace string
	Email     string
	Provider  string
	Headers   map[string]string
}

// 权限绑定主体与工作区；正常 AT/RT 轮换不改变主体，未知主体保守绑定令牌。
func DaybreakIdentityOf(principal DaybreakPrincipal) string {
	subject := daybreakTokenSubject(principal.Token)
	if subject == "" {
		subject = strings.ToLower(strings.TrimSpace(principal.Email))
	}
	if subject == "" {
		subject = principal.Token
	}
	workspace := strings.TrimSpace(principal.Workspace)
	if override := openaiidentity.WorkspaceOverrideFromHeaders(principal.Headers); override != "" {
		workspace = override
	}
	provider := strings.ToLower(strings.TrimSpace(principal.Provider))
	if provider == "" {
		provider = "codex"
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(provider+"\x00"+subject+"\x00"+workspace)))
}

func daybreakTokenSubject(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Subject string `json:"sub"`
		Auth    struct {
			UserID string `json:"chatgpt_user_id"`
		} `json:"https://api.openai.com/auth"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return ""
	}
	if claims.Auth.UserID != "" {
		return claims.Auth.UserID
	}
	return claims.Subject
}

func daybreakRowIdentity(raw any) string {
	row := AccountRow{Credentials: decodeCredentials(raw)}
	return DaybreakIdentityOf(DaybreakPrincipal{Token: row.GetCredential("access_token"), Workspace: row.GetCredential("account_id"), Email: row.GetCredential("email"), Provider: row.GetCredential("upstream_type"), Headers: row.GetCredentialStringMap("custom_headers")})
}

package proxy

import (
	"errors"
	"log"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/codex2api/auth"
)

type CodexClientIdentityInput struct {
	Account      *auth.Account
	APIKey       string
	DeviceConfig *DeviceProfileConfig
	Headers      http.Header
}

type CodexOutboundClientIdentity struct {
	UserAgent string
	Version   string
	Generated bool
}

// ResolveCodexOutboundClientIdentity 将版本不可用错误传到出站前的调用者。
func ResolveCodexOutboundClientIdentity(input CodexClientIdentityInput) (CodexOutboundClientIdentity, error) {
	if IsDeviceProfileStabilizationEnabled(input.DeviceConfig) {
		return codexDeviceClientIdentity(input), nil
	}
	userAgent := strings.TrimSpace(input.Headers.Get("User-Agent"))
	originator := strings.TrimSpace(input.Headers.Get("Originator"))
	settings := CurrentRuntimeSettings()
	if shouldGenerateCodexClientHeaders(settings, userAgent, originator) {
		ua, version, err := generatedCodexClientHeadersChecked(input.Account, settings)
		return CodexOutboundClientIdentity{UserAgent: ua, Version: version, Generated: true}, err
	}
	if IsCodexOfficialClientByHeaders(userAgent, originator) && userAgent != "" {
		version := firstNonEmptyHeader(input.Headers, "Version", codexVersionFromUserAgent(userAgent, latestCodexCLIVersion))
		return CodexOutboundClientIdentity{UserAgent: userAgent, Version: version}, nil
	}
	accountID := int64(0)
	if input.Account != nil {
		accountID = input.Account.ID()
	}
	floor := codexIdentityVersionFloor(settings)
	ua, version, configured, err := codexUserAgentFromConfigChecked(settings.CodexUserAgentConfig, accountID, floor)
	if err != nil || configured {
		return CodexOutboundClientIdentity{UserAgent: ua, Version: version, Generated: true}, err
	}
	version = effectiveLatestCodexCLIVersion()
	if !codexVersionAtLeast(version, floor) {
		return CodexOutboundClientIdentity{}, codexClientVersionUnavailable("codex-cli", "", floor)
	}
	return CodexOutboundClientIdentity{UserAgent: replaceCodexUserAgentVersion(defaultCodexCLIUserAgent, version), Version: version}, nil
}

func codexIdentityVersionFloor(settings RuntimeSettings) string {
	if settings.ClientCompatMode == ClientCompatModeAuto {
		return settings.CodexMinCLIVersion
	}
	return ""
}

func codexDeviceClientIdentity(input CodexClientIdentityInput) CodexOutboundClientIdentity {
	profile := ResolveDeviceProfile(input.Account, input.APIKey, input.Headers, input.DeviceConfig)
	return CodexOutboundClientIdentity{UserAgent: firstNonEmptyString(profile.UserAgent, defaultCodexCLIUserAgent), Version: codexVersionFromProfile(profile, input.DeviceConfig.PackageVersion)}
}

func generatedCodexClientHeadersChecked(account *auth.Account, settings RuntimeSettings) (string, string, error) {
	floor := codexIdentityVersionFloor(settings)
	accountID := int64(0)
	if account != nil {
		accountID = account.ID()
	}
	ua, version, configured, err := codexUserAgentFromConfigChecked(settings.CodexUserAgentConfig, accountID, floor)
	if err != nil || configured {
		return ua, version, err
	}
	profile := ProfileForAccount(accountID)
	version = effectiveLatestCodexCLIVersion()
	if !codexVersionAtLeast(version, floor) {
		return "", "", codexClientVersionUnavailable("codex-cli", "", floor)
	}
	ua = firstNonEmptyString(profile.UserAgent, defaultCodexCLIUserAgent)
	return replaceCodexUserAgentVersion(ua, version), version, nil
}

// CodexMaintenanceIdentity 是网关自发的 Codex 维护请求（wham 用量/重置券、每日用量、
// token 明细、订阅同步、模型清单、中转模型发现）携带的客户端身份。
type CodexMaintenanceIdentity struct {
	UserAgent  string
	Originator string
	Version    string
	// Unified 表示身份来自统一身份开关下的配置解析；false 为内置 codex-tui 身份
	// （开关关闭，或配置解析失败后的回退）。
	Unified bool
}

// Apply 写入维护请求的身份头。Version 只由已经发送它的调用方自行写入：
// 维护请求不凭空补发真实客户端不会带的头。
func (id CodexMaintenanceIdentity) Apply(headers http.Header) {
	if headers == nil {
		return
	}
	headers.Set("User-Agent", id.UserAgent)
	headers.Set("Originator", id.Originator)
}

const codexMaintenanceIdentityFallbackLogEvery = 10 * time.Minute

var codexMaintenanceIdentityFallbackLastLog atomic.Int64

// ResolveCodexMaintenanceIdentity 返回网关自发维护请求使用的客户端身份。
//
// 统一身份开关（codex_unified_client_identity_enabled）关闭时保持内置 codex-tui 身份。
// 开启后与对话出站走同一套解析：号池画像按账号抽取、Originator 跟随生成的 UA，最后
// 套用账号自定义头里的身份头，覆盖顺序与对话请求一致，使同一账号对上游只呈现一份
// 客户端身份（issue #774）。downstream 为空表示纯后台请求；模型清单透传时传入下游头，
// 透传官方客户端的规则也与对话一致。
//
// 配置的版本低于最低版本要求时回退内置身份并限频记日志：探针承担 401 识别、额度
// 刷新与自动暂停，不能因为身份配置问题停摆。
func ResolveCodexMaintenanceIdentity(account *auth.Account, downstream http.Header) CodexMaintenanceIdentity {
	builtin := CodexMaintenanceIdentity{
		UserAgent:  MinimalCodexCLIUserAgentForHeaders(),
		Originator: Originator,
		Version:    effectiveLatestCodexCLIVersion(),
	}
	if !CurrentRuntimeSettings().CodexUnifiedClientIdentityEnabled {
		return builtin
	}
	identity, err := ResolveCodexOutboundClientIdentity(CodexClientIdentityInput{Account: account, Headers: downstream})
	if err == nil && strings.TrimSpace(identity.UserAgent) == "" {
		err = errEmptyCodexMaintenanceUserAgent
	}
	if err != nil {
		logCodexMaintenanceIdentityFallback(account, err)
		return builtin
	}

	resolved := CodexMaintenanceIdentity{UserAgent: identity.UserAgent, Version: identity.Version, Unified: true}
	if identity.Generated {
		resolved.Originator = CodexOriginatorForGeneratedUserAgent(identity.UserAgent)
	} else if originator := strings.TrimSpace(downstream.Get("Originator")); originator != "" && IsCodexOfficialClientByHeaders("", originator) {
		resolved.Originator = originator
	} else {
		resolved.Originator = Originator
	}
	if resolved.Version == "" {
		resolved.Version = codexVersionFromUserAgent(resolved.UserAgent, effectiveLatestCodexCLIVersion())
	}
	// 账号自定义头在对话出站里最后应用（applyAccountCustomHeaders），钉死的身份头
	// 同样作用于维护请求，否则对话与探针又是两套身份。
	for name, value := range account.GetCustomHeaders() {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		switch http.CanonicalHeaderKey(strings.TrimSpace(name)) {
		case "User-Agent":
			resolved.UserAgent = value
		case "Originator":
			resolved.Originator = value
		case "Version":
			resolved.Version = value
		}
	}
	return resolved
}

var errEmptyCodexMaintenanceUserAgent = errors.New("resolved empty User-Agent")

func logCodexMaintenanceIdentityFallback(account *auth.Account, err error) {
	now := time.Now().UnixNano()
	last := codexMaintenanceIdentityFallbackLastLog.Load()
	if last != 0 && time.Duration(now-last) < codexMaintenanceIdentityFallbackLogEvery {
		return
	}
	if !codexMaintenanceIdentityFallbackLastLog.CompareAndSwap(last, now) {
		return
	}
	accountID := int64(0)
	if account != nil {
		accountID = account.ID()
	}
	log.Printf("[CodexIdentity] 统一身份解析失败，维护请求回退内置身份: account_id=%d err=%v", accountID, err)
}

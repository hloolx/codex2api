package proxy

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"github.com/codex2api/auth"
)

// ==================== Codex 出口链路统一解析 ====================
//
// Codex 渠道的出站有三套配置并存(issue #679):
//
//  1. Resin 反向代理(全局,系统设置 resin_url + resin_platform_name)
//  2. 代理池 / 分组代理 / 账号 proxy_url / 全局 proxy_url
//     (由 auth.Store.ResolveProxyForAccount 按 账号 > 分组 > 代理池 > 全局 选出一条)
//  3. 直连
//
// 优先级固定为 Resin > 代理 > 直连,且 Resin 是整层覆盖:一旦启用,Codex 渠道
// 所有携带账号身份的出站(/responses、compact、WS、wham 用量、订阅查询、遥测、
// 令牌刷新)全部改经 Resin,第 2 层选出的代理只保留在日志/审计里、不参与拨号。
// 显式模板刷新可通过 ResolveCodexRequestEgress 使用专属签发代理,不改变以上默认链路。
// Claude / Grok / Antigravity 等中继型账号不经 Resin,继续走第 2 层。
//
// 所有 Codex 出站选客户端的地方都必须经过本文件的解析器,禁止各自再写
// `if IsResinEnabled()` 分支——分支散落正是"三套互相覆盖、看不出谁在生效"的根源。

// CodexEgressKind 标识一次 Codex 出站最终走的是哪条链路。
type CodexEgressKind string

const (
	// CodexEgressResin 经 Resin 反代;URL 已改写,请求头需带 X-Resin-Account。
	CodexEgressResin CodexEgressKind = "resin"
	// CodexEgressProxy 经第 2 层选出的代理直达上游。
	CodexEgressProxy CodexEgressKind = "proxy"
	// CodexEgressDirect 无代理直连上游。
	CodexEgressDirect CodexEgressKind = "direct"
)

// CodexEgress 描述解析后的出口:最终请求地址、拨号用代理与客户端。
type CodexEgress struct {
	Kind CodexEgressKind
	// URL 是最终请求地址;Resin 模式下已改写为反代路径。
	URL string
	// ProxyURL 是第 2 层选出的代理。Resin 模式下保留原值用于审计,但拨号不使用。
	ProxyURL string
	// DialProxyURL 是实际参与拨号的代理:Resin 模式恒为空。
	DialProxyURL string

	account *auth.Account
	client  *http.Client
}

// ResolveCodexEgress 为一次携带账号身份的 Codex HTTP 出站决定链路。
// proxyURL 是第 2 层已经解析好的代理(可为空);Resin 启用时被整层覆盖。
// account 为 nil 时不可能走 Resin(Resin 按账号粘性,无身份无从粘),退回代理/直连。
func ResolveCodexEgress(account *auth.Account, targetURL, proxyURL string) CodexEgress {
	if proxyURL == ipv6StateDirectRoute {
		return CodexEgress{Kind: CodexEgressDirect, URL: targetURL, account: account}
	}
	proxyURL = strings.TrimSpace(proxyURL)
	if resinCarriesEgress(account) {
		return CodexEgress{
			Kind:     CodexEgressResin,
			URL:      BuildReverseProxyURL(targetURL),
			ProxyURL: proxyURL,
			account:  account,
			client:   getResinHTTPClient(account),
		}
	}
	kind := CodexEgressDirect
	if proxyURL != "" {
		kind = CodexEgressProxy
	}
	return CodexEgress{
		Kind:         kind,
		URL:          targetURL,
		ProxyURL:     proxyURL,
		DialProxyURL: proxyURL,
		account:      account,
	}
}

// resinCarriesEgress 判定该账号的出站是否由 Resin 承担:Resin 已启用、有账号身份
// (Resin 按账号粘性,无身份无从粘)且不是中继型账号(Claude/Grok/Antigravity 不经 Resin)。
// 与 auth.Store 的 fail-closed 放行、upstream_trace 的审计标签取同一口径。
func resinCarriesEgress(account *auth.Account) bool {
	return IsResinEnabled() && account != nil && !account.IsRelayStyle()
}

// ViaResin 报告本次出站是否经 Resin。
func (e CodexEgress) ViaResin() bool {
	return e.Kind == CodexEgressResin
}

// Client 返回本链路的 HTTP 客户端。Resin 模式复用按账号隔离的 Resin 连接池;
// 其余模式复用网关主池(uTLS + 代理)。account 为 nil 且非 Resin 时返回 nil,
// 由调用方按自己的兜底 transport 处理。
func (e CodexEgress) Client() *http.Client {
	if e.client != nil {
		return e.client
	}
	if e.account == nil {
		return nil
	}
	e.client = getPooledClient(e.account, e.DialProxyURL)
	return e.client
}

// ApplyHeaders 补齐链路要求的请求头:Resin 模式注入账号身份头,其余模式不改。
func (e CodexEgress) ApplyHeaders(h http.Header) {
	if h == nil || !e.ViaResin() || e.account == nil {
		return
	}
	h.Set("X-Resin-Account", ResinAccountID(e.account))
}

// ResolveCodexWebsocketEgress 为 Codex WS 出站决定链路:返回最终 WS 地址与
// 拨号用代理。Resin 模式下 WS 地址改写为 Resin 的 ws:// 反代路径、拨号不走代理;
// 账号身份头由调用方用 ApplyHeaders 注入。
func ResolveCodexWebsocketEgress(account *auth.Account, wsURL, proxyURL string) CodexEgress {
	if proxyURL == ipv6StateDirectRoute {
		return CodexEgress{Kind: CodexEgressDirect, URL: wsURL, account: account}
	}
	proxyURL = strings.TrimSpace(proxyURL)
	if resinCarriesEgress(account) {
		return CodexEgress{
			Kind:     CodexEgressResin,
			URL:      BuildWebSocketURL(wsURL),
			ProxyURL: proxyURL,
			account:  account,
		}
	}
	kind := CodexEgressDirect
	if proxyURL != "" {
		kind = CodexEgressProxy
	}
	return CodexEgress{
		Kind:         kind,
		URL:          wsURL,
		ProxyURL:     proxyURL,
		DialProxyURL: proxyURL,
		account:      account,
	}
}

// CodexDialProxyURL 返回拨号实际使用的代理:Resin 承担出站时恒为空(地址已是反代
// 路径,再套代理只会把 Resin 本身推到代理后面)。供只需要"拨号走不走代理"、
// 手里 URL 已经改写过的调用方(WS 连接建立)使用,避免对改写后的地址二次改写。
func CodexDialProxyURL(account *auth.Account, proxyURL string) string {
	if proxyURL == ipv6StateDirectRoute {
		return ""
	}
	if resinCarriesEgress(account) {
		return ""
	}
	return strings.TrimSpace(proxyURL)
}

// CodexEgressSummary 是给设置接口/日志用的全局出口摘要:只回答"Codex 渠道现在
// 由谁承担出站"。逐账号的代理选择仍由 auth.Store 决定,这里不展开。
type CodexEgressSummary struct {
	// Mode 是 resin 或 proxy_chain;proxy_chain 表示按 账号 > 分组 > 代理池 > 全局 > 直连 解析。
	Mode string `json:"mode"`
	// ResinEnabled 为 true 时 Codex 渠道的代理池/proxy_url 均不参与出站。
	ResinEnabled bool `json:"resin_enabled"`
	// ResinEndpoint 是打码后的 Resin 地址(去掉路径里的 token),仅用于展示。
	ResinEndpoint string `json:"resin_endpoint,omitempty"`
	// ResinPlatformName 是 Resin 侧平台标识。
	ResinPlatformName string `json:"resin_platform_name,omitempty"`
}

// CurrentCodexEgressSummary 返回当前生效的 Codex 出口摘要。
func CurrentCodexEgressSummary() CodexEgressSummary {
	cfg := GetResinConfig()
	if cfg == nil {
		return CodexEgressSummary{Mode: "proxy_chain"}
	}
	return CodexEgressSummary{
		Mode:              "resin",
		ResinEnabled:      true,
		ResinEndpoint:     MaskResinBaseURL(cfg.BaseURL),
		ResinPlatformName: cfg.PlatformName,
	}
}

// MaskResinBaseURL 把 Resin 基础地址里的 token(路径段)打码,只保留 scheme 与
// host:port,供日志与界面展示。resin_url 形如 http://host:port/<token>,整段
// 打印等于把凭据写进日志(issue #679)。
func MaskResinBaseURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "***"
	}
	masked := parsed.Scheme + "://" + parsed.Host
	if strings.Trim(parsed.Path, "/") != "" {
		masked += "/***"
	}
	return masked
}

// ResolveCodexRequestEgress picks the websocket or HTTP egress for one request.
func ResolveCodexRequestEgress(_ context.Context, account *auth.Account, targetURL, proxyURL string, websocket bool) CodexEgress {
	if websocket {
		return ResolveCodexWebsocketEgress(account, targetURL, proxyURL)
	}
	return ResolveCodexEgress(account, targetURL, proxyURL)
}

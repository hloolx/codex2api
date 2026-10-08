package proxy

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/codex2api/auth"
)

// ==================== 账号维护请求的 HTTP Client 池（issue #446） ====================
//
// 背景：wham 用量/重置券、Codex 模型清单、alpha search、订阅到期同步这些
// 「维护类」旁路请求，过去每次调用都 `&http.Client{Transport: newCodexTransport(...)}`
// 新建一次性 transport。标准 transport 下遗留连接靠 IdleConnTimeout 兜底（90s 回收）；
// 但 uTLS transport 自管连接池，一次性实例用完即被丢弃，其名下的 HTTP/2 连接
// 没有任何持有者去关闭，readLoop goroutine 与 socket 常驻到进程结束。
// 后台探针是周期任务、/models 又被 Codex 客户端高频请求，于是连接数、goroutine
// 与内存随运行时长线性增长，最终 OOM。
//
// 修复：维护请求复用按 (用途 + 账号 + 代理 + transport 模式) 隔离的 Client。
// 直接复用 clientPool，从而自动获得既有的 TTL 淘汰（clientPoolTTL）与
// releaseEvictedClient 的连接关闭逻辑，无需第二套清理协程。
//
// 为什么不直接共用 /responses 的池（getPooledClient）：
//   - 维护请求与业务请求的 UA/Originator 语义不同（订阅端点用浏览器 UA），
//     混在同一条 h2 连接上会改变上游看到的流量画像；
//   - /responses 失败时会 recyclePooledClient 摘除整个 Client，不应因为一次
//     后台探针失败而波及业务连接（反之亦然）。

// 维护请求的池用途标识。区分用途让「订阅同步（浏览器 UA）」与
// 「wham/清单/搜索（Codex CLI UA）」各自持有连接，不混用同一条 h2 连接。
const (
	maintenancePurposeCodex        = "codex"        // wham / models / alpha search，Codex CLI UA
	maintenancePurposeSubscription = "subscription" // 网页端订阅端点，浏览器 UA
	maintenancePurposeInvite       = "invite"       // 网页端推荐邀请端点，浏览器 UA
)

// maintenanceClientKey 构造维护 Client 的池键。与 clientPoolKey 前缀不同，
// 保证与 /responses 主池、Resin 池互不覆盖。
func maintenanceClientKey(account *auth.Account, proxyURL, transportMode, purpose string) string {
	accountID := int64(0)
	if account != nil {
		accountID = account.ID()
	}
	return fmt.Sprintf("maint|%s|%d|%s|%s", purpose, accountID, strings.TrimSpace(proxyURL), transportMode)
}

// getMaintenanceClient 返回维护请求专用的池化 Client。
//
// forceUTLS=true 时无视 CODEX_TRANSPORT_MODE 强制使用 uTLS Chrome 指纹
// （订阅端点在 Cloudflare 后面，普通指纹会被拦截）。
func getMaintenanceClient(account *auth.Account, proxyURL, purpose string, forceUTLS bool) *http.Client {
	return wrapIPv6Client(account, getRawMaintenanceClient(account, proxyURL, purpose, forceUTLS), true, forceUTLS, "maintenance:"+purpose)
}

func getRawMaintenanceClient(account *auth.Account, proxyURL, purpose string, forceUTLS bool) *http.Client {
	transportMode := codexTransportModeFromEnv()
	if forceUTLS {
		transportMode = codexTransportModeUTLSChrome
	}
	key := maintenanceClientKey(account, proxyURL, transportMode, purpose)

	if v, ok := clientPool.Load(key); ok {
		entry := v.(*poolEntry)
		entry.touch()
		return entry.client
	}

	var transport http.RoundTripper
	if forceUTLS {
		transport = NewUTLSTransport(proxyURL)
	} else {
		transport = newCodexTransport(proxyURL)
	}

	entry := &poolEntry{
		client: &http.Client{
			Transport: transport,
			// 不设整体超时：各调用方都用 context 控制超时（wham 25s、清单 15s、
			// search 120s、订阅 15s），与 /responses 池的语义保持一致。
			Timeout: 0,
		},
	}
	entry.touch()

	if v, loaded := clientPool.LoadOrStore(key, entry); loaded {
		// 竞争失败：丢弃自己刚建的 transport 并释放其连接（此时通常尚未拨号，
		// 但 uTLS 下不主动关闭就可能留下无人持有的连接）。
		releaseEvictedClient(entry.client)
		e := v.(*poolEntry)
		e.touch()
		return e.client
	}
	return entry.client
}

// getCodexMaintenanceClient 是 wham / 模型清单 / alpha search 共用的池化 Client。
// 走网关同款 transport（含 uTLS Chrome 指纹），与 /responses 的指纹保持一致。
func getCodexMaintenanceClient(account *auth.Account, proxyURL string) *http.Client {
	return getMaintenanceClient(account, proxyURL, maintenancePurposeCodex, false)
}

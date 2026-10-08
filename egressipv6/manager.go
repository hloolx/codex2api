// Package egressipv6 manages dedicated, locally assigned IPv6 source addresses.
package egressipv6

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/netip"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/codex2api/database"
	"github.com/tidwall/gjson"
)

type Manager struct {
	DB            *database.DB
	LocalIPs      func() ([]string, error)
	now           func() time.Time
	enabled       atomic.Bool
	ObserveSource func(context.Context, int64, string)
}

var current atomic.Pointer[Manager]

func Current() *Manager            { return current.Load() }
func Install(m *Manager)           { current.Store(m) }
func New(db *database.DB) *Manager { return &Manager{DB: db, LocalIPs: Discover, now: time.Now} }
func Enabled() bool                { m := Current(); return m != nil && m.enabled.Load() }
func (m *Manager) Refresh(ctx context.Context) error {
	c, e := m.DB.IPv6EgressConfig(ctx)
	if e == nil {
		m.enabled.Store(c.Enabled)
	}
	return e
}
func (m *Manager) Run(ctx context.Context) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := m.Refresh(ctx); err != nil {
				log.Printf("[IPv6Egress] configuration refresh failed: %v", err)
			}
		}
	}
}

func Discover() ([]string, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	ips := []string{}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, e := iface.Addrs()
		if e != nil {
			return nil, e
		}
		for _, addr := range addrs {
			p, e := netip.ParsePrefix(addr.String())
			if e != nil {
				continue
			}
			ip := p.Addr()
			if !ValidIP(ip) {
				continue
			}
			s := ip.String()
			if !seen[s] {
				ips = append(ips, s)
				seen[s] = true
			}
		}
	}
	sort.Slice(ips, func(i, j int) bool { return netip.MustParseAddr(ips[i]).Less(netip.MustParseAddr(ips[j])) })
	return ips, nil
}
func ValidIP(ip netip.Addr) bool {
	return ip.Is6() && !ip.Is4In6() && ip.IsGlobalUnicast() && !ip.IsPrivate() && ip.Zone() == ""
}
func ValidateSources(values, local []string) ([]string, error) {
	allowed := map[string]bool{}
	for _, ip := range local {
		allowed[ip] = true
	}
	seen := map[string]bool{}
	out := []string{}
	if len(values) > 4096 {
		return nil, errors.New("too many source addresses")
	}
	for _, value := range values {
		ip, e := netip.ParseAddr(strings.TrimSpace(value))
		if e != nil || !ValidIP(ip) || !allowed[ip.String()] {
			return nil, fmt.Errorf("IPv6 address is not assigned locally: %s", value)
		}
		s := ip.String()
		if !seen[s] {
			out = append(out, s)
			seen[s] = true
		}
	}
	return out, nil
}

type Route struct {
	Binding database.IPv6Binding
	Config  database.IPv6EgressConfig
}

func (m *Manager) Acquire(ctx context.Context, id int64) (Route, error) {
	return m.claim(ctx, id, "", "")
}
func (m *Manager) Rotate(ctx context.Context, id int64, ip, reason string) (Route, error) {
	return m.claim(ctx, id, ip, reason)
}
func (m *Manager) claim(ctx context.Context, id int64, ip, reason string) (Route, error) {
	c, err := m.DB.IPv6EgressConfig(ctx)
	if err != nil {
		return Route{}, err
	}
	m.enabled.Store(c.Enabled)
	if !c.Enabled {
		return Route{Config: c}, nil
	}
	local, err := m.LocalIPs()
	if err != nil {
		return Route{}, err
	}
	b, c, err := m.DB.ClaimIPv6Egress(ctx, id, local, ip, reason, m.now().Unix())
	if ip != "" && err == nil && b.IP != ip && c.Enabled {
		log.Printf("[IPv6Egress] account=%d old_ip=%s new_ip=%s reason=%s", id, ip, b.IP, reason)
	}
	return Route{b, c}, err
}

// Dialer only binds an existing address; it never adds addresses or falls back to IPv4.
type Dialer struct{ net.Dialer }

func NewDialer(ip string) (*Dialer, error) {
	addr, err := netip.ParseAddr(ip)
	if err != nil || !ValidIP(addr) {
		return nil, errors.New("invalid dedicated IPv6 source")
	}
	return &Dialer{net.Dialer{LocalAddr: &net.TCPAddr{IP: net.IP(addr.AsSlice())}, KeepAlive: 30 * time.Second}}, nil
}
func (d *Dialer) DialContext(ctx context.Context, _ string, address string) (net.Conn, error) {
	return d.Dialer.DialContext(ctx, "tcp6", address)
}
func (d *Dialer) Dial(_ string, address string) (net.Conn, error) {
	return d.DialContext(context.Background(), "tcp6", address)
}

// Only explicit transient upstream failures are candidates. Authentication,
// exhausted account quota, policy decisions and malformed input remain terminal.
func FailureReason(status int, body []byte, retry5xx bool) string {
	lower := strings.ToLower(string(body))
	for _, s := range []string{"usage limit", "quota exceeded", "exceeded your current quota", "insufficient credits", "insufficient_quota", "usage_limit_reached", "quota_exceeded", "billing_hard_limit", "credits_exhausted", "deactivated", "model_not_supported", "invalid_api_key", "token_expired", "policy_violation", "cyber"} {
		if strings.Contains(lower, s) {
			return ""
		}
	}
	if status == 429 {
		return "http_429"
	}
	if retry5xx && (status == 500 || status == 502 || status == 503 || status == 504) {
		return fmt.Sprintf("http_%d", status)
	}
	return ""
}
func FrameFailure(payload []byte, retry5xx bool) string {
	typ := gjson.GetBytes(payload, "type").String()
	if typ != "error" && typ != "response.failed" {
		return ""
	}
	for _, path := range []string{"status", "status_code", "response.status_code"} {
		if n := gjson.GetBytes(payload, path).Int(); n > 0 {
			if s := FailureReason(int(n), payload, retry5xx); s != "" {
				return s
			}
		}
	}
	code := gjson.GetBytes(payload, "error.code").String()
	if code == "" {
		code = gjson.GetBytes(payload, "response.error.code").String()
	}
	switch code {
	case "rate_limit_exceeded", "rate_limit_error":
		return FailureReason(429, payload, retry5xx)
	case "server_error", "server_is_overloaded":
		return FailureReason(503, payload, retry5xx)
	}
	return ""
}

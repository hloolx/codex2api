package wsrelay

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/egressipv6"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

func TestIPv6WebsocketRetriesOnlyBeforeOutput(t *testing.T) {
	for _, outputFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "pre-output", true: "after-output"}[outputFirst], func(t *testing.T) {
			ctx := context.Background()
			db, err := database.New("sqlite", filepath.Join(t.TempDir(), "ipv6.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			id, err := db.InsertAccountWithCredentials(ctx, "ipv6-ws-fixture", map[string]interface{}{"access_token": "fixture"}, "")
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := db.IPv6EgressConfig(ctx)
			if err != nil {
				t.Fatal(err)
			}
			cfg.Enabled = true
			if err = db.SaveIPv6EgressConfig(ctx, cfg); err != nil {
				t.Fatal(err)
			}
			routes := egressipv6.New(db)
			routes.LocalIPs = func() ([]string, error) { return []string{"2604::1", "2604::2", "2604::3"}, nil }
			route, err := routes.Acquire(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				conn, err := (&websocket.Upgrader{}).Upgrade(w, req, nil)
				if err != nil {
					return
				}
				defer conn.Close()
				if _, _, err = conn.ReadMessage(); err != nil {
					return
				}
				n := calls.Add(1)
				frames := []string{`{"type":"response.created"}`}
				if outputFirst || n > 1 {
					frames = append(frames, `{"type":"response.output_text.delta","delta":"hello"}`)
				}
				if n == 1 {
					frames = append(frames, `{"type":"response.failed","response":{"error":{"code":"server_error"}}}`)
				} else {
					frames = append(frames, `{"type":"response.completed","response":{"id":"fixture-response"}}`)
				}
				for _, frame := range frames {
					if err = conn.WriteMessage(websocket.TextMessage, []byte(frame)); err != nil {
						return
					}
				}
				_, _, _ = conn.ReadMessage()
			}))
			defer server.Close()
			manager := NewManager()
			defer manager.Stop()
			account := &auth.Account{DBID: id}
			wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
			attempt := &ipv6WSAttempt{ctx: ctx, manager: routes, accountID: id, route: route}
			seenIPs := []string{}
			attempt.execute = func(ip string) (*WsResponse, error) {
				seenIPs = append(seenIPs, ip)
				wc, pr, err := manager.AcquireConnection(ctx, account, wsURL, ip, http.Header{}, "")
				if err != nil {
					return nil, err
				}
				if err = NewExecutorWithManager(manager).sendRequest(wc, []byte(`{"type":"response.create"}`), pr.RequestID); err != nil {
					return nil, err
				}
				return &WsResponse{conn: wc, pendingReq: pr, manager: manager, sessionID: ip}, nil
			}
			resp, err := attempt.run()
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Close()
			types := []string{}
			if err = resp.ReadStream(func(payload []byte) bool {
				types = append(types, gjson.GetBytes(payload, "type").String())
				return true
			}); err != nil {
				t.Fatal(err)
			}
			if outputFirst {
				if len(seenIPs) != 1 || types[len(types)-1] != "response.failed" {
					t.Fatalf("replayed output: %v %v", seenIPs, types)
				}
			} else {
				if len(seenIPs) != 2 || seenIPs[0] == seenIPs[1] || strings.Join(types, ",") != "response.created,response.output_text.delta,response.completed" {
					t.Fatalf("retry output: %v %v", seenIPs, types)
				}
			}
		})
	}
}

package supabase_realtime

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// ---------------------------------------------------------------------------
// Fake Supabase Realtime server
// ---------------------------------------------------------------------------

type serverMode int

const (
	modeNormal    serverMode = iota // reply to join + heartbeat, push one change after join
	modeSilent                      // accept the socket but never answer anything (half-open link)
	modeDropAfter                   // normal, but close the socket right after pushing the first change
	modeJoinError                   // reject the join with phx_reply status=error
	modeErrorOnce                   // normal, but send phx_error for the channel after the first change
)

type fakeServer struct {
	srv    *httptest.Server
	conns  atomic.Int32 // sockets accepted
	joins  atomic.Int32 // phx_join messages received
	pushed atomic.Int32 // postgres_changes events pushed
	// modeFor decides the behaviour of the n-th accepted socket (1-based).
	modeFor func(n int) serverMode
}

func newFakeServer(t *testing.T, modeFor func(n int) serverMode) *fakeServer {
	t.Helper()
	fs := &fakeServer{modeFor: modeFor}
	fs.srv = httptest.NewServer(http.HandlerFunc(fs.handle))
	t.Cleanup(fs.srv.Close)
	return fs
}

func (fs *fakeServer) url() string {
	return "ws" + strings.TrimPrefix(fs.srv.URL, "http") + "/realtime/v1/websocket"
}

func (fs *fakeServer) handle(w http.ResponseWriter, r *http.Request) {
	n := int(fs.conns.Add(1))
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer c.CloseNow()

	mode := fs.modeFor(n)
	ctx := r.Context()
	sentFirst := false

	for {
		var msg map[string]any
		if err := wsjson.Read(ctx, c, &msg); err != nil {
			return
		}
		event, _ := msg["event"].(string)
		topic, _ := msg["topic"].(string)

		switch event {
		case "phx_join":
			fs.joins.Add(1)
			switch mode {
			case modeSilent:
				continue
			case modeJoinError:
				_ = wsjson.Write(ctx, c, map[string]any{
					"event": "phx_reply", "topic": topic, "ref": "1",
					"payload": map[string]any{
						"status":   "error",
						"response": map[string]any{"reason": "unauthorized"},
					},
				})
				continue
			}
			_ = wsjson.Write(ctx, c, map[string]any{
				"event": "phx_reply", "topic": topic, "ref": "1",
				"payload": map[string]any{"status": "ok", "response": map[string]any{}},
			})
			seq := fs.pushed.Add(1)
			_ = wsjson.Write(ctx, c, map[string]any{
				"event": "postgres_changes", "topic": topic,
				"payload": map[string]any{"data": map[string]any{
					"type": "UPDATE", "record": map[string]any{"seq": float64(seq)},
				}},
			})
			if !sentFirst {
				sentFirst = true
				switch mode {
				case modeDropAfter:
					_ = c.Close(websocket.StatusGoingAway, "bye")
					return
				case modeErrorOnce:
					_ = wsjson.Write(ctx, c, map[string]any{
						"event": "phx_error", "topic": topic, "ref": "1",
						"payload": map[string]any{},
					})
				}
			}
		case "heartbeat":
			if mode == modeSilent {
				continue
			}
			_ = wsjson.Write(ctx, c, map[string]any{
				"event": "phx_reply", "topic": "phoenix", "ref": msg["ref"],
				"payload": map[string]any{"status": "ok", "response": map[string]any{}},
			})
		}
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

type testEnv struct {
	client *Client
	fs     *fakeServer
	events chan int // seq numbers of received changes
	logs   *observer.ObservedLogs
}

// newTestEnv builds a client wired to the fake server. hb is the heartbeat
// interval and reconnect the reconnect interval; both are far shorter than the
// production defaults so the tests run quickly.
func newTestEnv(t *testing.T, modeFor func(n int) serverMode, hb, reconnect time.Duration) *testEnv {
	t.Helper()
	fs := newFakeServer(t, modeFor)
	core, logs := observer.New(zapcore.DebugLevel)

	c := &Client{
		Url:               fs.url(),
		logger:            zap.New(core),
		dialTimeout:       2 * time.Second,
		heartbeatDuration: 500 * time.Millisecond,
		heartbeatInterval: hb,
		reconnectInterval: reconnect,
		state:             stateDisconnected,
		messageHandlers:   make(map[string]func(map[string]any)),
	}
	env := &testEnv{client: c, fs: fs, events: make(chan int, 64), logs: logs}

	if err := c.Connect(); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { _ = c.Disconnect() })

	err := c.ListenToPostgresChanges(PostgresChangesOptions{Schema: "device", Table: "device_list", Filter: "*"},
		func(msg map[string]any) {
			payload, _ := msg["payload"].(map[string]any)
			data, _ := payload["data"].(map[string]any)
			rec, _ := data["record"].(map[string]any)
			seq, _ := rec["seq"].(float64)
			env.events <- int(seq)
		})
	if err != nil {
		t.Fatalf("ListenToPostgresChanges: %v", err)
	}
	return env
}

func (e *testEnv) waitEvent(t *testing.T, what string) int {
	t.Helper()
	select {
	case seq := <-e.events:
		return seq
	case <-time.After(6 * time.Second):
		t.Fatalf("timed out waiting for %s (joins=%d conns=%d)", what, e.fs.joins.Load(), e.fs.conns.Load())
		return 0
	}
}

func (e *testEnv) breakLocalConnection(t *testing.T) {
	t.Helper()
	e.client.mu.Lock()
	c := e.client.conn
	e.client.mu.Unlock()
	if c == nil {
		t.Fatal("no active connection to break")
	}
	_ = c.CloseNow()
}

func normalThenNormal(int) serverMode { return modeNormal }

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestReceivesChanges(t *testing.T) {
	env := newTestEnv(t, normalThenNormal, time.Second, 100*time.Millisecond)
	if got := env.waitEvent(t, "first change"); got != 1 {
		t.Fatalf("seq = %d, want 1", got)
	}
}

// The server closes the socket cleanly: the read loop notices and the client
// must reconnect and re-subscribe.
func TestReconnectsAfterServerClose(t *testing.T) {
	env := newTestEnv(t, func(n int) serverMode {
		if n == 1 {
			return modeDropAfter
		}
		return modeNormal
	}, time.Second, 100*time.Millisecond)

	env.waitEvent(t, "change before drop")
	env.waitEvent(t, "change after reconnect")
	if j := env.fs.joins.Load(); j < 2 {
		t.Fatalf("joins = %d, want >= 2 (client must re-subscribe)", j)
	}
}

// Regression test for the dead-forever bug. When the heartbeat write fails,
// heartbeatLoop used to call Disconnect(), which sets shutdown=true and cancels
// the context, so the client never reconnected again and the process kept
// running without any Realtime subscription. Here the heartbeat (40ms) is much
// faster than the reconnect interval (400ms), so the heartbeat failure wins the
// race, exactly like a network blip in production.
func TestReconnectsAfterHeartbeatFailure(t *testing.T) {
	env := newTestEnv(t, normalThenNormal, 40*time.Millisecond, 400*time.Millisecond)
	env.waitEvent(t, "initial change")

	env.breakLocalConnection(t)

	env.waitEvent(t, "change after heartbeat failure")
	if !waitUntil(3*time.Second, env.client.IsClientAlive) {
		t.Fatal("client did not become alive again after reconnect")
	}
	env.client.mu.Lock()
	shutdown := env.client.shutdown
	env.client.mu.Unlock()
	if shutdown {
		t.Fatal("shutdown flag was set by an internal failure; only Disconnect() may set it")
	}
}

// Half-open link (silently dropped NAT/ECS connection): writes keep succeeding
// and the read blocks forever. The client must notice that nothing is coming
// back and reconnect.
func TestReconnectsWhenServerGoesSilent(t *testing.T) {
	env := newTestEnv(t, func(n int) serverMode {
		if n == 1 {
			return modeSilent
		}
		return modeNormal
	}, 50*time.Millisecond, 100*time.Millisecond)

	env.waitEvent(t, "change delivered by second connection")
	if c := env.fs.conns.Load(); c < 2 {
		t.Fatalf("conns = %d, want >= 2", c)
	}
}

func TestDisconnectStopsReconnectAndConnectRevives(t *testing.T) {
	env := newTestEnv(t, normalThenNormal, time.Second, 100*time.Millisecond)
	env.waitEvent(t, "initial change")

	if err := env.client.Disconnect(); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
	connsAfterDisconnect := env.fs.conns.Load()
	time.Sleep(500 * time.Millisecond)
	if got := env.fs.conns.Load(); got != connsAfterDisconnect {
		t.Fatalf("client reconnected after an intentional Disconnect (conns %d -> %d)", connsAfterDisconnect, got)
	}
	if env.client.IsClientAlive() {
		t.Fatal("client reports alive after Disconnect")
	}

	// An explicit Connect() is a clear request to run again: it re-joins the
	// existing subscriptions and reconnects automatically afterwards.
	if err := env.client.Connect(); err != nil {
		t.Fatalf("second Connect: %v", err)
	}
	if !env.client.IsClientAlive() {
		t.Fatal("client not alive after second Connect")
	}
	env.waitEvent(t, "change after Connect()")

	env.breakLocalConnection(t)
	env.waitEvent(t, "change after reconnect following revived Connect()")
}

// A rejected subscription used to be silently ignored (phx_reply was never
// inspected), so a bad table/permission produced a process that "runs" but
// never receives anything and never says why.
func TestRejectedJoinIsLogged(t *testing.T) {
	env := newTestEnv(t, func(int) serverMode { return modeJoinError }, time.Second, 100*time.Millisecond)

	if !waitUntil(3*time.Second, func() bool {
		return env.logs.FilterLevelExact(zapcore.ErrorLevel).FilterMessageSnippet("rejected").Len() > 0
	}) {
		t.Fatalf("no error log for a rejected subscription; logs: %v", env.logs.All())
	}
}

// If the server kills the channel (phx_error) while the socket stays up, the
// client must re-join, otherwise events silently stop.
func TestRejoinsAfterChannelError(t *testing.T) {
	env := newTestEnv(t, func(n int) serverMode {
		if n == 1 {
			return modeErrorOnce
		}
		return modeNormal
	}, time.Second, 100*time.Millisecond)

	env.waitEvent(t, "change before channel error")
	env.waitEvent(t, "change after re-join")
	if j := env.fs.joins.Load(); j < 2 {
		t.Fatalf("joins = %d, want >= 2 (client must re-join after phx_error)", j)
	}
}

// Dial errors contain the full WebSocket URL, which carries the API key and
// (in pooler mode) the database password. They must never reach the logs.
func TestRedactsSecretsFromErrors(t *testing.T) {
	c := CreateRealtimeClient("sb_secret_abc123", zap.NewNop(),
		"host", "6543", "postgres", "postgres.projref", "p@ss w0rd!")

	err := c.redact(errors.New(`failed to WebSocket dial: Get "` + c.Url + `": dial tcp: no such host`))
	for _, secret := range []string{"sb_secret_abc123", "p@ss w0rd!", url.QueryEscape("p@ss w0rd!")} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("redacted error still contains %q: %s", secret, err)
		}
	}
	if !strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatalf("expected [REDACTED] marker, got: %s", err)
	}
}

func waitUntil(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return cond()
}

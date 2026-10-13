package supabase_realtime

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"go.uber.org/zap"
)

// connectionState represents the WebSocket connection status
type connectionState int

const (
	stateDisconnected connectionState = iota
	stateConnecting
	stateConnected
	stateReconnecting
)

const (
	// maxReconnectInterval caps the exponential reconnect backoff.
	maxReconnectInterval = 30 * time.Second

	// readLimit is the largest single Realtime message we accept. The
	// websocket library defaults to 32 KiB, which a postgres_changes event
	// for a wide row (REPLICA IDENTITY FULL sends both old and new rows)
	// can exceed; hitting the limit closes the connection.
	readLimit = 16 << 20
)

// Client is the Supabase Realtime WebSocket client.
//
// Lifecycle: Connect() starts the client and Disconnect() stops it. In between,
// any connection failure (read error, failed heartbeat, server gone silent) is
// handled internally by reconnecting with exponential backoff and re-joining
// every subscribed topic. Only Disconnect() stops reconnecting.
type Client struct {
	Url     string // WebSocket URL
	RestUrl string // REST API URL
	ApiKey  string

	mu           sync.Mutex
	conn         *websocket.Conn
	ctx          context.Context    // lives from Connect() until Disconnect()
	cancel       context.CancelFunc // cancels ctx
	connCancel   context.CancelFunc // stops the reader and heartbeat of the current conn
	state        connectionState
	shutdown     bool // set only by Disconnect()
	reconnecting bool // a reconnect loop is running

	subscriptions   []PostgresChangesOptions
	messageHandlers map[string]func(map[string]any)

	logger            *zap.Logger
	dialTimeout       time.Duration
	reconnectInterval time.Duration
	heartbeatDuration time.Duration
	heartbeatInterval time.Duration

	ref      atomic.Uint64 // message ref counter
	lastRecv atomic.Int64  // unix nanos of the last frame received from the server
}

// CreateRealtimeClient initializes a new Client
// Supports either projectRef or separate host/port/database/user/password params
func CreateRealtimeClient(apiKey string, logger *zap.Logger, opts ...string) *Client {
	var wsUrl, restUrl string

	switch len(opts) {
	case 1:
		// Direct mode: opts[0] = projectRef
		projectRef := opts[0]
		wsUrl = fmt.Sprintf(
			"wss://%s/realtime/v1/websocket?apikey=%s&log_level=info&vsn=1.0.0",
			projectRef, apiKey,
		)
		restUrl = fmt.Sprintf("https://%s/rest/v1", projectRef)
		logger.Info("Using normal projectRef mode", zap.String("projectRef", projectRef))

	case 5:
		// Pooler mode: opts[0]=host, opts[1]=port, opts[2]=database, opts[3]=user, opts[4]=password
		host, port, database, user, password := opts[0], opts[1], opts[2], opts[3], opts[4]
		parts := strings.SplitN(user, ".", 2)
		if len(parts) != 2 {
			logger.Fatal("Expected user format postgres.PROJECT_REF", zap.String("user", user))
		}
		projectRef := parts[1]
		wsUrl = fmt.Sprintf(
			"wss://%s.supabase.co/realtime/v1/websocket?apikey=%s&user_token=%s&log_level=info&vsn=1.0.0",
			projectRef, apiKey, url.QueryEscape(password),
		)
		restUrl = fmt.Sprintf("https://%s.supabase.co/rest/v1", projectRef)
		logger.Info("Using session pooler mode",
			zap.String("host", host),
			zap.String("port", port),
			zap.String("database", database),
			zap.String("user", user),
		)

	default:
		logger.Fatal("Invalid arguments: pass projectRef or host,port,database,user,password")
	}

	return &Client{
		Url:               wsUrl,
		RestUrl:           restUrl,
		ApiKey:            apiKey,
		logger:            logger,
		dialTimeout:       10 * time.Second,
		heartbeatDuration: 5 * time.Second,
		heartbeatInterval: 20 * time.Second,
		reconnectInterval: 500 * time.Millisecond,
		state:             stateDisconnected,
		messageHandlers:   make(map[string]func(map[string]any)),
	}
}

// Connect establishes the WebSocket connection and starts the client. Calling
// it after Disconnect() starts the client again and re-joins any topics that
// were subscribed before.
func (client *Client) Connect() error {
	client.mu.Lock()
	if client.state == stateConnected && client.conn != nil {
		client.mu.Unlock()
		return nil
	}
	if client.shutdown || client.ctx == nil || client.ctx.Err() != nil {
		client.ctx, client.cancel = context.WithCancel(context.Background())
	}
	client.shutdown = false
	if client.state == stateDisconnected {
		client.state = stateConnecting
	}
	client.mu.Unlock()

	return client.connectOnce()
}

// connectOnce dials the server once and, on success, starts the reader and
// heartbeat for the new connection and re-joins all subscribed topics.
func (client *Client) connectOnce() error {
	client.mu.Lock()
	runCtx := client.ctx
	client.mu.Unlock()
	if runCtx == nil || runCtx.Err() != nil {
		return errors.New("connect failed: client is disconnected")
	}

	dialCtx, cancel := context.WithTimeout(runCtx, client.dialTimeout)
	conn, _, err := websocket.Dial(dialCtx, client.Url, nil)
	cancel()
	if err != nil {
		err = client.redact(err)
		client.logger.Error("Dial failed", zap.Error(err))
		return fmt.Errorf("connect failed: %w", err)
	}
	conn.SetReadLimit(readLimit)

	client.mu.Lock()
	if client.shutdown || runCtx.Err() != nil {
		client.mu.Unlock()
		_ = conn.CloseNow()
		return errors.New("connect failed: client is disconnected")
	}
	if client.conn != nil {
		// Another goroutine connected while we were dialing; keep theirs.
		client.mu.Unlock()
		_ = conn.CloseNow()
		return nil
	}
	connCtx, connCancel := context.WithCancel(runCtx)
	client.conn = conn
	client.connCancel = connCancel
	client.state = stateConnected
	client.lastRecv.Store(time.Now().UnixNano())
	client.mu.Unlock()

	client.logger.Info("Realtime connected")

	go client.listenForMessages(connCtx, conn)
	go client.heartbeatLoop(connCtx, conn)
	client.resubscribeAll()
	return nil
}

// Disconnect gracefully closes the WebSocket connection and stops reconnecting.
func (client *Client) Disconnect() error {
	client.mu.Lock()
	client.shutdown = true
	cancel := client.cancel
	connCancel := client.connCancel
	conn := client.conn
	client.cancel = nil
	client.connCancel = nil
	client.ctx = nil
	client.conn = nil
	client.state = stateDisconnected
	client.mu.Unlock()

	if conn != nil {
		_ = conn.Close(websocket.StatusNormalClosure, "client disconnect")
	}
	if connCancel != nil {
		connCancel()
	}
	if cancel != nil {
		cancel()
	}
	return nil
}

// IsClientAlive reports whether the client currently has a live connection.
func (client *Client) IsClientAlive() bool {
	client.mu.Lock()
	defer client.mu.Unlock()
	return client.state == stateConnected && client.conn != nil
}

// IsConnected checks if the client is connected
func (client *Client) IsConnected() bool {
	return client.IsClientAlive()
}

// connectionLost tears down conn and starts reconnecting, unless the client
// was intentionally disconnected. It is safe to call from several goroutines
// for the same conn; only the first call has an effect.
func (client *Client) connectionLost(conn *websocket.Conn, reason string, err error) {
	client.mu.Lock()
	if client.conn != conn {
		// Stale connection, or already handled.
		client.mu.Unlock()
		return
	}
	client.conn = nil
	if client.connCancel != nil {
		client.connCancel()
		client.connCancel = nil
	}
	shutdown := client.shutdown
	if shutdown {
		client.state = stateDisconnected
	} else {
		client.state = stateReconnecting
	}
	client.mu.Unlock()

	_ = conn.CloseNow()
	if shutdown {
		return
	}

	client.logger.Warn("Realtime connection lost, reconnecting",
		zap.String("reason", reason), zap.Error(client.redact(err)))
	client.startReconnect()
}

// startReconnect launches the reconnect loop unless one is already running.
func (client *Client) startReconnect() {
	client.mu.Lock()
	if client.reconnecting || client.shutdown || client.ctx == nil {
		client.mu.Unlock()
		return
	}
	client.reconnecting = true
	runCtx := client.ctx
	client.mu.Unlock()

	go client.reconnectLoop(runCtx)
}

// reconnectLoop retries connectOnce with exponential backoff until it
// succeeds or the client is disconnected.
func (client *Client) reconnectLoop(ctx context.Context) {
	delay := client.reconnectInterval
	for attempt := 1; ; attempt++ {
		select {
		case <-ctx.Done():
			client.mu.Lock()
			client.reconnecting = false
			client.mu.Unlock()
			client.logger.Info("Reconnect stopped: client disconnected")
			return
		case <-time.After(delay):
		}

		if err := client.connectOnce(); err != nil {
			delay = min(delay*2, maxReconnectInterval)
			client.logger.Warn("Reconnect failed",
				zap.Int("attempt", attempt), zap.Duration("next_retry_in", delay), zap.Error(err))
			continue
		}

		client.mu.Lock()
		if client.conn == nil && !client.shutdown && ctx.Err() == nil {
			// The new connection already dropped again before we got here;
			// its connectionLost call saw us still running, so keep going.
			client.mu.Unlock()
			continue
		}
		client.reconnecting = false
		client.mu.Unlock()
		client.logger.Info("Reconnected successfully", zap.Int("attempt", attempt))
		return
	}
}

// nextRef returns a fresh message ref.
func (client *Client) nextRef() string {
	return fmt.Sprint(client.ref.Add(1))
}

// redact removes the API key and user token from err's text. Dial errors
// include the full WebSocket URL, which carries both as query parameters.
func (client *Client) redact(err error) error {
	if err == nil {
		return nil
	}
	u, perr := url.Parse(client.Url)
	if perr != nil {
		return err
	}
	msg := err.Error()
	changed := false
	for _, key := range []string{"apikey", "user_token"} {
		v := u.Query().Get(key)
		if v == "" {
			continue
		}
		for _, form := range []string{v, url.QueryEscape(v)} {
			if strings.Contains(msg, form) {
				msg = strings.ReplaceAll(msg, form, "[REDACTED]")
				changed = true
			}
		}
	}
	if !changed {
		return err
	}
	return errors.New(msg)
}

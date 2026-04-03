package supabase_realtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
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

// Client is the Supabase Realtime WebSocket client
type Client struct {
	Url     string // WebSocket URL
	RestUrl string // REST API URL
	ApiKey  string

	mu       sync.Mutex
	conn     *websocket.Conn
	closed   chan struct{}
	ctx      context.Context
	cancel   context.CancelFunc
	state    connectionState
	shutdown bool

	subscriptions   []PostgresChangesOptions
	messageHandlers map[string]func(map[string]any)

	logger            *zap.Logger
	reconnectMu       sync.Mutex
	dialTimeout       time.Duration
	reconnectInterval time.Duration
	heartbeatDuration time.Duration
	heartbeatInterval time.Duration
	heartbeatCancel   context.CancelFunc
}

// CreateRealtimeClient initializes a new Client
func CreateRealtimeClient(projectRef string, apiKey string, logger *zap.Logger) *Client {
	wsUrl := fmt.Sprintf(
		"wss://%s.supabase.co/realtime/v1/websocket?apikey=%s&log_level=info&vsn=1.0.0",
		projectRef, apiKey,
	)

	restUrl := fmt.Sprintf("https://%s.supabase.co/rest/v1", projectRef)

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

// Connect establishes a WebSocket connection
func (client *Client) Connect() error {
	client.mu.Lock()
	if client.ctx != nil && client.state == stateConnected && client.conn != nil {
		client.mu.Unlock()
		return nil
	}

	client.ctx, client.cancel = context.WithCancel(context.Background())
	client.closed = make(chan struct{})
	client.state = stateConnecting
	client.mu.Unlock()

	if err := client.dialServer(); err != nil {
		return fmt.Errorf("connect failed: %w", err)
	}

	client.startHeartbeats()
	go client.listenForMessages()

	client.mu.Lock()
	client.state = stateConnected
	client.mu.Unlock()

	return nil
}

// Disconnect gracefully closes the WebSocket connection
func (client *Client) Disconnect() error {
	client.mu.Lock()
	defer client.mu.Unlock()

	client.shutdown = true // mark client as intentionally shutting down

	if client.heartbeatCancel != nil {
		client.heartbeatCancel()
		client.heartbeatCancel = nil
	}

	if client.cancel != nil {
		client.cancel()
		client.cancel = nil
		client.ctx = nil
	}

	if client.conn != nil {
		_ = client.conn.Close(websocket.StatusNormalClosure, "client disconnect")
		client.conn = nil
	}

	if client.closed != nil {
		close(client.closed)
		client.closed = nil
	}

	client.state = stateDisconnected
	return nil
}

// isClientAlive checks if client is connected and healthy
func (client *Client) IsClientAlive() bool {
	client.mu.Lock()
	defer client.mu.Unlock()
	return client.state == stateConnected && client.conn != nil && client.closed != nil
}

// dialServer connects to the WebSocket server
func (client *Client) dialServer() error {
	client.mu.Lock()
	defer client.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), client.dialTimeout)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, client.Url, nil)
	if err != nil {
		client.logger.Error("Dial failed", zap.Error(err))
		return err
	}

	client.conn = conn
	//client.logger.Info("WebSocket connected", zap.String("url", client.Url))
	return nil
}

// reconnect attempts to reconnect with exponential backoff
func (client *Client) reconnect(ctx context.Context) {
	client.reconnectMu.Lock()
	defer client.reconnectMu.Unlock()

	client.mu.Lock()
	if client.shutdown {
		client.mu.Unlock()
		client.logger.Info("Reconnect skipped: client is shutting down")
		return
	}
	client.state = stateReconnecting
	client.mu.Unlock()

	client.logger.Warn("Starting reconnect loop...")

	retryTicker := time.NewTicker(client.reconnectInterval)
	defer retryTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			client.logger.Warn("Reconnect context done, giving up")
			return
		case <-retryTicker.C:
			client.mu.Lock()
			if client.shutdown {
				client.mu.Unlock()
				client.logger.Info("Reconnect stopped: client is shutting down")
				return
			}
			client.mu.Unlock()

			if err := client.Connect(); err == nil {
				client.logger.Info("Reconnected successfully")
				client.resubscribeAll()
				return
			} else {
				client.logger.Warn("Reconnect failed", zap.Error(err))
			}
		}
	}
}

// IsConnected checks if the client is connected
func (client *Client) IsConnected() bool {
	client.mu.Lock()
	defer client.mu.Unlock()
	return client.state == stateConnected && client.conn != nil && client.closed != nil
}

// isConnectionAlive determines if a connection error means a dead connection
func (client *Client) isConnectionAlive(err error) bool {
	return !(errors.Is(err, io.EOF) ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.As(err, new(net.Error)) ||
		errors.As(err, new(websocket.CloseError)))
}

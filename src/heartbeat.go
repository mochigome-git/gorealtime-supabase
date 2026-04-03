package supabase_realtime

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/coder/websocket/wsjson"
	"go.uber.org/zap"
)

// startHeartbeats launches a heartbeat goroutine
func (client *Client) startHeartbeats() {
	client.mu.Lock()
	if client.heartbeatCancel != nil {
		client.logger.Info("Stop heartbeat...")
		client.heartbeatCancel()
	}

	ctx := client.ctx
	if ctx == nil {
		client.mu.Unlock()
		return
	}

	hbCtx, cancel := context.WithCancel(ctx)
	client.heartbeatCancel = cancel
	client.mu.Unlock()

	go client.heartbeatLoop(hbCtx)
}

// heartbeatLoop periodically sends heartbeat messages
func (client *Client) heartbeatLoop(ctx context.Context) {
	retryInterval := client.heartbeatInterval

	for {
		if ctx.Err() != nil {
			return
		}

		err := client.sendHeartbeat()
		if err != nil {
			client.logger.Error("Heartbeat failed", zap.Error(err))
			_ = client.Disconnect()
			time.Sleep(retryInterval)
			client.reconnect(context.Background())

			retryInterval = time.Duration(math.Min(float64(retryInterval*2), float64(30*time.Second)))
			continue
		}

		retryInterval = client.heartbeatInterval

		select {
		case <-ctx.Done():
			return
		case <-time.After(client.heartbeatInterval):
		}
	}
}

// sendHeartbeat sends a heartbeat message to the server
func (client *Client) sendHeartbeat() error {
	client.mu.Lock()
	conn := client.conn
	ctx := client.ctx
	client.mu.Unlock()

	if conn == nil {
		return errors.New("no active connection")
	}

	heartbeat := HearbeatMsg{
		TemplateMsg: TemplateMsg{
			Event: HEARTBEAT_EVENT,
			Topic: "phoenix",
			Ref:   "",
		},
		Payload: struct{}{},
	}

	heartbeatCtx, cancel := context.WithTimeout(ctx, client.heartbeatDuration)
	defer cancel()

	client.logger.Debug("Sending heartbeat")
	return wsjson.Write(heartbeatCtx, conn, heartbeat)
}

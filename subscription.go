package supabase_realtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"go.uber.org/zap"
)

func topicFor(opts PostgresChangesOptions) string {
	return fmt.Sprintf("realtime:%s:%s", opts.Schema, opts.Table)
}

// resubscribeAll re-joins every subscribed topic on the current connection.
func (client *Client) resubscribeAll() {
	client.mu.Lock()
	subs := append([]PostgresChangesOptions(nil), client.subscriptions...)
	client.mu.Unlock()

	for _, sub := range subs {
		if err := client.sendSubscription(sub); err != nil {
			client.logger.Warn("Re-subscribe failed",
				zap.String("topic", topicFor(sub)), zap.Error(err))
		}
	}
}

// ListenToPostgresChanges subscribes to changes on a table. The subscription
// is kept across reconnects. Subscribing to the same schema and table again
// replaces the previous options and handler.
func (client *Client) ListenToPostgresChanges(opts PostgresChangesOptions, handler func(map[string]any)) error {
	if !client.IsClientAlive() {
		return errors.New("client not connected")
	}

	topic := topicFor(opts)

	client.mu.Lock()
	replaced := false
	for i, s := range client.subscriptions {
		if topicFor(s) == topic {
			client.subscriptions[i] = opts
			replaced = true
		}
	}
	if !replaced {
		client.subscriptions = append(client.subscriptions, opts)
	}
	client.messageHandlers[topic] = handler
	client.mu.Unlock()

	return client.sendSubscription(opts)
}

// sendSubscription sends a join request for opts on the current connection.
// The server's answer (phx_reply / system) is logged by dispatch.
func (client *Client) sendSubscription(opts PostgresChangesOptions) error {
	subscribeMsg := map[string]any{
		"topic": topicFor(opts),
		"event": JOIN_EVENT,
		"payload": map[string]any{
			"config": map[string]any{
				"postgres_changes": []map[string]any{
					{
						"event":  opts.Filter,
						"schema": opts.Schema,
						"table":  opts.Table,
					},
				},
			},
		},
		"ref": client.nextRef(),
	}

	client.mu.Lock()
	conn := client.conn
	ctx := client.ctx
	client.mu.Unlock()

	if conn == nil || ctx == nil {
		return errors.New("sendSubscription: no active connection")
	}

	ctx, cancel := context.WithTimeout(ctx, client.dialTimeout)
	defer cancel()

	return wsjson.Write(ctx, conn, subscribeMsg)
}

// listenForMessages reads messages from conn until it fails or ctx is
// cancelled. A read failure hands the connection to connectionLost.
func (client *Client) listenForMessages(ctx context.Context, conn *websocket.Conn) {
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			if ctx.Err() != nil {
				client.logger.Debug("Listener exiting")
				return
			}
			client.connectionLost(conn, "read failed", err)
			return
		}
		client.lastRecv.Store(time.Now().UnixNano())

		var msg map[string]any
		if err := json.Unmarshal(data, &msg); err != nil {
			client.logger.Warn("Ignoring malformed Realtime message", zap.Error(err))
			continue
		}
		client.dispatch(msg)
	}
}

// dispatch routes one server message.
func (client *Client) dispatch(msg map[string]any) {
	event, _ := msg["event"].(string)
	topic, _ := msg["topic"].(string)

	switch event {
	case POSTGRES_CHANGE_EVENT:
		client.mu.Lock()
		handler, exists := client.messageHandlers[topic]
		client.mu.Unlock()

		if exists {
			go handler(msg)
		} else {
			client.logger.Warn("No handler found for topic", zap.String("topic", topic))
		}

	case REPLY_EVENT:
		if topic == "phoenix" {
			return // heartbeat reply
		}
		payload, _ := msg["payload"].(map[string]any)
		status, _ := payload["status"].(string)
		if status == "ok" {
			client.logger.Info("Realtime channel joined", zap.String("topic", topic))
			return
		}
		client.logger.Error("Realtime subscription rejected",
			zap.String("topic", topic),
			zap.String("status", status),
			zap.Any("response", payload["response"]))

	case SYS_EVENT:
		// Supabase reports the outcome of the postgres_changes setup here,
		// e.g. "Subscribed to PostgreSQL" or why it could not subscribe.
		payload, _ := msg["payload"].(map[string]any)
		status, _ := payload["status"].(string)
		message, _ := payload["message"].(string)
		if status == "error" {
			client.logger.Error("Realtime subscription error",
				zap.String("topic", topic), zap.String("message", message))
		} else {
			client.logger.Info("Realtime system message",
				zap.String("topic", topic), zap.String("status", status), zap.String("message", message))
		}

	case ERROR_EVENT, CLOSE_EVENT:
		// The server dropped the channel while the socket stays open. Without
		// a re-join, events for this topic would silently stop.
		if opts, ok := client.subscriptionFor(topic); ok {
			client.logger.Warn("Realtime channel closed by server, re-joining",
				zap.String("topic", topic), zap.String("event", event))
			go client.rejoin(opts)
		}
	}
}

func (client *Client) subscriptionFor(topic string) (PostgresChangesOptions, bool) {
	client.mu.Lock()
	defer client.mu.Unlock()
	for _, s := range client.subscriptions {
		if topicFor(s) == topic {
			return s, true
		}
	}
	return PostgresChangesOptions{}, false
}

// rejoin re-sends the join for one topic after a short delay. If there is no
// connection at that point, the reconnect path re-joins it anyway.
func (client *Client) rejoin(opts PostgresChangesOptions) {
	client.mu.Lock()
	ctx := client.ctx
	client.mu.Unlock()
	if ctx == nil {
		return
	}

	select {
	case <-ctx.Done():
		return
	case <-time.After(client.reconnectInterval):
	}

	if err := client.sendSubscription(opts); err != nil {
		client.logger.Warn("Re-join failed",
			zap.String("topic", topicFor(opts)), zap.Error(err))
	}
}

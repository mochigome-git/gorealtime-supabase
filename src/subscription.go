package supabase_realtime

import (
	"context"
	"errors"
	"fmt"

	"github.com/coder/websocket/wsjson"
	"go.uber.org/zap"
)

// resubscribeAll re-subscribes to all previous Postgres topics
func (client *Client) resubscribeAll() {
	client.mu.Lock()
	subs := append([]PostgresChangesOptions(nil), client.subscriptions...)
	client.mu.Unlock()

	for _, sub := range subs {
		_ = client.sendSubscription(sub)
	}
}

// ListenToPostgresChanges subscribes to changes on a table
func (client *Client) ListenToPostgresChanges(opts PostgresChangesOptions, handler func(map[string]any)) error {
	if !client.IsClientAlive() {
		return errors.New("client not connected")
	}

	topic := fmt.Sprintf("realtime:%s:%s", opts.Schema, opts.Table)

	client.mu.Lock()
	client.subscriptions = append(client.subscriptions, opts)
	client.messageHandlers[topic] = handler
	client.mu.Unlock()

	return client.sendSubscription(opts)
}

// sendSubscription sends a subscription request to the server
func (client *Client) sendSubscription(opts PostgresChangesOptions) error {
	topic := fmt.Sprintf("realtime:%s:%s", opts.Schema, opts.Table)
	subscribeMsg := map[string]any{
		"topic": topic,
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
		"ref": "1",
	}

	client.mu.Lock()
	conn := client.conn
	ctx := client.ctx
	client.mu.Unlock()

	if conn == nil {
		return errors.New("sendSubscription: no active connection")
	}

	ctx, cancel := context.WithTimeout(ctx, client.dialTimeout)
	defer cancel()

	return wsjson.Write(ctx, conn, subscribeMsg)
}

// listenForMessages reads messages from the WebSocket server
func (client *Client) listenForMessages() {
	for {
		client.mu.Lock()
		ctx := client.ctx
		conn := client.conn
		client.mu.Unlock()

		if ctx == nil || ctx.Err() != nil {
			client.logger.Info("Listener exiting")
			return
		}

		var msg map[string]any
		if err := wsjson.Read(ctx, conn, &msg); err != nil {
			if !client.isConnectionAlive(err) {
				client.logger.Warn("Connection lost during message read")
				go client.reconnect(context.Background())
				return
			}
			continue
		}

		if event, ok := msg["event"].(string); ok && event == POSTGRES_CHANGE_EVENT {
			topic, _ := msg["topic"].(string)

			client.mu.Lock()
			handler, exists := client.messageHandlers[topic]
			client.mu.Unlock()

			if exists {
				go handler(msg)
			} else {
				client.logger.Warn("No handler found for topic", zap.String("topic", topic))
			}
		}
	}
}

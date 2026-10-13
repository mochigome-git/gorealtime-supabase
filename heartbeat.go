package supabase_realtime

import (
	"context"
	"fmt"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// heartbeatLoop sends a heartbeat every heartbeatInterval for as long as conn
// is the active connection. It hands the connection to connectionLost (which
// reconnects) when a heartbeat cannot be written, or when the server has sent
// nothing back for two intervals: a healthy server replies to every heartbeat,
// so silence means a half-open link (e.g. dropped by a NAT or load balancer)
// where writes still succeed but nothing is delivered.
func (client *Client) heartbeatLoop(ctx context.Context, conn *websocket.Conn) {
	ticker := time.NewTicker(client.heartbeatInterval)
	defer ticker.Stop()

	silenceLimit := 2*client.heartbeatInterval + client.heartbeatDuration

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		if since := time.Since(time.Unix(0, client.lastRecv.Load())); since > silenceLimit {
			client.connectionLost(conn, "no response from server",
				fmt.Errorf("nothing received for %s", since.Round(time.Millisecond)))
			return
		}

		if err := client.sendHeartbeat(ctx, conn); err != nil {
			if ctx.Err() != nil {
				return
			}
			client.connectionLost(conn, "heartbeat failed", err)
			return
		}
	}
}

// sendHeartbeat writes one heartbeat message to conn.
func (client *Client) sendHeartbeat(ctx context.Context, conn *websocket.Conn) error {
	heartbeat := HearbeatMsg{
		TemplateMsg: TemplateMsg{
			Event: HEARTBEAT_EVENT,
			Topic: "phoenix",
			Ref:   client.nextRef(),
		},
		Payload: struct{}{},
	}

	heartbeatCtx, cancel := context.WithTimeout(ctx, client.heartbeatDuration)
	defer cancel()

	client.logger.Debug("Sending heartbeat")
	return wsjson.Write(heartbeatCtx, conn, heartbeat)
}

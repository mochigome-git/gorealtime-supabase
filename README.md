# supabase-realtime-go

A Go client library for subscribing to [Supabase Realtime](https://supabase.com/docs/guides/realtime) WebSocket events and querying your database via the REST API.

## Installation

```bash
go get github.com/yourusername/supabase-realtime-go
```

## Requirements

- Go 1.22+
- A Supabase project ([create one here](https://supabase.com))

## Quick Start

```go
package main

import (
    "fmt"
    "log"

    realtime "github.com/yourusername/supabase-realtime-go"
    "go.uber.org/zap"
)

func main() {
    logger, _ := zap.NewProduction()

    client := realtime.CreateRealtimeClient("your-project-ref", "your-anon-key", logger)

    if err := client.Connect(); err != nil {
        log.Fatal(err)
    }
    defer client.Disconnect()

    err := client.ListenToPostgresChanges(realtime.PostgresChangesOptions{
        Schema: "public",
        Table:  "messages",
        Filter: "*",
    }, func(msg map[string]any) {
        fmt.Println("Change received:", msg)
    })
    if err != nil {
        log.Fatal(err)
    }

    // Block forever
    select {}
}
```

---

## API Reference

### Creating a Client

```go
client := realtime.CreateRealtimeClient(projectRef, apiKey, logger)
```

| Parameter    | Type          | Description                                             |
| ------------ | ------------- | ------------------------------------------------------- |
| `projectRef` | `string`      | Your Supabase project ref (e.g. `abcxyz`)               |
| `apiKey`     | `string`      | Your Supabase `anon` or `service_role` key              |
| `logger`     | `*zap.Logger` | A [zap](https://github.com/uber-go/zap) logger instance |

---

### Connecting & Disconnecting

```go
// Connect establishes the WebSocket connection
err := client.Connect()

// Disconnect gracefully closes the connection
err := client.Disconnect()
```

The client automatically handles:

- Heartbeats every 20 seconds
- Reconnection on connection loss with exponential backoff (up to 30s)
- Re-subscribing to all topics after a reconnect

---

### Listening to Postgres Changes

```go
err := client.ListenToPostgresChanges(realtime.PostgresChangesOptions{
    Schema: "public",
    Table:  "orders",
    Filter: "*",       // "INSERT", "UPDATE", "DELETE", or "*" for all
}, func(msg map[string]any) {
    // Handle the incoming change event
    fmt.Println(msg)
})
```

**`PostgresChangesOptions` fields:**

| Field    | Type     | Description                                             |
| -------- | -------- | ------------------------------------------------------- |
| `Schema` | `string` | Database schema (usually `"public"`)                    |
| `Table`  | `string` | Table name to listen on                                 |
| `Filter` | `string` | Event filter: `"*"`, `"INSERT"`, `"UPDATE"`, `"DELETE"` |

The handler receives a `map[string]any` containing the full Supabase Realtime message envelope. A typical payload looks like:

```json
{
  "event": "postgres_changes",
  "topic": "realtime:public:orders",
  "payload": {
    "data": {
      "schema": "public",
      "table": "orders",
      "commit_timestamp": "2024-01-01T00:00:00Z",
      "eventType": "INSERT",
      "new": { "id": "1", "status": "pending" },
      "old": {}
    }
  }
}
```

You can subscribe to **multiple tables** by calling `ListenToPostgresChanges` multiple times:

```go
client.ListenToPostgresChanges(realtime.PostgresChangesOptions{
    Schema: "public", Table: "orders", Filter: "INSERT",
}, handleOrders)

client.ListenToPostgresChanges(realtime.PostgresChangesOptions{
    Schema: "public", Table: "users", Filter: "*",
}, handleUsers)
```

---

### Querying via REST API

Use `QueryTable` for a one-off filtered fetch without needing a subscription:

```go
type Order struct {
    ID     string `json:"id"`
    Status string `json:"status"`
}

var orders []Order
_, err := client.QueryTable("orders", &orders, map[string]any{
    "status": "pending",
    "user_id": 42,
})
if err != nil {
    log.Fatal(err)
}

fmt.Println(orders)
```

All filters are applied as equality checks (`column=eq.value`). Multiple filters are ANDed together.

---

### Health Checks

```go
// Returns true if the WebSocket connection is active
alive := client.IsClientAlive()

// Also checks connection state
connected := client.IsConnected()
```

---

## Configuration Defaults

| Setting            | Default | Description                                                            |
| ------------------ | ------- | ---------------------------------------------------------------------- |
| Dial timeout       | 10s     | Max time to establish connection                                       |
| Heartbeat interval | 20s     | How often heartbeats are sent                                          |
| Heartbeat timeout  | 5s      | Max time to wait for heartbeat write                                   |
| Reconnect interval | 500ms   | Initial delay between reconnect attempts (doubles on failure, max 30s) |

---

## Error Handling

All errors are surfaced as standard Go errors. The client handles transient disconnections internally — your handlers will resume automatically after reconnection.

```go
if err := client.Connect(); err != nil {
    // Fatal: could not establish initial connection
    log.Fatal(err)
}

if err := client.ListenToPostgresChanges(opts, handler); err != nil {
    // Client was not connected when subscribing
    log.Println("subscribe error:", err)
}
```

---

## Dependencies

- [`github.com/coder/websocket`](https://github.com/coder/websocket) — WebSocket transport
- [`go.uber.org/zap`](https://github.com/uber-go/zap) — Structured logging

## License

MIT

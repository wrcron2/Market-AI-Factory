---
description: Conventions for Go HTTP, gRPC, WebSocket, and database code in this backend
---

# Go Backend API Patterns

## Server Setup

Dual HTTP (`:8080`) + gRPC (`:50051`) servers. Load `.env` early, init zap logger, open DB, run migrations, start WebSocket hub, then register handlers.

```go
_ = godotenv.Load()   // ignore error — env vars may be set externally

logger, _ := zap.NewProduction()
defer logger.Sync()

database, _ := db.Open(getEnv("DB_DSN", "./infra/db/marketflow.db"))
defer database.Close()
database.Migrate()

hub := ws.NewHub(logger)
go hub.Run()
```

Graceful shutdown always: listen for SIGINT/SIGTERM, call `grpcSrv.GracefulStop()` then `httpSrv.Shutdown(ctx)` with a 10-second timeout.

```go
quit := make(chan os.Signal, 1)
signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
<-quit

ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
defer cancel()

grpcSrv.GracefulStop()
httpSrv.Shutdown(ctx)
```

`getEnv` helper with fallback:

```go
func getEnv(key, fallback string) string {
    if v := os.Getenv(key); v != "" {
        return v
    }
    return fallback
}
```

## HTTP Handlers

Handler is a struct with injected dependencies. Constructor injects all deps.

```go
type Handler struct {
    db   *db.DB
    hub  *ws.Hub
    mode *mode.Manager
    log  *zap.Logger
}

func NewHandler(database *db.DB, hub *ws.Hub, modeManager *mode.Manager, log *zap.Logger) *Handler {
    return &Handler{db: database, hub: hub, mode: modeManager, log: log}
}
```

Handler method pattern:

```go
func (h *Handler) ListPending(w http.ResponseWriter, r *http.Request) {
    if r.Method != http.MethodGet {
        http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
        return
    }

    limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
    offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))

    orders, total, err := h.db.ListByStatus(db.StatusPending, limit, offset)
    if err != nil {
        h.log.Error("ListPending failed", zap.Error(err))
        http.Error(w, "internal error", http.StatusInternalServerError)
        return
    }

    writeJSON(w, map[string]any{"orders": orders, "total": total})
}

func writeJSON(w http.ResponseWriter, v any) {
    w.Header().Set("Content-Type", "application/json")
    json.NewEncoder(w).Encode(v)
}
```

Rules:
- Method guard at top of every handler
- Parse query params with `r.URL.Query().Get()` + type conversion
- Log errors with `h.log.Error("description", zap.Error(err))` before responding
- JSON response via `writeJSON` helper (sets Content-Type automatically)
- HTTP errors via `http.Error(w, message, statusCode)` — never `fmt.Fprintf`

## Request Body Decoding

Use inline structs per endpoint — no shared global request types.

```go
var req struct {
    SignalID string  `json:"signal_id"`
    Comment  string  `json:"comment"`
}
if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
    http.Error(w, "invalid request body", http.StatusBadRequest)
    return
}
if req.SignalID == "" {
    http.Error(w, "signal_id is required", http.StatusBadRequest)
    return
}
```

## Database Layer

`DB` wraps `*sql.DB` to add domain-specific methods.

```go
type DB struct{ *sql.DB }

func Open(dsn string) (*DB, error) {
    sqlDB, err := sql.Open("sqlite3", dsn+"?_journal_mode=WAL&_foreign_keys=on")
    if err != nil { return nil, fmt.Errorf("sql.Open: %w", err) }
    if err := sqlDB.Ping(); err != nil { return nil, fmt.Errorf("db.Ping: %w", err) }
    return &DB{sqlDB}, nil
}
```

Domain model struct with JSON tags:

```go
type StagedOrder struct {
    ID            string      `json:"id"`
    Symbol        string      `json:"symbol"`
    Direction     string      `json:"direction"`
    Quantity      float64     `json:"quantity"`
    LimitPrice    float64     `json:"limit_price"`
    Confidence    float64     `json:"confidence"`
    Status        OrderStatus `json:"status"`
    TraderComment string      `json:"trader_comment,omitempty"`
    IBKROrderID   *int64      `json:"ibkr_order_id,omitempty"`
    CreatedAt     int64       `json:"created_at"`
    UpdatedAt     int64       `json:"updated_at"`
}
```

State machine enforcement:

```go
func (d *DB) TransitionStatus(id string, to OrderStatus, actor, comment string) error {
    order, err := d.GetOrder(id)
    if err != nil { return fmt.Errorf("order not found: %w", err) }
    if !validTransition(order.Status, to) {
        return fmt.Errorf("invalid transition %s → %s", order.Status, to)
    }
    // ... update + audit log
}

func validTransition(from, to OrderStatus) bool {
    allowed := map[OrderStatus][]OrderStatus{
        StatusPending:  {StatusApproved, StatusRejected},
        StatusApproved: {StatusExecuted, StatusFailed},
    }
    for _, next := range allowed[from] { if next == to { return true } }
    return false
}
```

Rules:
- Always wrap errors: `fmt.Errorf("MethodName: %w", err)`
- Set `CreatedAt`/`UpdatedAt` to `time.Now().UnixMilli()` in the method (not the caller)
- Append audit log on every status change
- Use positional `?` placeholders for SQLite

## gRPC Services

Embed the unimplemented server to future-proof against new proto methods.

```go
type SignalServer struct {
    proto.UnimplementedSignalServiceServer
    db            *db.DB
    hub           *ws.Hub
    log           *zap.Logger
    minConfidence float64
}

func NewSignalServer(database *db.DB, hub *ws.Hub, log *zap.Logger) *SignalServer {
    minConf, _ := strconv.ParseFloat(os.Getenv("MIN_SIGNAL_CONFIDENCE"), 64)
    if minConf == 0 { minConf = 0.90 }
    return &SignalServer{db: database, hub: hub, log: log, minConfidence: minConf}
}
```

Method signature: `(ctx context.Context, req *proto.Request) (*proto.Response, error)`

Business logic rejection (not an error — returns response with `Accepted: false`):

```go
if req.Confidence < s.minConfidence {
    return &proto.SignalResponse{
        SignalId: req.SignalId,
        Accepted: false,
        Message:  fmt.Sprintf("confidence %.2f below threshold %.2f", req.Confidence, s.minConfidence),
    }, nil
}
```

Infrastructure errors use gRPC status codes:

```go
if err := s.db.StageOrder(order); err != nil {
    s.log.Error("failed to stage order", zap.Error(err))
    return nil, status.Errorf(codes.Internal, "failed to stage order: %v", err)
}
```

Broadcast on side effects:

```go
s.hub.Broadcast("order_staged", order)
```

## WebSocket Hub

```go
type Hub struct {
    mu       sync.RWMutex
    clients  map[*client]bool
    upgrader websocket.Upgrader
    log      *zap.Logger
}

type Message struct {
    Type    string `json:"type"`    // "order_staged", "order_approved", etc.
    Payload any    `json:"payload"`
}
```

Broadcast uses `select` with `default` to drop messages for slow clients rather than blocking:

```go
func (h *Hub) Broadcast(msgType string, payload any) {
    data, _ := json.Marshal(Message{Type: msgType, Payload: payload})
    h.mu.RLock()
    defer h.mu.RUnlock()
    for c := range h.clients {
        select {
        case c.send <- data:
        default:   // slow client — drop
        }
    }
}
```

Each client runs two goroutines: `readPump` (ping/pong, disconnect detection) and `writePump` (message delivery, 30s heartbeat pings). `readPump` blocks until disconnect, then cleans up.

## Thread-Safe Shared State

Use `sync.RWMutex`. `RLock` for reads, `Lock` for writes. Always `defer Unlock`.

```go
type Manager struct {
    mu      sync.RWMutex
    current TradingMode
    log     *zap.Logger
}

func (m *Manager) Get() TradingMode {
    m.mu.RLock()
    defer m.mu.RUnlock()
    return m.current
}

func (m *Manager) Set(mode TradingMode) {
    m.mu.Lock()
    defer m.mu.Unlock()
    prev := m.current
    m.current = mode
    m.log.Info("trading mode changed", zap.String("from", string(prev)), zap.String("to", string(mode)))
}
```

## Error Handling Summary

| Context      | Pattern                                                  |
|--------------|----------------------------------------------------------|
| Internal ops | `fmt.Errorf("MethodName: %w", err)`                      |
| HTTP handler | `h.log.Error(...); http.Error(w, msg, code)`             |
| gRPC handler | `return nil, status.Errorf(codes.Code, msg)`             |
| Business rule| Return response with `Accepted: false`, `nil` error      |
| State machine| `fmt.Errorf("invalid transition %s → %s", from, to)`    |

## Route Registration

Use `http.NewServeMux()`. Multi-method routes use a switch inside the handler func.

```go
mux.HandleFunc("/api/orders/pending", glHandler.ListPending)
mux.HandleFunc("/api/mode", func(w http.ResponseWriter, r *http.Request) {
    switch r.Method {
    case http.MethodGet:
        modeManager.GetHandler(w, r)
    case http.MethodPost:
        modeManager.SetHandler(w, r)
        hub.Broadcast("mode_changed", map[string]string{"mode": string(modeManager.Get())})
    default:
        http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
    }
})
```

Apply CORS middleware at the mux level (`corsMiddleware(mux)`), not per-handler.

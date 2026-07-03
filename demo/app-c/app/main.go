// app-c — the Go/pgx ambient-enrolled client and tail of the a->b->c chain.
//
// The third of three deliberately-distinct pool implementations (app-a is
// Node/TypeORM, app-b is Python/SQLAlchemy+psycopg3). Each holds its own
// pgbouncer-fronted pool so a later upgrade run can compare how the three
// clients observe and recover from the ztunnel-drain RST.
//
// pgbouncer runs pool_mode=transaction, so this pool is configured to never use
// server-side prepared statements or the statement/description caches (any of
// which would break when a transaction lands on a different Postgres backend):
//
//	DefaultQueryExecMode  = pgx.QueryExecModeExec  // server-side extended proto,
//	                                               // unnamed (never prepared)
//	StatementCacheCapacity   = 0                   // no prepared-statement cache
//	DescriptionCacheCapacity = 0                   // no row-description cache
//
// Endpoints: /healthz (liveness, no DB), /readyz (pool + SELECT 1), /query
// (widgets read; also the chain tail app-b calls), /hold (Acquire + park a
// pooled conn). A 20s keepalive pings a parked connection to hold one
// persistent, in-mesh, HBONE-carried app-c -> pgbouncer socket open.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

var (
	pool    *pgxpool.Pool
	poolMu  sync.RWMutex
	ready   bool
	readyMu sync.RWMutex

	// A single pooled connection pinned by GET /hold — the app-c analogue of
	// app-a's heldRunner / app-b's _held. Parks an idle pooled server socket.
	held   *pgxpool.Conn
	heldMu sync.Mutex
)

func setReady(v bool) {
	readyMu.Lock()
	ready = v
	readyMu.Unlock()
}

func isReady() bool {
	readyMu.RLock()
	defer readyMu.RUnlock()
	return ready
}

func getPool() *pgxpool.Pool {
	poolMu.RLock()
	defer poolMu.RUnlock()
	return pool
}

type widgetRow struct {
	ID        int32     `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

func buildPoolConfig() (*pgxpool.Config, error) {
	dsn := "host=" + env("DB_HOST", "pgbouncer-writer") +
		" port=" + env("DB_PORT", "5432") +
		" user=" + env("DB_USER", "demo_app") +
		" password=" + os.Getenv("DB_PASSWORD") +
		" dbname=" + env("DB_NAME", "demo") +
		" application_name=demo-app-c"

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}

	// Transaction-pooling safety: never leave a named prepared statement or a
	// cached statement/description on a backend that pgbouncer may hand to a
	// different client's next transaction.
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeExec
	cfg.ConnConfig.StatementCacheCapacity = 0
	cfg.ConnConfig.DescriptionCacheCapacity = 0

	cfg.MinConns = 2
	cfg.MaxConns = 5
	// Retire a socket before pgbouncer's server_lifetime=3600 does.
	cfg.MaxConnLifetime = 30 * time.Minute

	return cfg, nil
}

// connectWithRetry brings the pool up with indefinite exponential backoff. Like
// app-a/app-b, it never exits on failure: pgbouncer/Postgres may not be
// reachable yet at startup and a self-inflicted CrashLoopBackOff would confound
// the RST measurements. /readyz stays 503 until the pool is actually up.
func connectWithRetry() {
	base := 500 * time.Millisecond
	max := 8 * time.Second
	delay := base
	for attempt := 1; ; attempt++ {
		cfg, err := buildPoolConfig()
		if err == nil {
			var p *pgxpool.Pool
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			p, err = pgxpool.NewWithConfig(ctx, cfg)
			if err == nil {
				err = p.Ping(ctx)
			}
			cancel()
			if err == nil {
				poolMu.Lock()
				pool = p
				poolMu.Unlock()
				setReady(true)
				log.Println("app-c: pool initialized")
				return
			}
			if p != nil {
				p.Close()
			}
		}
		log.Printf("app-c: pool init failed (attempt %d), retrying in %s: %v", attempt, delay, err)
		time.Sleep(delay)
		if delay < max {
			delay *= 2
			if delay > max {
				delay = max
			}
		}
	}
}

// keepAliveLoop holds one dedicated pooled connection open and pings it every
// KEEPALIVE_MS. This is the persistent app-c -> pgbouncer connection the drill
// protects. Plain SELECT 1 under QueryExecModeExec — transaction-pooling safe.
func keepAliveLoop() {
	interval := time.Duration(mustAtoi(env("KEEPALIVE_MS", "20000"))) * time.Millisecond
	var conn *pgxpool.Conn
	for {
		time.Sleep(interval)
		p := getPool()
		if p == nil {
			continue
		}
		var err error
		if conn == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			conn, err = p.Acquire(ctx)
			cancel()
			if err != nil {
				log.Printf("app-c: keepalive acquire failed: %v", err)
				conn = nil
				continue
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, err = conn.Exec(ctx, "SELECT 1")
		cancel()
		if err != nil {
			log.Printf("app-c: keepalive query failed: %v", err)
			conn.Release()
			conn = nil
		}
	}
}

func mustAtoi(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 20000
		}
		n = n*10 + int(c-'0')
	}
	if n == 0 {
		return 20000
	}
	return n
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func readWidget(ctx context.Context, p *pgxpool.Pool) (*widgetRow, error) {
	var wr widgetRow
	err := p.QueryRow(ctx,
		"SELECT id, name, created_at FROM widgets ORDER BY id LIMIT 1").
		Scan(&wr.ID, &wr.Name, &wr.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &wr, nil
}

func handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

func handleReadyz(w http.ResponseWriter, r *http.Request) {
	p := getPool()
	if !isReady() || p == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ready": false, "reason": "pool not initialized"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if err := p.Ping(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ready": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ready": true})
}

func handleQuery(w http.ResponseWriter, r *http.Request) {
	p := getPool()
	if p == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"service": "app-c", "error": "pool not initialized"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	wr, err := readWidget(ctx, p)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"service": "app-c", "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"service": "app-c", "widget": wr})
}

// handleHold pins a long-lived pooled connection (Acquire + park) and proves it
// still queries. No transaction is left open, so pgbouncer's
// idle_transaction_timeout never kills it.
func handleHold(w http.ResponseWriter, r *http.Request) {
	p := getPool()
	if p == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"held": false, "error": "pool not initialized"})
		return
	}
	heldMu.Lock()
	defer heldMu.Unlock()
	if held == nil {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		c, err := p.Acquire(ctx)
		cancel()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"held": false, "error": err.Error()})
			return
		}
		held = c
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	var heldAt time.Time
	if err := held.QueryRow(ctx, "SELECT now()").Scan(&heldAt); err != nil {
		held.Release()
		held = nil
		writeJSON(w, http.StatusInternalServerError, map[string]any{"held": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"held": true, "heldAt": heldAt})
}

func main() {
	port := env("PORT", "8080")

	go connectWithRetry()
	go keepAliveLoop()

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", handleHealthz)
	mux.HandleFunc("/readyz", handleReadyz)
	mux.HandleFunc("/query", handleQuery)
	mux.HandleFunc("/hold", handleHold)

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("app-c listening on :%s", port)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("app-c server error: %v", err)
	}
}

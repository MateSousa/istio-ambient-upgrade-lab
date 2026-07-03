"""app-b — the Python/SQLAlchemy(+psycopg3) ambient-enrolled client.

The second of three deliberately-distinct pool implementations (app-a is
Node/TypeORM, app-c is Go/pgx). Each holds its own pgbouncer-fronted pool so a
later upgrade run can compare how the three clients observe and recover from the
ztunnel-drain RST.

Endpoints:
  GET /healthz  liveness — process only, never touches the DB, so a DB blip
                never restarts the pod.
  GET /readyz   readiness — engine initialised AND a SELECT 1 round-trips
                through pgbouncer to Postgres.
  GET /query    reads one widgets row (app -> pgbouncer -> Postgres).
  GET /hold     parks a long-lived pooled connection (no open transaction) and
                proves it can still query.
  GET /chain    own SELECT, then GET http://app-c:8080/query, forwarding the
                trace headers — the app-b -> app-c hop of the a->b->c chain.

Uvicorn runs a SINGLE worker so there is exactly one process owning one pool
(so the pool maths and the pgbouncer client count are deterministic). Route
handlers are plain `def` (not `async def`), so FastAPI runs each in the
threadpool and the blocking SQLAlchemy / httpx calls never stall the event loop.
"""
import os
import threading
import time
from contextlib import asynccontextmanager
from typing import Optional

import httpx
from fastapi import FastAPI, Request
from fastapi.responses import JSONResponse
from sqlalchemy import text
from sqlalchemy.engine import Connection, Engine

from db import make_engine

PORT = int(os.environ.get("PORT", "8000"))
KEEPALIVE_MS = int(os.environ.get("KEEPALIVE_MS", "20000"))
APPC_URL = os.environ.get("APPC_URL", "http://app-c:8080/query")
CHAIN_TIMEOUT_S = float(os.environ.get("CHAIN_TIMEOUT_MS", "3000")) / 1000.0
CONNECT_RETRY_BASE_MS = int(os.environ.get("CONNECT_RETRY_BASE_MS", "500"))
CONNECT_RETRY_MAX_MS = int(os.environ.get("CONNECT_RETRY_MAX_MS", "8000"))

# Trace-context headers propagated down the chain so the whole a->b->c request
# stitches into one distributed trace once the observability slice is wired up.
TRACE_HEADERS = ("traceparent", "tracestate", "x-request-id", "b3")

_engine: Optional[Engine] = None
_ready = False
# A single pooled connection pinned by GET /hold — the app-b analogue of app-a's
# heldRunner. AUTOCOMMIT so it never parks an idle-in-transaction backend.
_held: Optional[Connection] = None
_held_lock = threading.Lock()


def _autocommit(engine: Engine) -> Connection:
    return engine.connect().execution_options(isolation_level="AUTOCOMMIT")


def _connect_with_retry() -> None:
    """Bring the engine up with indefinite exponential backoff.

    Deliberately never exits on failure: pgbouncer/Postgres may not be
    reachable yet at startup, and a self-inflicted CrashLoopBackOff would
    confound the RST measurements this app exists to support. /readyz stays 503
    until the pool is actually up.
    """
    global _engine, _ready
    engine = make_engine()
    delay = CONNECT_RETRY_BASE_MS
    attempt = 0
    while True:
        attempt += 1
        try:
            with _autocommit(engine) as conn:
                conn.execute(text("SELECT 1"))
            _engine = engine
            _ready = True
            print("app-b: engine initialized", flush=True)
            return
        except Exception as err:  # noqa: BLE001 — retry on anything, never crash
            print(
                f"app-b: engine init failed (attempt {attempt}), "
                f"retrying in {delay}ms: {err}",
                flush=True,
            )
            time.sleep(delay / 1000.0)
            delay = min(delay * 2, CONNECT_RETRY_MAX_MS)


def _keepalive_loop() -> None:
    """Hold one dedicated pooled connection open and ping it every KEEPALIVE_MS.

    This is the persistent, in-mesh app-b -> pgbouncer connection the drill
    exists to protect. Simple SELECT 1 under AUTOCOMMIT — no transaction, no
    prepared statement — keeps it transaction-pooling safe. Reconnects on any
    failure rather than exiting.
    """
    conn: Optional[Connection] = None
    while True:
        time.sleep(KEEPALIVE_MS / 1000.0)
        if _engine is None:
            continue
        try:
            if conn is None:
                conn = _autocommit(_engine)
            conn.execute(text("SELECT 1"))
        except Exception as err:  # noqa: BLE001
            print(f"app-b: keepalive query failed: {err}", flush=True)
            try:
                if conn is not None:
                    conn.close()
            except Exception:  # noqa: BLE001
                pass
            conn = None


@asynccontextmanager
async def lifespan(_app: FastAPI):
    # Connect (with retry) and start the keepalive in background threads so the
    # server answers /healthz immediately regardless of DB reachability.
    threading.Thread(target=_connect_with_retry, daemon=True).start()
    threading.Thread(target=_keepalive_loop, daemon=True).start()
    yield


app = FastAPI(lifespan=lifespan)


@app.get("/healthz")
def healthz() -> JSONResponse:
    return JSONResponse({"status": "ok"})


@app.get("/readyz")
def readyz() -> JSONResponse:
    if not _ready or _engine is None:
        return JSONResponse(
            {"ready": False, "reason": "engine not initialized"}, status_code=503
        )
    try:
        with _autocommit(_engine) as conn:
            conn.execute(text("SELECT 1"))
        return JSONResponse({"ready": True})
    except Exception as err:  # noqa: BLE001
        return JSONResponse({"ready": False, "error": str(err)}, status_code=503)


def _read_widget(conn: Connection) -> Optional[dict]:
    row = conn.execute(
        text("SELECT id, name, created_at FROM widgets ORDER BY id LIMIT 1")
    ).mappings().first()
    if row is None:
        return None
    return {"id": row["id"], "name": row["name"], "created_at": str(row["created_at"])}


@app.get("/query")
def query() -> JSONResponse:
    if _engine is None:
        return JSONResponse({"error": "engine not initialized"}, status_code=503)
    try:
        with _autocommit(_engine) as conn:
            return JSONResponse({"service": "app-b", "widget": _read_widget(conn)})
    except Exception as err:  # noqa: BLE001
        return JSONResponse({"service": "app-b", "error": str(err)}, status_code=500)


@app.get("/hold")
def hold() -> JSONResponse:
    """Pin a long-lived pooled connection and prove it still queries.

    Like app-a's /hold: parks an idle pooled client (AUTOCOMMIT — no open
    transaction, so pgbouncer's idle_transaction_timeout never kills it).
    """
    global _held
    if _engine is None:
        return JSONResponse({"held": False, "error": "engine not initialized"}, status_code=503)
    try:
        with _held_lock:
            if _held is None:
                _held = _autocommit(_engine)
            held_at = _held.execute(text("SELECT now() AS held_at")).scalar()
        return JSONResponse({"held": True, "heldAt": str(held_at)})
    except Exception as err:  # noqa: BLE001
        with _held_lock:
            _held = None
        return JSONResponse({"held": False, "error": str(err)}, status_code=500)


def _forward_headers(req: Request) -> dict:
    out = {}
    for h in TRACE_HEADERS:
        v = req.headers.get(h)
        if v:
            out[h] = v
    return out


@app.get("/chain")
def chain(request: Request) -> JSONResponse:
    """Own SELECT, then call app-c's /query, forwarding the trace headers.

    The app-b -> app-c hop of the load -> app-a -> app-b -> app-c chain. Both
    east-west hops traverse the mesh (L4 + mTLS via ztunnel, L7 via the tenant
    waypoint once the Service carries istio.io/use-waypoint).
    """
    if _engine is None:
        return JSONResponse({"service": "app-b", "error": "engine not initialized"}, status_code=503)
    try:
        with _autocommit(_engine) as conn:
            widget = _read_widget(conn)
    except Exception as err:  # noqa: BLE001
        return JSONResponse({"service": "app-b", "error": str(err)}, status_code=500)
    try:
        with httpx.Client(timeout=CHAIN_TIMEOUT_S) as client:
            resp = client.get(APPC_URL, headers=_forward_headers(request))
            downstream = resp.json()
    except Exception as err:  # noqa: BLE001
        return JSONResponse(
            {"service": "app-b", "widget": widget, "downstream_error": str(err)},
            status_code=502,
        )
    return JSONResponse({"service": "app-b", "widget": widget, "downstream": downstream})


if __name__ == "__main__":
    import uvicorn

    # SINGLE worker: one process, one pool, deterministic pgbouncer client count.
    uvicorn.run(app, host="0.0.0.0", port=PORT, workers=1)

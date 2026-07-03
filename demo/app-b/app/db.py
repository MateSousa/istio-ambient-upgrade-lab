"""SQLAlchemy engine for app-b, pointed at the pgbouncer WRITER service.

pgbouncer runs pool_mode=transaction, so this app must never leave a
transaction open across requests and must never rely on server-side prepared
statements (a prepared statement lives on one Postgres backend, but transaction
pooling hands each transaction a different backend). Two settings enforce that:

  * connect_args={"prepare_threshold": None} disables psycopg3's automatic
    server-side prepared statements entirely (default is to prepare after 5
    executions of the same query) — the psycopg3 analogue of app-a's
    simple-query-only path. This is the single most important knob for
    transaction-pooling safety.
  * every connection runs AUTOCOMMIT (see get_conn / keepalive), so a SELECT
    never parks an idle-in-transaction backend that pgbouncer's
    idle_transaction_timeout=60 would kill.

The QueuePool holds a bounded set of long-lived app -> pgbouncer client sockets
(pool_size 5 + max_overflow 2). Those persistent, in-mesh, HBONE-carried
connections are the unit a later slice watches get RST when the old ztunnel
drains during an upgrade. pool_pre_ping validates a pooled socket before handing
it out (so a silently-dropped connection surfaces as a reconnect, not an error);
pool_recycle 1800 proactively retires a socket before pgbouncer's
server_lifetime=3600 does.
"""
import os

from sqlalchemy import create_engine
from sqlalchemy.engine import Engine

DB_HOST = os.environ.get("DB_HOST", "pgbouncer-writer")
DB_PORT = os.environ.get("DB_PORT", "5432")
DB_USER = os.environ.get("DB_USER", "demo_app")
DB_PASSWORD = os.environ.get("DB_PASSWORD", "")
DB_NAME = os.environ.get("DB_NAME", "demo")


def make_engine() -> Engine:
    url = (
        f"postgresql+psycopg://{DB_USER}:{DB_PASSWORD}"
        f"@{DB_HOST}:{DB_PORT}/{DB_NAME}"
    )
    return create_engine(
        url,
        pool_size=5,
        max_overflow=2,
        pool_pre_ping=True,
        pool_recycle=1800,
        # psycopg3 connect() kwargs. prepare_threshold=None disables server-side
        # prepared statements (transaction-pooling safe). application_name is a
        # libpq param forwarded into the conninfo so the client is identifiable
        # in pg_stat_activity / pgbouncer SHOW CLIENTS.
        connect_args={
            "prepare_threshold": None,
            "application_name": "demo-app-b",
        },
    )

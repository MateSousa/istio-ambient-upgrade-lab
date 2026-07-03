import "reflect-metadata";
import express, { Request, Response } from "express";
import { QueryRunner } from "typeorm";
import { AppDataSource } from "./data-source";

const PORT = Number(process.env.PORT ?? "3000");
const KEEPALIVE_MS = Number(process.env.KEEPALIVE_MS ?? "20000");

// Chain config: app-a is the HEAD of the a->b->c chain. GET /chain does its own
// SELECT, then calls app-b's /chain (which in turn calls app-c). ~4s timeout so
// app-a's budget comfortably outlasts app-b's ~3s call to app-c.
const APPB_URL = process.env.APPB_URL ?? "http://app-b:8000/chain";
const CHAIN_TIMEOUT_MS = Number(process.env.CHAIN_TIMEOUT_MS ?? "4000");

// Trace-context headers forwarded down the chain so the whole a->b->c request
// stitches into one distributed trace once observability is wired up.
const TRACE_HEADERS = ["traceparent", "tracestate", "x-request-id", "b3"];

const app = express();

let initialized = false;
// A QueryRunner pinned by GET /hold. Holding a checked-out client keeps a
// dedicated app -> pgbouncer client socket open. We intentionally do NOT keep a
// transaction open on it (BEGIN without COMMIT would trip pgbouncer's
// idle_transaction_timeout=60 and get the connection killed) — it just parks
// an idle pooled client.
let heldRunner: QueryRunner | null = null;

// Liveness: process is up. Deliberately does NOT touch the database, so a
// transient DB blip never restarts the pod.
app.get("/healthz", (_req: Request, res: Response) => {
  res.status(200).json({ status: "ok" });
});

// Readiness: DataSource initialized AND a round-trip through pgbouncer to
// Postgres succeeds.
app.get("/readyz", async (_req: Request, res: Response) => {
  if (!initialized) {
    res.status(503).json({ ready: false, reason: "datasource not initialized" });
    return;
  }
  try {
    await AppDataSource.query("SELECT 1");
    res.status(200).json({ ready: true });
  } catch (err) {
    res.status(503).json({ ready: false, error: String(err) });
  }
});

// End-to-end proof: raw read of a widgets row, app -> pgbouncer -> Postgres.
app.get("/query", async (_req: Request, res: Response) => {
  try {
    const rows = await AppDataSource.query(
      "SELECT id, name, created_at FROM widgets ORDER BY id LIMIT 1"
    );
    res.status(200).json({ widget: rows[0] ?? null });
  } catch (err) {
    res.status(500).json({ error: String(err) });
  }
});

// Chain head: own SELECT, then call app-b's /chain (app-b then calls app-c),
// forwarding the trace headers. Both east-west hops traverse the mesh (L4 + mTLS
// via ztunnel, L7 via the tenant waypoint once the Services carry
// istio.io/use-waypoint). Returns the combined a+b+c result.
app.get("/chain", async (req: Request, res: Response) => {
  let widget: unknown = null;
  try {
    const rows = await AppDataSource.query(
      "SELECT id, name, created_at FROM widgets ORDER BY id LIMIT 1"
    );
    widget = rows[0] ?? null;
  } catch (err) {
    res.status(500).json({ service: "app-a", error: String(err) });
    return;
  }

  const headers: Record<string, string> = {};
  for (const h of TRACE_HEADERS) {
    const v = req.header(h);
    if (typeof v === "string" && v.length > 0) headers[h] = v;
  }

  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), CHAIN_TIMEOUT_MS);
  try {
    const resp = await fetch(APPB_URL, { headers, signal: controller.signal });
    const downstream = await resp.json();
    res.status(200).json({ service: "app-a", widget, downstream });
  } catch (err) {
    res
      .status(502)
      .json({ service: "app-a", widget, downstream_error: String(err) });
  } finally {
    clearTimeout(timer);
  }
});

// Pin a long-lived pooled client and prove it can still query.
app.get("/hold", async (_req: Request, res: Response) => {
  try {
    if (!heldRunner) {
      heldRunner = AppDataSource.createQueryRunner();
      await heldRunner.connect();
    }
    const rows = await heldRunner.query("SELECT now() AS held_at");
    res.status(200).json({ held: true, heldAt: rows[0]?.held_at ?? null });
  } catch (err) {
    res.status(500).json({ held: false, error: String(err) });
  }
});

const CONNECT_RETRY_BASE_MS = Number(process.env.CONNECT_RETRY_BASE_MS ?? "500");
const CONNECT_RETRY_MAX_MS = Number(process.env.CONNECT_RETRY_MAX_MS ?? "8000");

const sleep = (ms: number): Promise<void> =>
  new Promise((resolve) => setTimeout(resolve, ms));

// Bring up the DataSource with indefinite exponential backoff. We deliberately
// never exit on failure: the out-of-mesh Postgres / in-mesh pgbouncer path may
// not be reachable yet at startup, and a self-inflicted CrashLoopBackOff would
// confound the RST measurements this app exists to support. /readyz stays 503
// (initialized === false) until the pool is actually up.
async function connectWithRetry(): Promise<void> {
  let delay = CONNECT_RETRY_BASE_MS;
  for (let attempt = 1; ; attempt++) {
    try {
      await AppDataSource.initialize();
      initialized = true;
      console.log("data source initialized");
      return;
    } catch (err) {
      console.error(
        `data source init failed (attempt ${attempt}), retrying in ${delay}ms:`,
        err
      );
      await sleep(delay);
      delay = Math.min(delay * 2, CONNECT_RETRY_MAX_MS);
    }
  }
}

async function main(): Promise<void> {
  // Start serving immediately so /healthz (liveness) is up regardless of DB
  // reachability. Readiness (/readyz) is separately gated on `initialized`.
  app.listen(PORT, () => console.log(`demo-app-a listening on :${PORT}`));

  await connectWithRetry();

  // Background keep-alive: hold a dedicated pooled client open and ping it on
  // an interval. This is the persistent, in-mesh app -> pgbouncer connection
  // the drill exists to protect. Simple SELECT 1 (no transaction, no prepared
  // statement) keeps it transaction-pooling safe.
  const keepAliveRunner = AppDataSource.createQueryRunner();
  await keepAliveRunner.connect();
  setInterval(async () => {
    try {
      await keepAliveRunner.query("SELECT 1");
    } catch (err) {
      console.error("keepalive query failed:", err);
    }
  }, KEEPALIVE_MS);
}

main().catch((err) => {
  console.error("fatal startup error:", err);
  process.exit(1);
});

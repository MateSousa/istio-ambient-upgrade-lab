import "reflect-metadata";
import { DataSource } from "typeorm";

// Single TypeORM DataSource pointed at the pgbouncer WRITER service. pgbouncer
// runs pool_mode=transaction, so this app must never rely on session state or
// server-side prepared statements — every query goes through raw
// dataSource.query() with the simple query protocol (see index.ts). A small
// client-side pool (extra.max) with TCP keep-alive holds sockets open across
// time; that long-lived app -> pgbouncer connection is the unit later slices
// watch get RST when the old ztunnel drains during an upgrade.
export const AppDataSource = new DataSource({
  type: "postgres",
  host: process.env.DB_HOST ?? "pgbouncer-writer",
  port: Number(process.env.DB_PORT ?? "5432"),
  username: process.env.DB_USER ?? "demo_app",
  password: process.env.DB_PASSWORD ?? "",
  database: process.env.DB_NAME ?? "demo",
  // No schema management and no migrations: the schema is owned by the
  // out-of-mesh Postgres init script, not this app.
  synchronize: false,
  migrationsRun: false,
  entities: [],
  migrations: [],
  applicationName: "demo-app-a",
  extra: {
    // Deliberately small: each client here maps to a pgbouncer client
    // connection, and pgbouncer fans a bounded set of server connections out
    // to Postgres. keepAlive keeps the underlying TCP sockets pinned open.
    max: 5,
    keepAlive: true,
  },
});

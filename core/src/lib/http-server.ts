import { randomBytes } from "node:crypto";
import { createServer, type IncomingMessage, type ServerResponse } from "node:http";
import type { AddressInfo } from "node:net";
import type { Logger } from "./logger.js";

export const requestIdHeader = "x-request-id";

const validRequestId = /^[A-Za-z0-9._-]{1,64}$/;

export type RequestContext = Readonly<{ logger: Logger; requestId: string }>;

export type Route = (
  req: IncomingMessage,
  res: ServerResponse,
  ctx: RequestContext,
) => Promise<void> | void;

export type HttpServer = Readonly<{
  port: number;
  close(graceMs?: number): Promise<void>;
}>;

export type HttpServerOptions = Readonly<{
  host: string;
  port: number;
  logger: Logger;
  routes: Readonly<Record<string, Route>>;
}>;

export function sendJson(res: ServerResponse, status: number, body: unknown): void {
  res
    .writeHead(status, { "content-type": "application/json", "cache-control": "no-store" })
    .end(`${JSON.stringify(body)}\n`);
}

function requestIdFrom(req: IncomingMessage): string {
  const incoming = req.headers[requestIdHeader];
  return typeof incoming === "string" && validRequestId.test(incoming)
    ? incoming
    : randomBytes(12).toString("hex");
}

export async function startHttpServer(options: HttpServerOptions): Promise<HttpServer> {
  const { logger, routes } = options;

  const server = createServer(async (req, res) => {
    const started = performance.now();
    const requestId = requestIdFrom(req);
    const log = logger.child({ request_id: requestId });
    res.setHeader(requestIdHeader, requestId);
    const path = new URL(req.url ?? "/", "http://localhost").pathname;

    try {
      const route = Object.hasOwn(routes, path) ? routes[path] : undefined;
      if (route) await route(req, res, { logger: log, requestId });
      else res.writeHead(404).end();
    } catch (err) {
      log.error({ err, path }, "request failed");
      if (!res.headersSent) sendJson(res, 500, { error: "internal error" });
      else res.destroy();
    }

    const status = res.statusCode;
    const level = status >= 500 ? "error" : path === "/health" ? "debug" : "info";
    log[level](
      { method: req.method, path, status, duration_ms: Math.round(performance.now() - started) },
      "http request",
    );
  });
  server.headersTimeout = 5_000;
  server.requestTimeout = 10_000;
  server.keepAliveTimeout = 5_000;

  await new Promise<void>((resolve, reject) => {
    server.once("error", reject);
    server.listen(options.port, options.host, () => {
      server.off("error", reject);
      resolve();
    });
  });

  return Object.freeze({
    port: (server.address() as AddressInfo).port,
    close: (graceMs = 10_000) =>
      new Promise<void>((resolve, reject) => {
        const force = setTimeout(() => server.closeAllConnections(), graceMs);
        force.unref();
        server.close((error) => {
          clearTimeout(force);
          if (error) reject(error);
          else resolve();
        });
        server.closeIdleConnections();
      }),
  });
}

import type pg from "pg";
import type {
  ConnectionState,
  GatewayEvent,
  MessageStored,
  SendResult,
} from "../../contracts/index.js";
import type { Logger } from "../../lib/logger.js";
import type { AfterCommit, EventHandler } from "./consumer.js";

export type MessageListener = (
  message: MessageStored,
  tx: pg.ClientBase,
) => Promise<AfterCommit | undefined>;

export type SendResultListener = (result: SendResult) => void;

export type GatewayEvents = Readonly<{
  handle: EventHandler;
  onMessage(listener: MessageListener): () => void;
  onSendResult(requestId: string, listener: SendResultListener): () => void;
}>;

export type Connection = Readonly<{
  state: ConnectionState["state"];
  reason?: string;
  ownJid?: string;
  occurredAt: Date;
}>;

export async function readConnection(db: pg.Pool | pg.ClientBase): Promise<Connection | undefined> {
  const { rows } = await db.query<{
    state: ConnectionState["state"];
    reason: string | null;
    own_jid: string | null;
    occurred_at: Date;
  }>("SELECT state, reason, own_jid, occurred_at FROM connection_state");
  const row = rows[0];
  if (!row) return undefined;
  return Object.freeze({
    state: row.state,
    occurredAt: row.occurred_at,
    ...(row.reason ? { reason: row.reason } : {}),
    ...(row.own_jid ? { ownJid: row.own_jid } : {}),
  });
}

export function createGatewayEvents(logger: Logger): GatewayEvents {
  const messageListeners = new Set<MessageListener>();
  const sendListeners = new Map<string, Set<SendResultListener>>();

  async function connectionChanged(
    data: ConnectionState,
    occurredAt: string,
    tx: pg.ClientBase,
  ): Promise<AfterCommit | undefined> {
    const { rowCount } = await tx.query(
      `INSERT INTO connection_state (state, reason, own_jid, occurred_at) VALUES ($1, $2, $3, $4)
       ON CONFLICT (only_row) DO UPDATE
       SET state = EXCLUDED.state, reason = EXCLUDED.reason, own_jid = EXCLUDED.own_jid,
           occurred_at = EXCLUDED.occurred_at, updated_at = now()
       WHERE connection_state.occurred_at <= EXCLUDED.occurred_at`,
      [data.state, data.reason ?? null, data.own_jid ?? null, occurredAt],
    );
    if (!rowCount) return undefined;
    return () =>
      logger.info({ state: data.state, reason: data.reason }, "whatsapp connection changed");
  }

  return Object.freeze({
    async handle(event: GatewayEvent, tx: pg.ClientBase) {
      switch (event.type) {
        case "connection.state":
          return connectionChanged(event.data, event.occurred_at, tx);
        case "message.stored": {
          const followUps: AfterCommit[] = [];
          for (const listener of messageListeners) {
            const followUp = await listener(event.data, tx);
            if (followUp) followUps.push(followUp);
          }
          const { message_id, chat_id, kind, change, source } = event.data;
          logger.debug({ message_id, chat_id, kind, change, source }, "message event handled");
          return () => {
            for (const followUp of followUps) followUp();
          };
        }
        case "send.result": {
          const result = event.data;
          return () => {
            for (const listener of sendListeners.get(result.request_id) ?? []) listener(result);
          };
        }
        case "history.synced": {
          const { chats, messages, progress, complete } = event.data;
          return () => logger.info({ chats, messages, progress, complete }, "history batch stored");
        }
      }
    },
    onMessage(listener: MessageListener) {
      messageListeners.add(listener);
      return () => messageListeners.delete(listener);
    },
    onSendResult(requestId: string, listener: SendResultListener) {
      const listeners = sendListeners.get(requestId) ?? new Set();
      sendListeners.set(requestId, listeners);
      listeners.add(listener);
      return () => {
        listeners.delete(listener);
        if (listeners.size === 0) sendListeners.delete(requestId);
      };
    },
  });
}

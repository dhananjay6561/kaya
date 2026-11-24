import type pg from "pg";
import { requestIdFor } from "../../providers/channel/index.js";
import type { Exchange } from "./context.js";

export type ConversationOptions = Readonly<{
  before: Date;
  excludeWaId?: string;
  withinMs?: number;
  maxExchanges?: number;
}>;

export const chatAnswerKey = (commandWaId: string) => `command:${commandWaId}:chat`;

/** DJ's recent questions in his own chat that Kaya answered, with the answers, oldest first. */
export async function readConversation(
  pool: pg.Pool,
  options: ConversationOptions,
): Promise<Exchange[]> {
  const withinMs = options.withinMs ?? 12 * 60 * 60_000;
  const maxExchanges = options.maxExchanges ?? 20;
  const { rows: asked } = await pool.query<{ wa_id: string; sent_at: Date; body: string }>(
    `SELECT c.wa_id, c.sent_at, m.body
     FROM commands c
     JOIN messages m ON m.id = c.message_id
     WHERE c.state = 'done'
       AND c.sent_at < $1 AND c.sent_at >= $2
       AND c.wa_id IS DISTINCT FROM $3
       AND m.deleted_at IS NULL AND m.body IS NOT NULL
     ORDER BY c.sent_at DESC, c.id DESC
     LIMIT $4`,
    [
      options.before,
      new Date(options.before.getTime() - withinMs),
      options.excludeWaId ?? null,
      maxExchanges,
    ],
  );
  if (asked.length === 0) return [];
  const ids = asked.map((row) => requestIdFor(chatAnswerKey(row.wa_id)));
  const { rows: answered } = await pool.query<{ id: string; text: string }>(
    `SELECT id::text, payload -> 'content' ->> 'text' AS text
     FROM send_requests WHERE id = ANY($1::uuid[]) AND state = 'sent'`,
    [ids],
  );
  const answers = new Map(answered.map((row) => [row.id, row.text]));
  return asked
    .flatMap((row, i) => {
      const answer = answers.get(ids[i] ?? "");
      return answer ? [{ question: row.body.trim(), answer, at: row.sent_at }] : [];
    })
    .reverse();
}

import type pg from "pg";
import type { Exchange } from "./context.js";

export type ConversationOptions = Readonly<{
  /** WhatsApp id of the question being answered: only exchanges before it are read. */
  before: string;
  sentAt: Date;
  withinMs?: number;
  maxExchanges?: number;
}>;

/** DJ's recent questions in his own chat with the answers Kaya sent, oldest first. */
export async function readConversation(
  pool: pg.Pool,
  options: ConversationOptions,
): Promise<Exchange[]> {
  const withinMs = options.withinMs ?? 12 * 60 * 60_000;
  const { rows } = await pool.query<{ question: string; answer: string; sent_at: Date }>(
    `SELECT q.body AS question, a.body AS answer, c.sent_at
     FROM commands c
     JOIN messages q ON q.id = c.message_id
     JOIN messages a ON a.chat_id = q.chat_id AND a.wa_id = c.answer_wa_id AND a.from_me
     WHERE c.answer_wa_id IS NOT NULL
       AND c.id < coalesce((SELECT min(id) FROM commands WHERE wa_id = $1), 'Infinity'::numeric)
       AND c.sent_at >= $2
       AND q.deleted_at IS NULL AND a.deleted_at IS NULL
       AND q.body IS NOT NULL AND a.body IS NOT NULL
       AND (q.edited_at IS NULL OR q.edited_at <= c.finished_at)
     ORDER BY c.id DESC
     LIMIT $3`,
    [options.before, new Date(options.sentAt.getTime() - withinMs), options.maxExchanges ?? 20],
  );
  return rows
    .map((row) => ({ question: row.question.trim(), answer: row.answer, at: row.sent_at }))
    .reverse();
}

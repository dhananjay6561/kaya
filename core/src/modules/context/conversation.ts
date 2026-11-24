import type pg from "pg";
import type { Exchange } from "./context.js";

export type ConversationOptions = Readonly<{ maxExchanges?: number }>;

/** The latest exchanges between DJ and Kaya, oldest first, however long ago they were. */
export async function readConversation(
  pool: pg.Pool,
  options: ConversationOptions = {},
): Promise<Exchange[]> {
  const { rows } = await pool.query<{ question: string; answer: string; sent_at: Date }>(
    `SELECT q.body AS question, a.body AS answer, q.sent_at
     FROM exchanges e
     JOIN messages q ON q.id = e.question_message_id
     JOIN messages a ON a.chat_id = q.chat_id AND a.wa_id = e.answer_wa_id AND a.from_me
     WHERE q.deleted_at IS NULL AND a.deleted_at IS NULL
       AND q.body IS NOT NULL AND a.body IS NOT NULL
       AND (q.edited_at IS NULL OR q.edited_at <= e.created_at)
     ORDER BY e.id DESC
     LIMIT $1`,
    [options.maxExchanges ?? 50],
  );
  return rows
    .map((row) => ({ question: row.question.trim(), answer: row.answer, at: row.sent_at }))
    .reverse();
}

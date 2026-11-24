-- migrate:up
CREATE TABLE exchanges (
  id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  question_message_id bigint NOT NULL UNIQUE,
  answer_wa_id text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);

COMMENT ON TABLE exchanges IS 'Kaya''s conversation memory: one row per question DJ asked in his own chat and the answer Kaya sent for it, in the order they were answered. Kept for good, unlike commands. The text stays in messages; this table only links the two.';
COMMENT ON COLUMN exchanges.question_message_id IS 'messages.id of DJ''s question. Not a foreign key: the chat tables belong to the gateway.';
COMMENT ON COLUMN exchanges.answer_wa_id IS 'WhatsApp id of Kaya''s answer, a message in the same chat';

INSERT INTO exchanges (question_message_id, answer_wa_id, created_at)
SELECT message_id, answer_wa_id, coalesce(finished_at, created_at) FROM commands
WHERE answer_wa_id IS NOT NULL
ORDER BY id;

COMMENT ON COLUMN commands.answer_wa_id IS 'Replaced by exchanges; no longer written. Kept only so a core from before exchanges keeps working during a deploy; dropped in a later release.';

GRANT SELECT, INSERT, UPDATE, DELETE ON exchanges TO kaya_core;

-- migrate:down
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM exchanges) THEN
    RAISE EXCEPTION 'exchanges holds conversation memory; copy or empty it before rolling back';
  END IF;
END
$$;
DROP TABLE exchanges;
COMMENT ON COLUMN commands.answer_wa_id IS 'WhatsApp id of the answer Kaya sent when the command was a question it answered; the question and this answer are one exchange of the conversation memory. Null for slash commands, notices and anything not answered.';

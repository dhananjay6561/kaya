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
SELECT message_id, answer_wa_id, finished_at FROM commands
WHERE answer_wa_id IS NOT NULL
ORDER BY id;

DROP INDEX commands_answered_idx;
ALTER TABLE commands DROP COLUMN answer_wa_id;

GRANT SELECT, INSERT, UPDATE, DELETE ON exchanges TO kaya_core;

-- migrate:down
ALTER TABLE commands ADD COLUMN answer_wa_id text;
CREATE INDEX commands_answered_idx ON commands (id) WHERE answer_wa_id IS NOT NULL;
UPDATE commands c SET answer_wa_id = e.answer_wa_id FROM exchanges e WHERE e.question_message_id = c.message_id;
DROP TABLE exchanges;

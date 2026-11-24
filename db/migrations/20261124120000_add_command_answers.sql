-- migrate:up
ALTER TABLE commands ADD COLUMN answer_wa_id text;
COMMENT ON COLUMN commands.answer_wa_id IS 'WhatsApp id of the answer Kaya sent when the command was a question it answered; the question and this answer are one exchange of the conversation memory. Null for slash commands, notices and anything not answered.';

CREATE INDEX commands_answered_idx ON commands (id) WHERE answer_wa_id IS NOT NULL;

-- migrate:down
DROP INDEX commands_answered_idx;
ALTER TABLE commands DROP COLUMN answer_wa_id;

-- migrate:up
ALTER TABLE messages ADD COLUMN forwarded boolean NOT NULL DEFAULT false;
COMMENT ON COLUMN messages.forwarded IS 'The sender passed on someone else''s message instead of writing it. False for everything stored before this column existed.';

CREATE TABLE commands (
  id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  message_id bigint NOT NULL UNIQUE,
  wa_id text NOT NULL,
  sent_at timestamptz NOT NULL,
  state text NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'done', 'failed')),
  attempts int NOT NULL DEFAULT 0 CHECK (attempts >= 0),
  error text,
  created_at timestamptz NOT NULL DEFAULT now(),
  finished_at timestamptz,
  CHECK ((state = 'pending') = (finished_at IS NULL)),
  CHECK ((state = 'failed') = (error IS NOT NULL))
);

COMMENT ON TABLE commands IS 'Core''s inbox of things DJ told Kaya to do: one row per message DJ typed in his own chat. Core adds the row in the same transaction that marks the message.stored event handled, then works through pending rows oldest first, one at a time. A row is added once per message, so a redelivered event never runs a command twice. The text stays in messages; this table only points at it.';
COMMENT ON COLUMN commands.message_id IS 'messages.id of the message that is the command. Not a foreign key: the chat tables belong to the gateway.';
COMMENT ON COLUMN commands.attempts IS 'How many times core started on this command. One that keeps being interrupted is given up on instead of being started for ever.';
COMMENT ON COLUMN commands.error IS 'Why a failed command failed: a short code, never message text';

CREATE INDEX commands_pending_idx ON commands (id) WHERE state = 'pending';
CREATE INDEX commands_finished_idx ON commands (finished_at) WHERE finished_at IS NOT NULL;

GRANT SELECT, INSERT, UPDATE, DELETE ON commands TO kaya_core;

-- migrate:down
DROP TABLE commands;
ALTER TABLE messages DROP COLUMN forwarded;

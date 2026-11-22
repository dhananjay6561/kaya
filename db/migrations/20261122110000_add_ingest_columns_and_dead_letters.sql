-- migrate:up
ALTER TABLE chats ADD COLUMN name_set_at timestamptz;
COMMENT ON COLUMN chats.name_set_at IS 'When WhatsApp says the name was set. A name is replaced only by a newer one, so an old copy from the history sync never overwrites a live rename.';

ALTER TABLE messages
  ADD COLUMN view_once boolean NOT NULL DEFAULT false,
  ADD COLUMN expires_at timestamptz;
COMMENT ON COLUMN messages.view_once IS 'Sent as view once. WhatsApp gives linked devices no content for these, so the row records that one was sent.';
COMMENT ON COLUMN messages.expires_at IS 'When a disappearing message expires in the chat. The row is kept; nothing deletes on this yet.';

CREATE TABLE ingest_dead (
  id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  wa_id text,
  chat_jid text,
  sender_jid text,
  error_code text NOT NULL CHECK (error_code <> ''),
  info jsonb,
  raw bytea,
  created_at timestamptz NOT NULL DEFAULT now()
);

COMMENT ON TABLE ingest_dead IS 'Messages the gateway received but could not store (a constraint it did not expect, or a bug in the mapper). WhatsApp delivers a message once, so the gateway keeps a copy here before acknowledging it, to replay after a fix. Only the gateway can read it.';
COMMENT ON COLUMN ingest_dead.error_code IS 'A short code such as an SQLSTATE or mapper_panic, never message text';
COMMENT ON COLUMN ingest_dead.info IS 'Message metadata: ids, timestamp, flags';
COMMENT ON COLUMN ingest_dead.raw IS 'The decrypted message as protobuf';

-- migrate:down
DROP TABLE ingest_dead;
ALTER TABLE messages DROP COLUMN expires_at, DROP COLUMN view_once;
ALTER TABLE chats DROP COLUMN name_set_at;

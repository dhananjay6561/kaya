-- migrate:up
CREATE TABLE history_jobs (
  id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  wa_id text NOT NULL UNIQUE,
  notification bytea,
  sync_type text NOT NULL,
  chunk_order int,
  progress int,
  state text NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'stored', 'done', 'dead')),
  attempts int NOT NULL DEFAULT 0,
  error_code text,
  chats int NOT NULL DEFAULT 0,
  messages int NOT NULL DEFAULT 0,
  created_at timestamptz NOT NULL DEFAULT now(),
  finished_at timestamptz,
  CHECK ((state = 'pending') = (notification IS NOT NULL))
);

COMMENT ON TABLE history_jobs IS 'One row per history sync chunk WhatsApp announced. The gateway stores the announcement in the same transaction that acknowledges it, then downloads and stores the chunk, and only then tells WhatsApp it has it. pending: not stored yet. stored: rows are in, the receipt is still to send. done: finished. dead: given up, see error_code. The row is kept so a repeated announcement is not processed twice.';
COMMENT ON COLUMN history_jobs.wa_id IS 'Id of the message that carried the announcement';
COMMENT ON COLUMN history_jobs.notification IS 'The announcement as protobuf. It holds the key to the whole chunk, so it is cleared as soon as the chunk is stored or given up on.';
COMMENT ON COLUMN history_jobs.chats IS 'Chat rows this job inserted';
COMMENT ON COLUMN history_jobs.messages IS 'Message rows this job inserted';

CREATE INDEX history_jobs_open_idx ON history_jobs (id) WHERE state IN ('pending', 'stored');

CREATE TABLE pending_changes (
  id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  record jsonb NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);

COMMENT ON TABLE pending_changes IS 'Live edits and revokes whose message is not stored yet, usually because it is still on its way in a history chunk. They are applied after each history job and dropped after 7 days. Without this a message deleted for everyone during the first sync would later be stored from history with its text.';

GRANT SELECT, INSERT, UPDATE, DELETE ON history_jobs, pending_changes TO kaya_gateway;

-- migrate:down
DROP TABLE pending_changes;
DROP TABLE history_jobs;

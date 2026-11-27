-- migrate:up
CREATE TABLE alerts (
  id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  message_id bigint UNIQUE,
  task_id bigint UNIQUE,
  chat_id bigint NOT NULL,
  urgent boolean NOT NULL DEFAULT false,
  note text NOT NULL CHECK (btrim(note) <> '' AND char_length(note) <= 400),
  created_at timestamptz NOT NULL DEFAULT now(),
  batch text,
  sent_at timestamptz,
  skipped text,
  CHECK ((message_id IS NULL) <> (task_id IS NULL)),
  CHECK (sent_at IS NULL OR (skipped IS NULL AND batch IS NOT NULL))
);

COMMENT ON TABLE alerts IS 'Heads-ups Kaya sends DJ in his own chat without being asked: a message that needs him, found by tagging, or a reminder coming up';
COMMENT ON COLUMN alerts.message_id IS 'messages.id the alert is about. Not a foreign key: the chat tables belong to the gateway.';
COMMENT ON COLUMN alerts.task_id IS 'tasks.id of a reminder coming up';
COMMENT ON COLUMN alerts.urgent IS 'Sent at once; other alerts wait to be sent together';
COMMENT ON COLUMN alerts.note IS 'One line for DJ saying who, what and why it matters, with private details put back';
COMMENT ON COLUMN alerts.batch IS 'The idempotency key of the message the alert goes out in, set before sending, so a send that was cut off is repeated under the same key';
COMMENT ON COLUMN alerts.skipped IS 'Why it was never sent (a short code): DJ had already answered in that chat, the message was deleted, or it was too old';

CREATE INDEX alerts_waiting_idx ON alerts (created_at) WHERE sent_at IS NULL AND skipped IS NULL;
CREATE INDEX alerts_sent_idx ON alerts (sent_at) WHERE sent_at IS NOT NULL;

CREATE TABLE alerting (
  id smallint PRIMARY KEY DEFAULT 1 CHECK (id = 1),
  enabled boolean NOT NULL DEFAULT true,
  changed_at timestamptz
);

COMMENT ON TABLE alerting IS 'Whether Kaya sends alerts, switched with /alerts on and /alerts off';

INSERT INTO alerting DEFAULT VALUES;

GRANT SELECT, INSERT, UPDATE ON alerts TO kaya_core;
GRANT SELECT, UPDATE ON alerting TO kaya_core;

-- migrate:down
DROP TABLE alerting;
DROP TABLE alerts;

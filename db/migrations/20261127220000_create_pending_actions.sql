-- migrate:up
CREATE TABLE pending_actions (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  kind text NOT NULL CHECK (kind ~ '^[a-z][a-z0-9_]{0,63}$'),
  target_jid text NOT NULL CHECK (target_jid ~ '^[0-9]+(-[0-9]+)?@(s\.whatsapp\.net|lid|g\.us)$'),
  summary text NOT NULL CHECK (btrim(summary) <> '' AND char_length(summary) <= 1000),
  payload jsonb NOT NULL,
  state text NOT NULL DEFAULT 'draft'
    CHECK (state IN ('draft', 'pending', 'confirmed', 'declined', 'expired', 'superseded', 'done', 'failed')),
  created_at timestamptz NOT NULL DEFAULT now(),
  expires_at timestamptz NOT NULL,
  shown_at timestamptz,
  shown_wa_id text,
  decided_at timestamptz,
  decided_by bigint,
  result text,
  CHECK (expires_at > created_at AND expires_at <= created_at + interval '1 hour'),
  CHECK ((decided_by IS NULL) = (state IN ('draft', 'pending', 'expired', 'superseded'))),
  CHECK ((decided_at IS NULL) = (decided_by IS NULL)),
  CHECK ((shown_at IS NULL) = (shown_wa_id IS NULL)),
  CHECK (state IN ('draft', 'expired') OR shown_at IS NOT NULL),
  CHECK (decided_at IS NULL OR shown_at IS NOT NULL)
);

COMMENT ON TABLE pending_actions IS 'Things Kaya would do outside DJ''s own chat, waiting for his yes: nothing outward happens without a row here he confirmed';
COMMENT ON COLUMN pending_actions.target_jid IS 'The chat the action writes to; the gateway sends a confirmed message only to this chat';
COMMENT ON COLUMN pending_actions.summary IS 'Exactly what DJ was asked to approve, as Kaya showed it to him';
COMMENT ON COLUMN pending_actions.payload IS 'What the action needs to run, by kind; for send_message the text to send';
COMMENT ON COLUMN pending_actions.state IS 'draft until the answer asking DJ reached his chat, then pending until he answers; only a shown action can be decided';
COMMENT ON COLUMN pending_actions.shown_at IS 'When the message asking DJ was sent, by WhatsApp''s clock; his yes must come after it';
COMMENT ON COLUMN pending_actions.shown_wa_id IS 'The WhatsApp id of the message that asked DJ';
COMMENT ON COLUMN pending_actions.decided_by IS 'messages.id of DJ''s yes or no in his own chat. Not a foreign key: the chat tables belong to the gateway.';
COMMENT ON COLUMN pending_actions.result IS 'What happened when it ran, or why it did not';

CREATE INDEX pending_actions_open_idx ON pending_actions (created_at) WHERE state = 'pending';
CREATE UNIQUE INDEX pending_actions_decided_by_idx ON pending_actions (decided_by) WHERE decided_by IS NOT NULL;

GRANT SELECT, INSERT, UPDATE ON pending_actions TO kaya_core;
GRANT SELECT ON pending_actions TO kaya_gateway;

-- migrate:down
DROP TABLE pending_actions;

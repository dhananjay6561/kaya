-- migrate:up
CREATE INDEX messages_sent_at_idx ON messages (sent_at);

COMMENT ON INDEX messages_sent_at_idx IS 'For core''s summaries, which read every message sent since a moment across chats. Core adds it although the gateway owns the table.';

-- migrate:down
DROP INDEX messages_sent_at_idx;

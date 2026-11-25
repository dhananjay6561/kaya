-- migrate:up
CREATE INDEX messages_body_search_idx ON messages USING gin (to_tsvector('simple', coalesce(body, '')));

COMMENT ON INDEX messages_body_search_idx IS 'Keyword search over message text for core''s search. The simple configuration neither stems nor drops stop words, so Hinglish and names match as typed; queries must use the same expression.';

-- migrate:down
DROP INDEX messages_body_search_idx;

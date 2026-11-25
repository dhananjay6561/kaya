-- migrate:up
CREATE EXTENSION IF NOT EXISTS vector;

CREATE TABLE chunks (
  id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  chat_id bigint NOT NULL,
  first_sent_at timestamptz NOT NULL,
  last_sent_at timestamptz NOT NULL,
  message_count int NOT NULL CHECK (message_count > 0),
  text_hash text NOT NULL,
  model text NOT NULL,
  embedding halfvec(1024),
  skipped text,
  created_at timestamptz NOT NULL DEFAULT now(),
  CHECK (first_sent_at <= last_sent_at),
  CHECK ((embedding IS NULL) = (skipped IS NOT NULL))
);

COMMENT ON TABLE chunks IS 'Search index over chat text: consecutive text messages of one chat sent close together, embedded as one vector. The text stays in messages; a chunk keeps a hash of the text it was made from, so a change is noticed and the chunk made again.';
COMMENT ON COLUMN chunks.chat_id IS 'chats.id. Not a foreign key: the chat tables belong to the gateway.';
COMMENT ON COLUMN chunks.text_hash IS 'sha256 of the chunk text before masking';
COMMENT ON COLUMN chunks.model IS 'The embedding model that made the vector; vectors from different models cannot be compared, so a chunk from another model is made again';
COMMENT ON COLUMN chunks.skipped IS 'Why the embedding service would not take this chunk (a short code); such a chunk has no vector and is tried again only when its text changes';

CREATE INDEX chunks_embedding_idx ON chunks USING hnsw (embedding halfvec_cosine_ops);
CREATE INDEX chunks_chat_idx ON chunks (chat_id, first_sent_at);

CREATE TABLE message_chunks (
  message_id bigint PRIMARY KEY,
  chunk_id bigint NOT NULL REFERENCES chunks (id) ON DELETE CASCADE,
  message_updated_at timestamptz NOT NULL
);

COMMENT ON TABLE message_chunks IS 'Which chunk each indexed message belongs to; a text message with no row here is not searchable yet';
COMMENT ON COLUMN message_chunks.message_id IS 'messages.id. Not a foreign key: the chat tables belong to the gateway, and a message deleted there frees its chunk on the next check.';
COMMENT ON COLUMN message_chunks.message_updated_at IS 'messages.updated_at as it was when the chunk text was read, so any later change is noticed even if it landed while the chunk was being embedded';

CREATE INDEX message_chunks_chunk_idx ON message_chunks (chunk_id);

GRANT SELECT, INSERT, UPDATE, DELETE ON chunks, message_chunks TO kaya_core;

-- migrate:down
DROP TABLE message_chunks;
DROP TABLE chunks;

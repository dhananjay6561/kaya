-- migrate:up
CREATE TABLE media (
  message_id bigint PRIMARY KEY REFERENCES messages (id) ON DELETE CASCADE,
  mime_type text,
  size_bytes bigint CHECK (size_bytes >= 0),
  file_name text,
  duration_s int CHECK (duration_s >= 0),
  width int CHECK (width >= 0),
  height int CHECK (height >= 0),
  sha256 text CHECK (sha256 ~ '^[0-9a-f]{64}$'),
  state text NOT NULL DEFAULT 'remote' CHECK (state IN ('remote', 'stored', 'expired', 'failed')),
  error_code text,
  stored_at timestamptz,
  CHECK ((state = 'stored') = (stored_at IS NOT NULL)),
  CHECK (state <> 'stored' OR sha256 IS NOT NULL)
);

COMMENT ON TABLE media IS 'What is known about the file of an image, video, audio, voice or document message. remote: still with WhatsApp. stored: downloaded, at media/<first two characters of sha256>/<sha256> under the gateway''s data folder. expired: WhatsApp no longer has it. failed: a download failed, see error_code; it can be asked for again. View-once messages and stickers have no row.';
COMMENT ON COLUMN media.sha256 IS 'SHA-256 of the file itself, in hex. It names the stored file; it cannot be used to fetch it.';
COMMENT ON COLUMN media.file_name IS 'Name the sender gave a document. Never used as a path.';

CREATE INDEX media_stored_sha256_idx ON media (sha256) WHERE state = 'stored';

CREATE TABLE media_keys (
  message_id bigint PRIMARY KEY REFERENCES media (message_id) ON DELETE CASCADE,
  media_key bytea NOT NULL,
  direct_path text NOT NULL,
  file_enc_sha256 bytea NOT NULL,
  media_type text NOT NULL CHECK (media_type IN ('image', 'video', 'audio', 'document'))
);

COMMENT ON TABLE media_keys IS 'What it takes to fetch and decrypt a file from WhatsApp''s servers. Together these work like a password for that file, with no further login, so only the gateway can read this table.';

CREATE TABLE media_requests (
  message_id bigint PRIMARY KEY REFERENCES media (message_id) ON DELETE CASCADE,
  requested_at timestamptz NOT NULL DEFAULT now(),
  attempts int NOT NULL DEFAULT 0
);

COMMENT ON TABLE media_requests IS 'The gateway''s download queue: a row asks for that message''s file. The gateway downloads only what is asked for here, and removes the row when media.state has the outcome.';

CREATE FUNCTION notify_media_requests() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  PERFORM pg_notify('media_requests', '');
  RETURN NULL;
END
$$;

CREATE TRIGGER media_requests_notify AFTER INSERT ON media_requests
FOR EACH STATEMENT EXECUTE FUNCTION notify_media_requests();

GRANT SELECT, INSERT, UPDATE, DELETE ON media, media_keys, media_requests TO kaya_gateway;
GRANT SELECT ON media TO kaya_core;

-- migrate:down
DROP TABLE media_requests;
DROP FUNCTION notify_media_requests();
DROP TABLE media_keys;
DROP TABLE media;

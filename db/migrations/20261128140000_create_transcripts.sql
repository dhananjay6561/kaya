-- migrate:up
CREATE TABLE transcripts (
  message_id bigint PRIMARY KEY,
  state text NOT NULL CHECK (state IN ('done', 'failed')),
  text text NOT NULL DEFAULT '' CHECK (char_length(text) <= 20000),
  language text,
  duration_s numeric(8, 2) CHECK (duration_s >= 0),
  model text,
  error text,
  created_at timestamptz NOT NULL DEFAULT now(),
  CHECK ((state = 'failed') = (error IS NOT NULL))
);

COMMENT ON TABLE transcripts IS 'What was said in a voice note, written by the speech model, so a voice note DJ sends Kaya is a command and anyone''s voice note can be searched, tagged and summed up';
COMMENT ON COLUMN transcripts.message_id IS 'messages.id of the voice note. Not a foreign key: the chat tables belong to the gateway.';
COMMENT ON COLUMN transcripts.error IS 'Why it could not be transcribed (a short code), such as the file never arriving';

CREATE TABLE transcribing (
  id smallint PRIMARY KEY DEFAULT 1 CHECK (id = 1),
  started_after bigint NOT NULL
);

COMMENT ON TABLE transcribing IS 'Where transcribing started: only voice notes stored after it are transcribed, never the history';

INSERT INTO transcribing (started_after) SELECT coalesce(max(id), 0) FROM messages;

GRANT SELECT, INSERT, UPDATE ON transcripts TO kaya_core;
GRANT SELECT ON transcribing TO kaya_core;
GRANT INSERT (message_id) ON media_requests TO kaya_core;

-- migrate:down
REVOKE INSERT (message_id) ON media_requests FROM kaya_core;
DROP TABLE transcribing;
DROP TABLE transcripts;

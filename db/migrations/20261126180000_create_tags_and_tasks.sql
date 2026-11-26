-- migrate:up
CREATE TABLE message_tags (
  message_id bigint PRIMARY KEY,
  tags text[] NOT NULL DEFAULT '{}' CHECK (tags <@ ARRAY['important', 'ignore', 'task', 'reminder']),
  skipped text,
  created_at timestamptz NOT NULL DEFAULT now(),
  CHECK (skipped IS NULL OR tags = '{}')
);

COMMENT ON TABLE message_tags IS 'One row for every new message Kaya has read for tags, tagged or not, so each is read once. A message with no row is still waiting.';
COMMENT ON COLUMN message_tags.message_id IS 'messages.id. Not a foreign key: the chat tables belong to the gateway.';
COMMENT ON COLUMN message_tags.tags IS 'important: needs DJ''s attention; ignore: noise he never needs; task: something he has to do; reminder: something at a set time. Empty for an ordinary message.';
COMMENT ON COLUMN message_tags.skipped IS 'Why the message was passed over without tags (a short code), such as the model refusing it';

CREATE INDEX message_tags_tags_idx ON message_tags USING gin (tags);

CREATE TABLE tasks (
  id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  message_id bigint NOT NULL,
  chat_id bigint NOT NULL,
  kind text NOT NULL CHECK (kind IN ('task', 'reminder')),
  title text NOT NULL CHECK (btrim(title) <> '' AND char_length(title) <= 300),
  due_at timestamptz,
  due_has_time boolean NOT NULL DEFAULT false,
  state text NOT NULL DEFAULT 'open' CHECK (state IN ('open', 'done')),
  created_at timestamptz NOT NULL DEFAULT now(),
  done_at timestamptz,
  UNIQUE (message_id, kind),
  CHECK ((state = 'done') = (done_at IS NOT NULL)),
  CHECK (NOT due_has_time OR due_at IS NOT NULL)
);

COMMENT ON TABLE tasks IS 'Things DJ has to do or be reminded of, found in his chats by tagging; /tasks lists the open ones and /done closes one';
COMMENT ON COLUMN tasks.message_id IS 'messages.id the task was found in. Not a foreign key: the chat tables belong to the gateway.';
COMMENT ON COLUMN tasks.chat_id IS 'chats.id of that message, so the task can say where it came from';
COMMENT ON COLUMN tasks.title IS 'One short line saying what to do, written by the model with private details put back';
COMMENT ON COLUMN tasks.due_at IS 'When it is due, if the message says; midnight in DJ''s time zone when only the day is known';
COMMENT ON COLUMN tasks.due_has_time IS 'Whether due_at has a time of day, or only the day';

CREATE INDEX tasks_open_idx ON tasks (state, due_at);

CREATE TABLE tagging (
  id smallint PRIMARY KEY DEFAULT 1 CHECK (id = 1),
  started_after bigint NOT NULL,
  usage_day date,
  usage_tokens int NOT NULL DEFAULT 0 CHECK (usage_tokens >= 0)
);

COMMENT ON TABLE tagging IS 'Tagging''s own state: which messages it reads, and the model tokens it has spent today against its daily allowance';
COMMENT ON COLUMN tagging.started_after IS 'messages.id of the last message stored before tagging began; only later messages are tagged, never the history';
COMMENT ON COLUMN tagging.usage_day IS 'The day in DJ''s time zone that usage_tokens counts';

INSERT INTO tagging (started_after) SELECT coalesce(max(id), 0) FROM messages;

GRANT SELECT, INSERT, UPDATE, DELETE ON message_tags, tasks TO kaya_core;
GRANT SELECT, UPDATE ON tagging TO kaya_core;

-- migrate:down
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM tasks) THEN
    RAISE EXCEPTION 'tasks holds DJ''s tasks; copy or empty it before rolling back';
  END IF;
END
$$;
DROP TABLE tagging;
DROP TABLE tasks;
DROP TABLE message_tags;

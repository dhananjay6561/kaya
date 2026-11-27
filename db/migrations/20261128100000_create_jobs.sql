-- migrate:up
CREATE TABLE jobs (
  id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  text text NOT NULL CHECK (btrim(text) <> '' AND char_length(text) <= 500),
  ask boolean NOT NULL DEFAULT false,
  repeat text NOT NULL DEFAULT 'none' CHECK (repeat IN ('none', 'hourly', 'daily', 'weekdays', 'weekly', 'monthly')),
  every int NOT NULL DEFAULT 1 CHECK (every BETWEEN 1 AND 365),
  weekdays smallint[] NOT NULL DEFAULT '{}' CHECK (weekdays <@ ARRAY[0, 1, 2, 3, 4, 5, 6]::smallint[]),
  local_time text NOT NULL CHECK (local_time ~ '^([01][0-9]|2[0-3]):[0-5][0-9]$'),
  until date,
  next_at timestamptz,
  state text NOT NULL DEFAULT 'active' CHECK (state IN ('active', 'done', 'cancelled')),
  created_at timestamptz NOT NULL DEFAULT now(),
  created_by bigint,
  CHECK ((state = 'active') = (next_at IS NOT NULL)),
  CHECK (repeat = 'weekly' OR weekdays = '{}')
);

COMMENT ON TABLE jobs IS 'Reminders and scheduled jobs DJ asked for: each fires at next_at in his own chat, and a repeating one then moves on to its next time';
COMMENT ON COLUMN jobs.text IS 'What to remind him of, or with ask, what Kaya answers each time as if he had just asked it';
COMMENT ON COLUMN jobs.ask IS 'false: send the text as a reminder, no model call; true: answer it with the chat model when it fires';
COMMENT ON COLUMN jobs.every IS 'Every how many hours, days, weeks or months it repeats';
COMMENT ON COLUMN jobs.weekdays IS 'For weekly: the days it fires, 0 = Sunday';
COMMENT ON COLUMN jobs.local_time IS 'The time of day in DJ''s time zone it fires at, HH:MM; for hourly, the minute it starts from';
COMMENT ON COLUMN jobs.next_at IS 'When it fires next; NULL once done or cancelled';
COMMENT ON COLUMN jobs.created_by IS 'messages.id of DJ''s message that asked for it. Not a foreign key: the chat tables belong to the gateway.';

CREATE INDEX jobs_next_idx ON jobs (next_at) WHERE state = 'active';

CREATE TABLE job_runs (
  job_id bigint NOT NULL REFERENCES jobs (id),
  due_at timestamptz NOT NULL,
  sent_at timestamptz,
  late boolean NOT NULL DEFAULT false,
  text text,
  PRIMARY KEY (job_id, due_at)
);

COMMENT ON TABLE job_runs IS 'Each time a job fired: one row per job and due time, so a time is never fired twice, and recent ones join the conversation';
COMMENT ON COLUMN job_runs.text IS 'The message Kaya sent';

CREATE FUNCTION jobs_notify() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  PERFORM pg_notify('jobs', '');
  RETURN NULL;
END
$$;

CREATE TRIGGER jobs_notify AFTER INSERT OR UPDATE ON jobs FOR EACH STATEMENT EXECUTE FUNCTION jobs_notify();

GRANT SELECT, INSERT, UPDATE ON jobs, job_runs TO kaya_core;

-- migrate:down
DROP TABLE job_runs;
DROP TABLE jobs;
DROP FUNCTION jobs_notify();

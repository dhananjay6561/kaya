-- migrate:up
CREATE TABLE facts (
  id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  body text NOT NULL CHECK (btrim(body) <> '' AND char_length(body) <= 500),
  source text NOT NULL CHECK (source IN ('told', 'learned')),
  message_id bigint,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now()
);

COMMENT ON TABLE facts IS 'Lasting facts about DJ that go into every context package: told with /remember, or learned from his exchanges with Kaya. Kept for good, until he removes one with /forget or a later exchange changes it.';
COMMENT ON COLUMN facts.body IS 'The fact as one short line, as DJ wrote it or as the routine model wrote it from his words';
COMMENT ON COLUMN facts.source IS 'told: DJ wrote it with /remember; learned: taken from what he said to Kaya';
COMMENT ON COLUMN facts.message_id IS 'messages.id of the /remember command or the question it was learned from. Not a foreign key: the chat tables belong to the gateway.';

CREATE UNIQUE INDEX facts_body_idx ON facts (lower(body));

CREATE TABLE fact_learning (
  id smallint PRIMARY KEY DEFAULT 1 CHECK (id = 1),
  exchange_id bigint NOT NULL DEFAULT 0,
  updated_at timestamptz NOT NULL DEFAULT now()
);

COMMENT ON TABLE fact_learning IS 'How far fact learning has read the exchanges: one row, moved in the same transaction as the facts an exchange gave, so no exchange is learned from twice';
COMMENT ON COLUMN fact_learning.exchange_id IS 'exchanges.id of the last exchange learned from; zero before the first';

INSERT INTO fact_learning DEFAULT VALUES;

GRANT SELECT, INSERT, UPDATE, DELETE ON facts TO kaya_core;
GRANT SELECT, UPDATE ON fact_learning TO kaya_core;

-- migrate:down
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM facts) THEN
    RAISE EXCEPTION 'facts holds what Kaya knows about DJ; copy or empty it before rolling back';
  END IF;
END
$$;
DROP TABLE fact_learning;
DROP TABLE facts;

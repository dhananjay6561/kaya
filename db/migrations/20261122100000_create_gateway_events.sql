-- migrate:up
CREATE TABLE gateway_events (
  seq bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  id uuid NOT NULL UNIQUE,
  type text NOT NULL CHECK (type IN ('connection.state', 'message.stored', 'history.synced', 'send.result')),
  payload jsonb NOT NULL,
  state text NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'handled', 'dead')),
  error text,
  created_at timestamptz NOT NULL DEFAULT now(),
  finished_at timestamptz,
  CHECK (payload ->> 'id' = id::text AND payload ->> 'type' = type),
  CHECK ((state = 'pending') = (finished_at IS NULL)),
  CHECK ((state = 'dead') = (error IS NOT NULL))
);

COMMENT ON TABLE gateway_events IS 'Outbox from gateway to core. The gateway inserts each event in the same transaction as the rows it describes; core is the only consumer, drains pending rows in seq order and marks each handled in the same transaction as its effects. A row that cannot be handled becomes dead with an error code. Handled rows are deleted after 7 days.';
COMMENT ON COLUMN gateway_events.payload IS 'The whole event, valid against contracts/schemas/v1/event.schema.json';
COMMENT ON COLUMN gateway_events.error IS 'Why a dead row was given up on: a short code, never payload values';

CREATE INDEX gateway_events_pending_idx ON gateway_events (seq) WHERE state = 'pending';
CREATE INDEX gateway_events_handled_idx ON gateway_events (finished_at) WHERE state = 'handled';

CREATE FUNCTION notify_gateway_events() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  PERFORM pg_notify('gateway_events', '');
  RETURN NULL;
END
$$;

COMMENT ON FUNCTION notify_gateway_events() IS 'Wakes core once per transaction that inserted events (identical notifications in one transaction collapse into one). It is only a wake-up: core drains by seq, so a missed notification delays delivery but loses nothing.';

CREATE TRIGGER gateway_events_notify AFTER INSERT ON gateway_events
FOR EACH STATEMENT EXECUTE FUNCTION notify_gateway_events();

-- migrate:down
DROP TABLE gateway_events;
DROP FUNCTION notify_gateway_events();

-- migrate:up
CREATE TABLE send_requests (
  seq bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  id uuid NOT NULL UNIQUE,
  payload jsonb NOT NULL,
  expires_at timestamptz NOT NULL,
  state text NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'sending', 'sent', 'failed')),
  wa_id text,
  error text,
  created_at timestamptz NOT NULL DEFAULT now(),
  finished_at timestamptz,
  CHECK (payload ->> 'id' IS NOT DISTINCT FROM id::text),
  CHECK (payload ->> 'expires_at' ~ '^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?Z$'),
  CHECK ((payload ->> 'expires_at')::timestamptz IS NOT DISTINCT FROM expires_at),
  CHECK ((state IN ('sent', 'failed')) = (finished_at IS NOT NULL)),
  CHECK ((state = 'failed') = (error IS NOT NULL)),
  CHECK (state NOT IN ('sending', 'sent') OR wa_id IS NOT NULL)
);

COMMENT ON TABLE send_requests IS 'Outbox from core to gateway: messages core asks the gateway to send. Core inserts a row; the gateway is the only consumer and takes pending rows in seq order. Before the network call it stores the WhatsApp message id and moves the row to sending, so a row found in sending after a restart is never sent again: it fails with outcome_unknown. Every outcome is also reported to core as a send.result event. Finished rows are deleted after 7 days.';
COMMENT ON COLUMN send_requests.payload IS 'The whole request, valid against contracts/schemas/v1/send-request.schema.json';
COMMENT ON COLUMN send_requests.expires_at IS 'Copy of the payload''s expires_at, so a request can be expired without reading a payload that may not be valid';
COMMENT ON COLUMN send_requests.wa_id IS 'WhatsApp id of the message, chosen by the gateway before sending. It tells a message Kaya sent from one DJ typed.';
COMMENT ON COLUMN send_requests.error IS 'Why a failed request failed: a short code, never payload values';

CREATE INDEX send_requests_pending_idx ON send_requests (seq) WHERE state = 'pending';
CREATE INDEX send_requests_finished_idx ON send_requests (finished_at) WHERE finished_at IS NOT NULL;
CREATE UNIQUE INDEX send_requests_wa_id_idx ON send_requests (wa_id) WHERE wa_id IS NOT NULL;

CREATE FUNCTION notify_send_requests() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  PERFORM pg_notify('send_requests', '');
  RETURN NULL;
END
$$;

CREATE TRIGGER send_requests_notify AFTER INSERT ON send_requests
FOR EACH STATEMENT EXECUTE FUNCTION notify_send_requests();

CREATE FUNCTION guard_send_request() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF NOT ((OLD.state = 'pending' AND NEW.state IN ('sending', 'failed'))
       OR (OLD.state = 'sending' AND NEW.state IN ('sent', 'failed'))) THEN
    RAISE EXCEPTION 'a send request cannot go from % to %', OLD.state, NEW.state USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END
$$;

COMMENT ON FUNCTION guard_send_request() IS 'Allows only pending to sending or failed, and sending to sent or failed. A finished request can never be changed, so an outcome core has been told cannot be rewritten.';

CREATE TRIGGER send_requests_guard BEFORE UPDATE ON send_requests
FOR EACH ROW EXECUTE FUNCTION guard_send_request();

CREATE TABLE connection_state (
  only_row boolean PRIMARY KEY DEFAULT true CHECK (only_row),
  state text NOT NULL CHECK (state IN ('connecting', 'pairing', 'connected', 'disconnected', 'logged_out')),
  reason text,
  own_jid text,
  occurred_at timestamptz NOT NULL,
  updated_at timestamptz NOT NULL DEFAULT now()
);

COMMENT ON TABLE connection_state IS 'The latest connection.state event core has handled: one row. It is what the gateway last reported, not proof the gateway is running now.';
COMMENT ON COLUMN connection_state.own_jid IS 'DJ''s own JID as the gateway reported it; where self-chat sends are addressed';

GRANT INSERT (id, payload, expires_at) ON send_requests TO kaya_core;
GRANT SELECT ON send_requests TO kaya_core;
GRANT SELECT, DELETE ON send_requests TO kaya_gateway;
GRANT UPDATE (state, wa_id, error, finished_at) ON send_requests TO kaya_gateway;
GRANT SELECT, INSERT, UPDATE ON connection_state TO kaya_core;

-- migrate:down
DROP TABLE connection_state;
DROP TABLE send_requests;
DROP FUNCTION guard_send_request();
DROP FUNCTION notify_send_requests();

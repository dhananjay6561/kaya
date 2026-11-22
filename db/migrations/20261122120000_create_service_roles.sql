-- migrate:up
DO $$
BEGIN
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'kaya_gateway') THEN
    CREATE ROLE kaya_gateway NOLOGIN;
  END IF;
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'kaya_core') THEN
    CREATE ROLE kaya_core NOLOGIN;
  END IF;
END
$$;

CREATE SCHEMA whatsmeow AUTHORIZATION kaya_gateway;
COMMENT ON SCHEMA whatsmeow IS 'whatsmeow''s own tables: the WhatsApp session, encryption keys and its event buffer. Owned by the gateway role; no other role can see inside.';

DO $$
DECLARE
  t text;
BEGIN
  FOR t IN SELECT tablename FROM pg_tables WHERE schemaname = 'public' AND tablename LIKE 'whatsmeow\_%' LOOP
    EXECUTE format('ALTER TABLE public.%I SET SCHEMA whatsmeow', t);
    EXECUTE format('ALTER TABLE whatsmeow.%I OWNER TO kaya_gateway', t);
  END LOOP;
END
$$;

REVOKE ALL ON SCHEMA whatsmeow FROM PUBLIC;

REVOKE EXECUTE ON FUNCTION contact_id_for(text, text, text) FROM PUBLIC;
REVOKE EXECUTE ON FUNCTION merge_contacts(bigint, bigint) FROM PUBLIC;

GRANT SELECT, INSERT, UPDATE, DELETE ON contacts, chats, messages, reactions, ingest_dead TO kaya_gateway;
GRANT INSERT ON gateway_events TO kaya_gateway;
GRANT EXECUTE ON FUNCTION contact_id_for(text, text, text) TO kaya_gateway;
GRANT EXECUTE ON FUNCTION merge_contacts(bigint, bigint) TO kaya_gateway;

GRANT SELECT ON contacts, chats, messages, reactions TO kaya_core;
GRANT SELECT, UPDATE, DELETE ON gateway_events TO kaya_core;

-- migrate:down
REVOKE ALL ON gateway_events FROM kaya_core;
REVOKE ALL ON contacts, chats, messages, reactions FROM kaya_core;
REVOKE ALL ON FUNCTION merge_contacts(bigint, bigint) FROM kaya_gateway;
REVOKE ALL ON FUNCTION contact_id_for(text, text, text) FROM kaya_gateway;
REVOKE ALL ON gateway_events FROM kaya_gateway;
REVOKE ALL ON contacts, chats, messages, reactions, ingest_dead FROM kaya_gateway;
GRANT EXECUTE ON FUNCTION merge_contacts(bigint, bigint) TO PUBLIC;
GRANT EXECUTE ON FUNCTION contact_id_for(text, text, text) TO PUBLIC;

DO $$
DECLARE
  t text;
BEGIN
  FOR t IN SELECT tablename FROM pg_tables WHERE schemaname = 'whatsmeow' LOOP
    EXECUTE format('ALTER TABLE whatsmeow.%I OWNER TO CURRENT_USER', t);
    EXECUTE format('ALTER TABLE whatsmeow.%I SET SCHEMA public', t);
  END LOOP;
END
$$;
DROP SCHEMA whatsmeow;

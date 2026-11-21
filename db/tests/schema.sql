\set ON_ERROR_STOP on
BEGIN;

CREATE FUNCTION pg_temp.expect_error(statement text, expected_state text) RETURNS void
LANGUAGE plpgsql AS $$
BEGIN
  EXECUTE statement;
  RAISE EXCEPTION 'expected SQLSTATE % from: %', expected_state, statement;
EXCEPTION
  WHEN OTHERS THEN
    IF SQLSTATE <> expected_state THEN
      RAISE EXCEPTION 'expected SQLSTATE %, got % (%) from: %', expected_state, SQLSTATE, SQLERRM, statement;
    END IF;
END
$$;

INSERT INTO chats (jid, kind) VALUES ('911111111111@s.whatsapp.net', 'direct');
INSERT INTO chats (jid, kind, name) VALUES ('120363000000000000@g.us', 'group', 'Test group');
INSERT INTO contacts (jid, lid, saved_name) VALUES ('911111111111@s.whatsapp.net', '100000000000001@lid', 'Test Person');

INSERT INTO messages (chat_jid, sender_jid, wa_id, from_me, sent_at, kind, body, source)
VALUES ('911111111111@s.whatsapp.net', '911111111111@s.whatsapp.net', 'TESTID1', false, now(), 'text', 'hi', 'live');

DO $$
DECLARE
  n int;
BEGIN
  INSERT INTO messages (chat_jid, sender_jid, wa_id, from_me, sent_at, kind, body, source)
  VALUES ('911111111111@s.whatsapp.net', '911111111111@s.whatsapp.net', 'TESTID1', false, now(), 'text', 'hi', 'history')
  ON CONFLICT (chat_jid, sender_jid, wa_id) DO NOTHING;
  SELECT count(*) INTO n FROM messages;
  ASSERT n = 1, 'a repeated message must not create a second row';

  INSERT INTO messages (chat_jid, sender_jid, wa_id, from_me, sent_at, kind, source)
  VALUES ('120363000000000000@g.us', '911111111111@s.whatsapp.net', 'TESTID1', false, now(), 'image', 'history');
  SELECT count(*) INTO n FROM messages;
  ASSERT n = 2, 'the same message id in another chat is a different message';
END
$$;

DO $$
BEGIN
  PERFORM pg_temp.expect_error($q$INSERT INTO messages (chat_jid, sender_jid, wa_id, from_me, sent_at, kind, source)
    VALUES ('999@s.whatsapp.net', '999@s.whatsapp.net', 'X', false, now(), 'text', 'live')$q$, '23503');
  PERFORM pg_temp.expect_error($q$INSERT INTO messages (chat_jid, sender_jid, wa_id, from_me, sent_at, kind, source)
    VALUES ('911111111111@s.whatsapp.net', '911111111111@s.whatsapp.net', 'X', false, now(), 'gif', 'live')$q$, '23514');
  PERFORM pg_temp.expect_error($q$INSERT INTO messages (chat_jid, sender_jid, wa_id, from_me, sent_at, kind, source)
    VALUES ('911111111111@s.whatsapp.net', '911111111111@s.whatsapp.net', 'X', false, now(), 'text', 'import')$q$, '23514');
  PERFORM pg_temp.expect_error($q$INSERT INTO messages (chat_jid, sender_jid, wa_id, from_me, sent_at, kind, source)
    VALUES ('911111111111@s.whatsapp.net', '911111111111@s.whatsapp.net', '', false, now(), 'text', 'live')$q$, '23514');
  PERFORM pg_temp.expect_error($q$INSERT INTO messages (chat_jid, sender_jid, wa_id, from_me, kind, source)
    VALUES ('911111111111@s.whatsapp.net', '911111111111@s.whatsapp.net', 'X', false, 'text', 'live')$q$, '23502');
  PERFORM pg_temp.expect_error($q$INSERT INTO chats (jid, kind) VALUES ('not-a-jid', 'direct')$q$, '23514');
  PERFORM pg_temp.expect_error($q$INSERT INTO chats (jid, kind) VALUES ('1@s.whatsapp.net', 'channel')$q$, '23514');
  PERFORM pg_temp.expect_error($q$INSERT INTO contacts (jid, lid) VALUES ('922222222222@s.whatsapp.net', '100000000000001@lid')$q$, '23505');
  PERFORM pg_temp.expect_error($q$INSERT INTO contacts (jid, lid) VALUES ('922222222222@s.whatsapp.net', '922222222222@s.whatsapp.net')$q$, '23514');
END
$$;

DO $$
DECLARE
  stamped timestamptz;
  n int;
BEGIN
  UPDATE chats SET name = 'Renamed', updated_at = '2000-01-01' WHERE kind = 'group';
  SELECT updated_at INTO stamped FROM chats WHERE kind = 'group';
  ASSERT stamped = now(), 'updates must set updated_at to now';

  DELETE FROM chats WHERE kind = 'group';
  SELECT count(*) INTO n FROM messages WHERE chat_jid = '120363000000000000@g.us';
  ASSERT n = 0, 'deleting a chat must delete its messages';
END
$$;

ROLLBACK;
\echo schema tests passed

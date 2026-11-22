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

DO $$
DECLARE
  dj bigint := contact_id_for(NULL, '919000000000@s.whatsapp.net');
  by_pn bigint := contact_id_for(NULL, '911111111111@s.whatsapp.net');
  by_lid bigint := contact_id_for('100000000000001@lid', NULL);
  pn_chat bigint;
  lid_chat bigint;
  merged bigint;
  n int;
BEGIN
  ASSERT contact_id_for(NULL, '911111111111@s.whatsapp.net') = by_pn, 'the same phone number is the same contact';
  ASSERT by_pn <> by_lid, 'before the mapping is known, the LID and the phone number are separate rows';

  INSERT INTO chats (kind, contact_id) VALUES ('direct', by_pn) RETURNING id INTO pn_chat;
  INSERT INTO chats (kind, contact_id) VALUES ('direct', by_lid) RETURNING id INTO lid_chat;

  INSERT INTO messages (chat_id, wa_id, sender_id, from_me, sent_at, kind, body, source)
  VALUES
    (pn_chat, 'TESTMSG1', by_pn, false, now(), 'text', 'hi', 'live'),
    (lid_chat, 'TESTMSG1', by_lid, false, now(), 'text', 'hi', 'history'),
    (pn_chat, 'TESTMSG2', dj, true, now(), 'text', 'hello', 'live'),
    (lid_chat, 'TESTMSG2', dj, true, now(), 'text', 'hello', 'history');
  INSERT INTO messages (chat_id, wa_id, sender_id, from_me, sent_at, kind, body, source, reply_to_wa_id, reply_to_sender_id)
  VALUES (lid_chat, 'TESTMSG3', dj, true, now(), 'text', 'replying', 'history', 'TESTMSG1', by_lid);
  INSERT INTO reactions (chat_id, target_wa_id, target_sender_id, sender_id, emoji, reacted_at)
  VALUES
    (pn_chat, 'TESTMSG2', dj, by_pn, '👍', now() - interval '1 hour'),
    (lid_chat, 'TESTMSG2', dj, by_lid, '❤️', now());

  merged := contact_id_for('100000000000001@lid', '911111111111@s.whatsapp.net');
  ASSERT merged = by_pn, 'merging keeps the older row';

  SELECT count(*) INTO n FROM contacts WHERE id IN (by_pn, by_lid);
  ASSERT n = 1, 'the LID row is folded into the phone-number row';
  PERFORM 1 FROM contacts
  WHERE id = merged AND lid = '100000000000001@lid' AND pn = '911111111111@s.whatsapp.net';
  ASSERT FOUND, 'the merged contact has both addresses';

  SELECT count(*) INTO n FROM chats WHERE contact_id = merged;
  ASSERT n = 1, 'the two spellings of one DM are one chat';
  SELECT count(*) INTO n FROM messages WHERE wa_id IN ('TESTMSG1', 'TESTMSG2', 'TESTMSG3');
  ASSERT n = 3, 'one message delivered under both spellings is stored once';
  SELECT count(*) INTO n FROM messages WHERE wa_id = 'TESTMSG3' AND reply_to_sender_id = merged;
  ASSERT n = 1, 'replies point at the merged contact';
  SELECT count(*) INTO n FROM reactions WHERE target_wa_id = 'TESTMSG2';
  ASSERT n = 1, 'one sender has one reaction per message after the merge';
  PERFORM 1 FROM reactions WHERE target_wa_id = 'TESTMSG2' AND emoji = '❤️';
  ASSERT FOUND, 'the newest reaction wins';

  ASSERT contact_id_for('100000000000001@lid', NULL) = merged, 'the LID now finds the merged contact';
END
$$;

DO $$
DECLARE
  old_owner bigint := contact_id_for('100000000000003@lid', '913333333333@s.whatsapp.net');
  other bigint := contact_id_for('100000000000004@lid', NULL);
  new_owner bigint;
  pn_only bigint;
  n int;
BEGIN
  new_owner := contact_id_for('100000000000009@lid', '913333333333@s.whatsapp.net');
  ASSERT new_owner NOT IN (old_owner, other), 'a recycled number goes to the account that has it now';
  PERFORM 1 FROM contacts WHERE id = old_owner AND lid = '100000000000003@lid' AND pn IS NULL;
  ASSERT FOUND, 'the previous owner keeps its LID and loses the number';
  ASSERT contact_id_for(NULL, '913333333333@s.whatsapp.net') = new_owner, 'the number now finds the new owner';

  ASSERT contact_id_for('100000000000003@lid', '915555555555@s.whatsapp.net') = old_owner,
    'an account that changed its number stays the same contact';
  PERFORM 1 FROM contacts WHERE id = old_owner AND pn = '915555555555@s.whatsapp.net';
  ASSERT FOUND, 'the new number is recorded';

  ASSERT contact_id_for('100000000000004@lid', '915555555555@s.whatsapp.net') = other,
    'a number that moved to another account follows it';
  PERFORM 1 FROM contacts WHERE id = old_owner AND pn IS NULL;
  ASSERT FOUND, 'the account it moved from loses it';

  pn_only := contact_id_for(NULL, '916666666666@s.whatsapp.net');
  ASSERT contact_id_for('100000000000004@lid', '916666666666@s.whatsapp.net') = LEAST(other, pn_only),
    'a phone-only row for an account''s new number is folded into the account';
  SELECT count(*) INTO n FROM contacts WHERE id IN (other, pn_only);
  ASSERT n = 1, 'the two rows became one';
  PERFORM 1 FROM contacts
  WHERE lid = '100000000000004@lid' AND pn = '916666666666@s.whatsapp.net';
  ASSERT FOUND, 'the account has its current number';

  PERFORM pg_temp.expect_error(format('SELECT merge_contacts(%s, %s)', old_owner, new_owner), '23505');
  PERFORM contact_id_for('100000000000010@lid', NULL, '13135550009@bot');
  PERFORM pg_temp.expect_error(
    $q$SELECT contact_id_for('100000000000011@lid', NULL, '13135550009@bot')$q$, '23505');
  PERFORM pg_temp.expect_error($q$SELECT contact_id_for(NULL, NULL)$q$, '22023');
END
$$;

DO $$
DECLARE
  by_pn bigint := contact_id_for(NULL, '917777777777@s.whatsapp.net');
  by_lid bigint := contact_id_for('100000000000007@lid', NULL);
  pn_chat bigint;
  lid_chat bigint;
  merged bigint;
  n int;
BEGIN
  INSERT INTO chats (kind, contact_id) VALUES ('direct', by_pn) RETURNING id INTO pn_chat;
  INSERT INTO chats (kind, contact_id) VALUES ('direct', by_lid) RETURNING id INTO lid_chat;
  INSERT INTO messages (chat_id, wa_id, sender_id, from_me, sent_at, kind, body, source)
  VALUES
    (pn_chat, 'TESTREVOKE', by_pn, false, now(), 'text', 'secret', 'history'),
    (lid_chat, 'TESTREVOKE', by_lid, false, now(), 'text', 'secret', 'live'),
    (pn_chat, 'TESTEDIT', by_pn, false, now(), 'text', 'first', 'history'),
    (lid_chat, 'TESTEDIT', by_lid, false, now(), 'text', 'first', 'live'),
    (pn_chat, 'TESTBOTH', by_pn, false, now(), 'text', 'same', 'history'),
    (lid_chat, 'TESTBOTH', by_pn, false, now(), 'text', 'same', 'live'),
    (lid_chat, 'TESTBOTH', by_lid, false, now(), 'text', 'same', 'live'),
    (pn_chat, 'TESTREVOKEKEPT', by_pn, false, now(), 'text', 'secret', 'history'),
    (lid_chat, 'TESTREVOKEKEPT', by_lid, false, now(), 'text', 'secret', 'live'),
    (pn_chat, 'TESTEDITKEPT', by_pn, false, now(), 'text', 'first', 'history'),
    (lid_chat, 'TESTEDITKEPT', by_lid, false, now(), 'text', 'first', 'live');
  INSERT INTO messages (chat_id, wa_id, sender_id, from_me, sent_at, kind, body, source, reply_to_wa_id, reply_to_sender_id)
  VALUES (lid_chat, 'TESTREPLY', by_lid, false, now(), 'text', 'yes', 'live', 'TESTEDIT', by_lid);
  INSERT INTO messages (chat_id, wa_id, sender_id, from_me, sent_at, kind, body, source)
  VALUES (pn_chat, 'TESTREPLY', by_pn, false, now(), 'text', 'yes', 'history');
  UPDATE messages SET body = NULL, deleted_at = now() WHERE wa_id = 'TESTREVOKE' AND sender_id = by_lid;
  UPDATE messages SET body = 'second', edited_at = now() WHERE wa_id = 'TESTEDIT' AND sender_id = by_lid;
  UPDATE messages SET body = NULL, deleted_at = now() WHERE wa_id = 'TESTREVOKEKEPT' AND sender_id = by_pn;
  UPDATE messages SET body = 'second', edited_at = now() WHERE wa_id = 'TESTEDITKEPT' AND sender_id = by_pn;

  merged := contact_id_for('100000000000007@lid', '917777777777@s.whatsapp.net');

  SELECT count(*) INTO n FROM messages
  WHERE wa_id IN ('TESTREVOKE', 'TESTEDIT', 'TESTBOTH', 'TESTREVOKEKEPT', 'TESTEDITKEPT');
  ASSERT n = 5, 'every spelling of a message becomes one row, even when one chat held several';
  PERFORM 1 FROM messages WHERE wa_id = 'TESTREVOKE' AND deleted_at IS NOT NULL AND body IS NULL;
  ASSERT FOUND, 'a revoke on either copy survives the merge';
  PERFORM 1 FROM messages WHERE wa_id = 'TESTEDIT' AND edited_at IS NOT NULL AND body = 'second';
  ASSERT FOUND, 'the latest edit on either copy survives the merge';
  PERFORM 1 FROM messages WHERE wa_id = 'TESTREVOKEKEPT' AND deleted_at IS NOT NULL AND body IS NULL;
  ASSERT FOUND, 'a revoke on the copy that is kept survives the merge';
  PERFORM 1 FROM messages WHERE wa_id = 'TESTEDITKEPT' AND edited_at IS NOT NULL AND body = 'second';
  ASSERT FOUND, 'an edit on the copy that is kept survives the merge';
  PERFORM 1 FROM messages
  WHERE wa_id = 'TESTREPLY' AND reply_to_wa_id = 'TESTEDIT' AND reply_to_sender_id = merged;
  ASSERT FOUND, 'a reply link on either copy survives the merge';
  SELECT count(*) INTO n FROM messages
  WHERE wa_id IN ('TESTREVOKE', 'TESTEDIT', 'TESTBOTH') AND (sender_id <> merged OR chat_id NOT IN (pn_chat, lid_chat));
  ASSERT n = 0, 'survivors point at the merged contact';
END
$$;

DO $$
DECLARE
  dj bigint := contact_id_for(NULL, '919000000000@s.whatsapp.net');
  bot bigint := contact_id_for(NULL, NULL, '13135550002@bot');
  hosted bigint := contact_id_for('200000000000001@hosted.lid', '914444444444@hosted');
  grp bigint;
  n int;
BEGIN
  ASSERT bot IS NOT NULL AND hosted IS NOT NULL, 'bots and hosted accounts are contacts too';
  INSERT INTO chats (kind, jid, name) VALUES ('group', '120363000000000000@g.us', 'Test group') RETURNING id INTO grp;
  INSERT INTO chats (kind, jid) VALUES ('group', '911111111111-1600000000@g.us');
  INSERT INTO chats (kind, jid) VALUES ('broadcast', 'status@broadcast');
  INSERT INTO chats (kind, jid) VALUES ('newsletter', '120363111111111111@newsletter');

  INSERT INTO messages (chat_id, wa_id, sender_id, from_me, sent_at, kind, source)
  VALUES (grp, 'TESTGRP1', dj, true, now(), 'image', 'live');
  INSERT INTO reactions (chat_id, target_wa_id, target_sender_id, sender_id, emoji, reacted_at)
  VALUES (grp, 'TESTGRP1', dj, bot, '😂', now());

  UPDATE chats SET name = 'Renamed', updated_at = '2000-01-01' WHERE id = grp;
  PERFORM 1 FROM chats WHERE id = grp AND updated_at = now();
  ASSERT FOUND, 'updates set updated_at to now';

  DELETE FROM chats WHERE id = grp;
  SELECT count(*) INTO n FROM messages WHERE wa_id = 'TESTGRP1';
  ASSERT n = 0, 'deleting a chat deletes its messages';
  SELECT count(*) INTO n FROM reactions WHERE target_wa_id = 'TESTGRP1';
  ASSERT n = 0, 'deleting a chat deletes its reactions';
END
$$;

DO $$
DECLARE
  dj bigint := contact_id_for(NULL, '919000000000@s.whatsapp.net');
  chat bigint;
BEGIN
  INSERT INTO chats (kind, jid) VALUES ('group', '120363222222222222@g.us') RETURNING id INTO chat;

  PERFORM pg_temp.expect_error($q$INSERT INTO contacts (pn) VALUES ('911111111111:12@s.whatsapp.net')$q$, '23514');
  PERFORM pg_temp.expect_error($q$INSERT INTO contacts (lid) VALUES ('100000000000001:3@lid')$q$, '23514');
  PERFORM pg_temp.expect_error($q$INSERT INTO contacts (pn) VALUES ('100000000000001@lid')$q$, '23514');
  PERFORM pg_temp.expect_error($q$INSERT INTO contacts (lid) VALUES ('a@b')$q$, '23514');
  PERFORM pg_temp.expect_error($q$INSERT INTO contacts (pn) VALUES (' 91 1@s.whatsapp.net')$q$, '23514');
  PERFORM pg_temp.expect_error($q$INSERT INTO contacts (saved_name) VALUES ('nobody')$q$, '23514');

  PERFORM pg_temp.expect_error($q$INSERT INTO chats (kind) VALUES ('direct')$q$, '23514');
  PERFORM pg_temp.expect_error($q$INSERT INTO chats (kind, jid) VALUES ('group', '911111111111@s.whatsapp.net')$q$, '23514');
  PERFORM pg_temp.expect_error($q$INSERT INTO chats (kind, jid) VALUES ('group', 'a@b')$q$, '23514');
  PERFORM pg_temp.expect_error($q$INSERT INTO chats (kind, jid) VALUES ('channel', '1@newsletter')$q$, '23514');
  PERFORM pg_temp.expect_error(
    format($q$INSERT INTO chats (kind, contact_id, jid) VALUES ('direct', %s, '1@g.us')$q$, dj), '23514');

  PERFORM pg_temp.expect_error(format($q$INSERT INTO messages (chat_id, wa_id, sender_id, from_me, sent_at, kind, source)
    VALUES (-1, 'X', %s, false, now(), 'text', 'live')$q$, dj), '23503');
  PERFORM pg_temp.expect_error(format($q$INSERT INTO messages (chat_id, wa_id, sender_id, from_me, sent_at, kind, source)
    VALUES (%s, 'X', %s, false, now(), 'gif', 'live')$q$, chat, dj), '23514');
  PERFORM pg_temp.expect_error(format($q$INSERT INTO messages (chat_id, wa_id, sender_id, from_me, sent_at, kind, source)
    VALUES (%s, 'X', %s, false, now(), 'text', 'import')$q$, chat, dj), '23514');
  PERFORM pg_temp.expect_error(format($q$INSERT INTO messages (chat_id, wa_id, sender_id, from_me, sent_at, kind, source)
    VALUES (%s, '', %s, false, now(), 'text', 'live')$q$, chat, dj), '23514');
  PERFORM pg_temp.expect_error(format($q$INSERT INTO messages (chat_id, wa_id, sender_id, from_me, sent_at, kind, source)
    VALUES (%s, 'has space', %s, false, now(), 'text', 'live')$q$, chat, dj), '23514');
  PERFORM pg_temp.expect_error(format($q$INSERT INTO messages (chat_id, wa_id, sender_id, from_me, kind, source)
    VALUES (%s, 'X', %s, false, 'text', 'live')$q$, chat, dj), '23502');
  PERFORM pg_temp.expect_error(format($q$INSERT INTO messages (chat_id, wa_id, sender_id, from_me, sent_at, kind, source, reply_to_wa_id)
    VALUES (%s, 'X', %s, false, now(), 'text', 'live', 'Y')$q$, chat, dj), '23514');
  PERFORM pg_temp.expect_error(format($q$INSERT INTO reactions (chat_id, target_wa_id, target_sender_id, sender_id, emoji, reacted_at)
    VALUES (%s, 'X', %s, %s, '', now())$q$, chat, dj, dj), '23514');
END
$$;

DO $$
DECLARE
  ok_id uuid := 'a3bb189e-8bf9-4888-9912-ace4e6543002';
  n int;
BEGIN
  INSERT INTO gateway_events (id, type, payload)
  VALUES (ok_id, 'connection.state', jsonb_build_object('id', ok_id, 'type', 'connection.state'));
  PERFORM 1 FROM gateway_events WHERE id = ok_id AND state = 'pending' AND finished_at IS NULL;
  ASSERT FOUND, 'a new event is pending';

  UPDATE gateway_events SET state = 'handled', finished_at = now() WHERE id = ok_id;
  PERFORM pg_temp.expect_error(format($q$UPDATE gateway_events SET state = 'dead', finished_at = now() WHERE id = %L$q$, ok_id), '23514');

  PERFORM pg_temp.expect_error($q$INSERT INTO gateway_events (id, type, payload) VALUES
    ('a3bb189e-8bf9-4888-9912-ace4e6543003', 'message.stored', '{"id": "a3bb189e-8bf9-4888-9912-ace4e6543004", "type": "message.stored"}')$q$, '23514');
  PERFORM pg_temp.expect_error($q$INSERT INTO gateway_events (id, type, payload) VALUES
    ('a3bb189e-8bf9-4888-9912-ace4e6543005', 'message.stored', '{"id": "a3bb189e-8bf9-4888-9912-ace4e6543005", "type": "history.synced"}')$q$, '23514');
  PERFORM pg_temp.expect_error($q$INSERT INTO gateway_events (id, type, payload) VALUES
    ('a3bb189e-8bf9-4888-9912-ace4e6543006', 'message.read', '{"id": "a3bb189e-8bf9-4888-9912-ace4e6543006", "type": "message.read"}')$q$, '23514');
  PERFORM pg_temp.expect_error(format($q$INSERT INTO gateway_events (id, type, payload) VALUES (%L, 'connection.state', %L)$q$,
    ok_id, jsonb_build_object('id', ok_id, 'type', 'connection.state')), '23505');

  SELECT count(*) INTO n FROM gateway_events;
  ASSERT n = 1, 'only the valid event was stored';
END
$$;

ROLLBACK;
\echo schema tests passed

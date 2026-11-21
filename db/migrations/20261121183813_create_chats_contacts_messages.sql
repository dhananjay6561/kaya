-- migrate:up
CREATE FUNCTION set_updated_at() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  NEW.updated_at = now();
  RETURN NEW;
END
$$;

CREATE TABLE contacts (
  jid text PRIMARY KEY CHECK (jid LIKE '_%@_%'),
  lid text UNIQUE CHECK (lid LIKE '_%@lid'),
  saved_name text,
  first_name text,
  push_name text,
  business_name text,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now()
);

COMMENT ON COLUMN contacts.jid IS 'Phone-number JID (number@s.whatsapp.net), or the LID when no number is known';
COMMENT ON COLUMN contacts.saved_name IS 'Name in DJ''s address book';
COMMENT ON COLUMN contacts.push_name IS 'Name the contact set for themselves';

CREATE TRIGGER contacts_set_updated_at BEFORE UPDATE ON contacts
FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE chats (
  jid text PRIMARY KEY CHECK (jid LIKE '_%@_%'),
  kind text NOT NULL CHECK (kind IN ('direct', 'group', 'broadcast', 'newsletter')),
  name text,
  last_message_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now()
);

COMMENT ON COLUMN chats.name IS 'Group subject; empty for direct chats, whose name comes from contacts';

CREATE TRIGGER chats_set_updated_at BEFORE UPDATE ON chats
FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE messages (
  id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  chat_jid text NOT NULL REFERENCES chats (jid) ON DELETE CASCADE,
  sender_jid text NOT NULL CHECK (sender_jid LIKE '_%@_%'),
  wa_id text NOT NULL CHECK (wa_id <> ''),
  from_me boolean NOT NULL,
  sent_at timestamptz NOT NULL,
  kind text NOT NULL CHECK (
    kind IN (
      'text', 'image', 'video', 'audio', 'voice', 'document', 'sticker', 'location', 'contact',
      'poll', 'other'
    )
  ),
  body text,
  reply_to_wa_id text,
  source text NOT NULL CHECK (source IN ('live', 'history')),
  edited_at timestamptz,
  deleted_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (chat_jid, sender_jid, wa_id)
);

COMMENT ON COLUMN messages.wa_id IS 'WhatsApp message id; unique together with chat and sender';
COMMENT ON COLUMN messages.body IS 'Text, or the caption of a media message';
COMMENT ON COLUMN messages.source IS 'live: received while connected; history: from the history sync';

CREATE INDEX messages_chat_sent_at_idx ON messages (chat_jid, sent_at DESC);
CREATE INDEX messages_sender_sent_at_idx ON messages (sender_jid, sent_at DESC);
CREATE INDEX messages_sent_at_idx ON messages (sent_at DESC);

CREATE TRIGGER messages_set_updated_at BEFORE UPDATE ON messages
FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- migrate:down
DROP TABLE messages;
DROP TABLE chats;
DROP TABLE contacts;
DROP FUNCTION set_updated_at();

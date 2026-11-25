-- migrate:up
CREATE EXTENSION IF NOT EXISTS pg_trgm;

DROP INDEX contacts_names_search_idx;
DROP INDEX chats_name_search_idx;

CREATE INDEX contacts_name_trgm_idx ON contacts USING gin ((lower(CASE WHEN coalesce(saved_name, '') = '' AND coalesce(first_name, '') = ''
  THEN coalesce(push_name, '') || ' ' || coalesce(business_name, '')
  ELSE coalesce(saved_name, '') || ' ' || coalesce(first_name, '') END)) gin_trgm_ops);
CREATE INDEX chats_name_trgm_idx ON chats USING gin ((lower(coalesce(name, ''))) gin_trgm_ops);

COMMENT ON INDEX contacts_name_trgm_idx IS 'Lets core find the contacts a question names in any script and despite typos, by the names DJ saved or, for a contact he has not saved, the names they chose; queries must use the same expression.';
COMMENT ON INDEX chats_name_trgm_idx IS 'Lets core find the groups a question names in any script and despite typos; queries must use the same expression.';

-- migrate:down
DROP INDEX chats_name_trgm_idx;
DROP INDEX contacts_name_trgm_idx;
CREATE INDEX contacts_names_search_idx ON contacts
  USING gin (to_tsvector('simple', CASE WHEN coalesce(saved_name, '') = '' AND coalesce(first_name, '') = '' THEN coalesce(push_name, '') || ' ' || coalesce(business_name, '') ELSE coalesce(saved_name, '') || ' ' || coalesce(first_name, '') END));
CREATE INDEX chats_name_search_idx ON chats USING gin (to_tsvector('simple', coalesce(name, '')));

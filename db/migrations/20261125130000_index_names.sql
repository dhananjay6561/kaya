-- migrate:up
CREATE INDEX contacts_names_search_idx ON contacts
  USING gin (to_tsvector('simple', coalesce(saved_name, '') || ' ' || coalesce(first_name, '') || ' ' || coalesce(push_name, '') || ' ' || coalesce(business_name, '')));
CREATE INDEX chats_name_search_idx ON chats USING gin (to_tsvector('simple', coalesce(name, '')));

COMMENT ON INDEX contacts_names_search_idx IS 'Lets core find the contacts a question names; queries must use the same expression.';
COMMENT ON INDEX chats_name_search_idx IS 'Lets core find the groups a question names; queries must use the same expression.';

-- migrate:down
DROP INDEX chats_name_search_idx;
DROP INDEX contacts_names_search_idx;

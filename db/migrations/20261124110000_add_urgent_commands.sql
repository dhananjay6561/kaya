-- migrate:up
ALTER TABLE commands ADD COLUMN urgent boolean NOT NULL DEFAULT false;
COMMENT ON COLUMN commands.urgent IS 'A command that controls Kaya itself (/pause, /status, ...): it runs before waiting ordinary commands and is never dropped by the per-minute cap. Set when the command is recorded.';

DROP INDEX commands_pending_idx;
CREATE INDEX commands_pending_idx ON commands (urgent DESC, id) WHERE state = 'pending';

-- migrate:down
DROP INDEX commands_pending_idx;
CREATE INDEX commands_pending_idx ON commands (id) WHERE state = 'pending';
ALTER TABLE commands DROP COLUMN urgent;

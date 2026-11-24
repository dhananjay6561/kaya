-- migrate:up
CREATE TABLE pause_state (
  only_row boolean PRIMARY KEY DEFAULT true CHECK (only_row),
  paused_at timestamptz,
  changed_by text,
  changed_at timestamptz NOT NULL DEFAULT now()
);

COMMENT ON TABLE pause_state IS 'Whether DJ has paused Kaya with /pause: one row. While paused, Kaya answers only its built-in slash commands and does nothing else on its own.';
COMMENT ON COLUMN pause_state.paused_at IS 'When Kaya was paused; null while it is running';
COMMENT ON COLUMN pause_state.changed_by IS 'wa_id of the command that last paused or resumed Kaya, so a retried command gives the same answer';

INSERT INTO pause_state DEFAULT VALUES;

GRANT SELECT, UPDATE ON pause_state TO kaya_core;

-- migrate:down
DROP TABLE pause_state;

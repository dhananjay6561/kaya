-- migrate:up
CREATE INDEX gateway_events_dead_idx ON gateway_events (seq) WHERE state = 'dead';

-- migrate:down
DROP INDEX gateway_events_dead_idx;

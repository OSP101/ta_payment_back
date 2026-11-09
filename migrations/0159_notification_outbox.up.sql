CREATE TABLE notification_outbox (
    id uuid PRIMARY KEY,
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    payload_enc bytea,
    state text NOT NULL DEFAULT 'pending' CHECK (state IN ('pending','processing','sent','failed')),
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts BETWEEN 0 AND 5),
    available_at timestamptz NOT NULL DEFAULT now(),
    lease_until timestamptz,
    lease_token uuid,
    created_at timestamptz NOT NULL DEFAULT now(),
    finished_at timestamptz
);
CREATE INDEX notification_outbox_due_idx ON notification_outbox(available_at,created_at)
    WHERE state IN ('pending','processing');
CREATE INDEX notification_outbox_user_idx ON notification_outbox(user_id);
CREATE INDEX notification_outbox_state_idx ON notification_outbox(state) WHERE state <> 'sent';
CREATE INDEX notification_outbox_retention_idx ON notification_outbox(finished_at) WHERE state IN ('sent','failed');

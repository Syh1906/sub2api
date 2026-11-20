-- A text request can outlive its HTTP handler while its bill waits in the worker pool.
CREATE TABLE text_usage_admissions (
    id uuid PRIMARY KEY,
    user_id bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    expires_at timestamptz NOT NULL
);
CREATE INDEX text_usage_admissions_buyer ON text_usage_admissions (user_id, expires_at);

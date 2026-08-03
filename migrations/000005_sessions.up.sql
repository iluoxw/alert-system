-- sessions: self-rolled login sessions for the management API (R8.1/R8.3).
-- No third-party session store: the plaintext token is only ever handed to the
-- client via Set-Cookie; the DB keeps token_hash = hex(sha256(token)). This is
-- independent from the managed ingest hook credential. expires_at is the fixed
-- absolute expiry set at login and never
-- extended; last_seen_at is the sliding-renewal watermark, bumped on every
-- authenticated request (session valid only while both are in the future).
CREATE TABLE sessions (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id       UUID NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    token_hash    TEXT NOT NULL,
    expires_at    TIMESTAMPTZ NOT NULL,
    last_seen_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Session lookup on every authenticated request keys on token_hash.
CREATE UNIQUE INDEX sessions_token_hash_uidx ON sessions (token_hash);
CREATE INDEX sessions_user_id_idx ON sessions (user_id);

-- Supports a future cleanup job: WHERE expires_at <= now().
CREATE INDEX sessions_expires_at_idx ON sessions (expires_at);

CREATE TRIGGER sessions_set_updated_at
    BEFORE UPDATE ON sessions
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

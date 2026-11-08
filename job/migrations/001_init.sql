-- Trest job schema version 1. Apply explicitly through your deployment migration process.
-- This schema owns only trest_job_* tables. Application tables remain application-owned.
BEGIN;
CREATE TABLE IF NOT EXISTS trest_job_schema (
    version integer PRIMARY KEY,
    installed_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE TABLE IF NOT EXISTS trest_job_queue (
    id text PRIMARY KEY,
    envelope_version integer NOT NULL CHECK (envelope_version = 1),
    operation_id text NOT NULL,
    payload_version text NOT NULL,
    input json NOT NULL,
    input_hash text NOT NULL,
    principal_id text NOT NULL,
    tenant_id text NOT NULL,
    caller_scope text NOT NULL,
    idempotency_key text,
    correlation_id text NOT NULL,
    trace_link json NOT NULL DEFAULT '{}'::json,
    state text NOT NULL CHECK (state IN ('pending','running','succeeded','failed','cancelled','uncertain')),
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    max_attempts integer NOT NULL CHECK (max_attempts > 0),
    retry_safe boolean NOT NULL DEFAULT false,
    available_at timestamptz NOT NULL,
    deadline_at timestamptz,
    lease_expires_at timestamptz,
    attempt_token text,
    cancel_requested boolean NOT NULL DEFAULT false,
    output json,
    error_code text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    finished_at timestamptz,
    dedupe_until timestamptz NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS trest_job_dedupe ON trest_job_queue
    (tenant_id, operation_id, caller_scope, idempotency_key) WHERE idempotency_key IS NOT NULL;
CREATE INDEX IF NOT EXISTS trest_job_ready ON trest_job_queue (available_at, id) WHERE state = 'pending';
CREATE INDEX IF NOT EXISTS trest_job_expired ON trest_job_queue (lease_expires_at) WHERE state = 'running';
CREATE INDEX IF NOT EXISTS trest_job_retention ON trest_job_queue (finished_at, dedupe_until) WHERE finished_at IS NOT NULL;
CREATE TABLE IF NOT EXISTS trest_job_event (
    sequence bigserial PRIMARY KEY,
    job_id text NOT NULL REFERENCES trest_job_queue(id) ON DELETE CASCADE,
    attempt integer NOT NULL,
    kind text NOT NULL,
    actor text NOT NULL DEFAULT '',
    detail text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX IF NOT EXISTS trest_job_event_history ON trest_job_event(job_id, sequence);
CREATE TABLE IF NOT EXISTS trest_job_outbox (
    id text PRIMARY KEY,
    envelope json NOT NULL,
    state text NOT NULL DEFAULT 'pending' CHECK (state IN ('pending','dispatched','failed')),
    job_id text,
    error_code text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    dispatched_at timestamptz
);
CREATE INDEX IF NOT EXISTS trest_job_outbox_ready ON trest_job_outbox(created_at, id) WHERE state = 'pending';
CREATE TABLE IF NOT EXISTS trest_job_effect (
    tenant_id text NOT NULL,
    operation_id text NOT NULL,
    caller_scope text NOT NULL,
    idempotency_key text NOT NULL,
    principal_id text NOT NULL,
    input_hash text NOT NULL,
    output json,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    expires_at timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, operation_id, caller_scope, idempotency_key)
);
CREATE INDEX IF NOT EXISTS trest_job_effect_retention ON trest_job_effect(expires_at);
INSERT INTO trest_job_schema(version) VALUES (1) ON CONFLICT DO NOTHING;
COMMIT;

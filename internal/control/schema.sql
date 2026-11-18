CREATE TABLE IF NOT EXISTS builds (
    id text PRIMARY KEY,
    name text NOT NULL,
    kind text NOT NULL,
    status text NOT NULL CHECK (status IN ('queued', 'running', 'succeeded', 'failed', 'canceled')),
    source_name text NOT NULL,
    source_path text NOT NULL,
    artifact_name text NOT NULL DEFAULT '',
    artifact_path text NOT NULL DEFAULT '',
    install_token text NOT NULL UNIQUE,
    spec jsonb NOT NULL DEFAULT '{}'::jsonb,
    logs text NOT NULL DEFAULT '',
    error text NOT NULL DEFAULT '',
    claimed_by text NOT NULL DEFAULT '',
    bundle_id text NOT NULL DEFAULT '',
    version text NOT NULL DEFAULT '',
    minimum_os text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS builds_status_created_idx ON builds(status, created_at);

ALTER TABLE builds ADD COLUMN IF NOT EXISTS cache_key text NOT NULL DEFAULT '';
ALTER TABLE builds ADD COLUMN IF NOT EXISTS source_mode text NOT NULL DEFAULT 'archive';
ALTER TABLE builds ADD COLUMN IF NOT EXISTS metrics jsonb NOT NULL DEFAULT '{}'::jsonb;
ALTER TABLE builds ADD COLUMN IF NOT EXISTS stage text NOT NULL DEFAULT '';
ALTER TABLE builds ADD COLUMN IF NOT EXISTS app_zip_name text NOT NULL DEFAULT '';
ALTER TABLE builds ADD COLUMN IF NOT EXISTS app_zip_path text NOT NULL DEFAULT '';
ALTER TABLE builds ADD COLUMN IF NOT EXISTS build_number text NOT NULL DEFAULT '';
ALTER TABLE builds ADD COLUMN IF NOT EXISTS architectures jsonb NOT NULL DEFAULT '[]'::jsonb;
ALTER TABLE builds ADD COLUMN IF NOT EXISTS signing text NOT NULL DEFAULT '';
ALTER TABLE builds ADD COLUMN IF NOT EXISTS signed boolean NOT NULL DEFAULT false;
ALTER TABLE builds ADD COLUMN IF NOT EXISTS notarized boolean NOT NULL DEFAULT false;
ALTER TABLE builds ADD COLUMN IF NOT EXISTS stapled boolean NOT NULL DEFAULT false;
ALTER TABLE builds ADD COLUMN IF NOT EXISTS sha256 text NOT NULL DEFAULT '';
ALTER TABLE builds ADD COLUMN IF NOT EXISTS live jsonb NOT NULL DEFAULT '{}'::jsonb;

ALTER TABLE builds DROP CONSTRAINT IF EXISTS builds_kind_check;
ALTER TABLE builds ADD CONSTRAINT builds_kind_check CHECK (kind IN ('xcode', 'expo', 'resign', 'simulator', 'macos', 'resolve', 'verify'));

ALTER TABLE builds DROP CONSTRAINT IF EXISTS builds_status_check;
ALTER TABLE builds ADD CONSTRAINT builds_status_check CHECK (status IN ('queued', 'running', 'succeeded', 'failed', 'canceled'));

ALTER TABLE builds DROP CONSTRAINT IF EXISTS builds_source_mode_check;
ALTER TABLE builds ADD CONSTRAINT builds_source_mode_check CHECK (source_mode IN ('archive', 'cas'));

CREATE TABLE IF NOT EXISTS devices (
    id text PRIMARY KEY,
    requested_by text NOT NULL,
    udid text NOT NULL UNIQUE,
    name text NOT NULL,
    platform text NOT NULL DEFAULT 'IOS' CHECK (platform IN ('IOS')),
    status text NOT NULL DEFAULT 'queued' CHECK (status IN ('queued', 'running', 'registered', 'failed')),
    apple_device_id text NOT NULL DEFAULT '',
    error text NOT NULL DEFAULT '',
    claimed_by text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS devices_status_created_idx ON devices(status, created_at);
CREATE INDEX IF NOT EXISTS devices_requested_by_created_idx ON devices(requested_by, created_at DESC);

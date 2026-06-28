package syncdb

import "github.com/msrsiddik/apicorex-sync/internal/plugin"

// InitSyncUp creates the generic sync_records table plus a schema-local version
// sequence. It runs inside each tenant's schema (the install flow sets the
// search_path), so every tenant gets an independent version space.
const InitSyncUp = `
CREATE SEQUENCE IF NOT EXISTS sync_version_seq;

CREATE TABLE IF NOT EXISTS sync_records (
    collection        text        NOT NULL,
    record_id         text        NOT NULL,
    user_id           text        NOT NULL,
    payload           jsonb       NOT NULL DEFAULT '{}'::jsonb,
    deleted           boolean     NOT NULL DEFAULT false,
    client_updated_at timestamptz NOT NULL,
    server_updated_at timestamptz NOT NULL DEFAULT now(),
    version           bigint      NOT NULL,
    last_change_id    text        NOT NULL,
    PRIMARY KEY (collection, record_id, user_id)
);

CREATE INDEX IF NOT EXISTS sync_records_pull_idx
    ON sync_records (user_id, version);
CREATE INDEX IF NOT EXISTS sync_records_coll_ver_idx
    ON sync_records (collection, version);
`

const initSyncDown = `
DROP TABLE IF EXISTS sync_records;
DROP SEQUENCE IF EXISTS sync_version_seq;
`

// DeclareMigrations registers the sync schema migrations on the plugin. They are
// advertised in the manifest and run per tenant by Identity on install.
func DeclareMigrations(p *plugin.Plugin) {
	p.Migration("20260601_001", "init_sync", InitSyncUp, initSyncDown)
}

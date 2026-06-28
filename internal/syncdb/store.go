// Package syncdb is the offline-first sync engine: a generic, versioned,
// per-user record store backed by a single sync_records table per tenant
// schema. It implements last-write-wins push and ordered, cursor-based pull.
package syncdb

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// Store runs sync queries against tenant schemas. Every operation opens a
// transaction and sets search_path to the caller's schema (no pool leak).
//
// Collections are per-user by default (records carry the caller's user_id).
// Collections listed in shared are tenant-wide: stored with user_id="" and
// visible to every user in the tenant.
type Store struct {
	db     *sql.DB
	shared map[string]bool
}

// New builds a Store. sharedCollections are tenant-wide (all users share);
// every other collection is per-user.
func New(db *sql.DB, sharedCollections ...string) *Store {
	shared := make(map[string]bool, len(sharedCollections))
	for _, c := range sharedCollections {
		shared[c] = true
	}
	return &Store{db: db, shared: shared}
}

// ownerFor returns the row owner for a collection: "" for tenant-wide (shared)
// collections, otherwise the caller's userID (per-user).
func (s *Store) ownerFor(collection, userID string) string {
	if s.shared[collection] {
		return ""
	}
	return userID
}

// Change is one client change in a push batch.
type Change struct {
	ChangeID   string          `json:"change_id"`
	Collection string          `json:"collection"`
	RecordID   string          `json:"record_id"`
	Deleted    bool            `json:"deleted"`
	Payload    json.RawMessage `json:"payload"`
	UpdatedAt  time.Time       `json:"updated_at"` // client clock — LWW compare key only
}

// Result is the outcome of applying one change.
type Result struct {
	ChangeID        string    `json:"change_id"`
	RecordID        string    `json:"record_id"`
	Status          string    `json:"status"` // applied | duplicate | stale | rejected
	Version         int64     `json:"version,omitempty"`
	ServerUpdatedAt time.Time `json:"server_updated_at,omitempty"`
	Error           string    `json:"error,omitempty"`
}

// Record is one change returned by pull.
type Record struct {
	Collection      string          `json:"collection"`
	RecordID        string          `json:"record_id"`
	Deleted         bool            `json:"deleted"`
	Payload         json.RawMessage `json:"payload"`
	Version         int64           `json:"version"`
	ServerUpdatedAt time.Time       `json:"server_updated_at"`
}

// Push applies a batch of client changes for one user in one transaction using
// last-write-wins. It returns a per-change result and the max version written.
func (s *Store) Push(ctx context.Context, schema, userID string, changes []Change) ([]Result, int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, 0, err
	}
	defer tx.Rollback() //nolint:errcheck — no-op after commit

	if _, err := tx.ExecContext(ctx, fmt.Sprintf("SET LOCAL search_path TO %q", schema)); err != nil {
		return nil, 0, fmt.Errorf("set search_path: %w", err)
	}

	results := make([]Result, 0, len(changes))
	var cursor int64

	for _, ch := range changes {
		res := Result{ChangeID: ch.ChangeID, RecordID: ch.RecordID}
		if ch.Collection == "" || ch.RecordID == "" || ch.ChangeID == "" {
			res.Status = "rejected"
			res.Error = "change_id, collection and record_id are required"
			results = append(results, res)
			continue
		}

		// owner is "" for tenant-wide collections, else the caller's user_id.
		owner := s.ownerFor(ch.Collection, userID)

		// lock the existing row (if any) so concurrent pushes to the same record
		// serialize and resolve LWW deterministically.
		var (
			exVersion   int64
			exUpdatedAt time.Time
			exChangeID  string
			found       bool
		)
		row := tx.QueryRowContext(ctx,
			`SELECT version, client_updated_at, last_change_id
			   FROM sync_records
			  WHERE collection=$1 AND record_id=$2 AND user_id=$3
			  FOR UPDATE`,
			ch.Collection, ch.RecordID, owner)
		switch err := row.Scan(&exVersion, &exUpdatedAt, &exChangeID); err {
		case nil:
			found = true
		case sql.ErrNoRows:
			found = false
		default:
			return nil, 0, fmt.Errorf("lookup record: %w", err)
		}

		// idempotent retry: same change already applied to this record.
		if found && exChangeID == ch.ChangeID {
			res.Status = "duplicate"
			res.Version = exVersion
			results = append(results, res)
			continue
		}
		// last-write-wins: an existing row with an equal-or-newer client time wins.
		if found && !exUpdatedAt.Before(ch.UpdatedAt) {
			res.Status = "stale"
			res.Version = exVersion
			results = append(results, res)
			continue
		}

		var version int64
		if err := tx.QueryRowContext(ctx, `SELECT nextval('sync_version_seq')`).Scan(&version); err != nil {
			return nil, 0, fmt.Errorf("next version: %w", err)
		}

		payload := ch.Payload
		if len(payload) == 0 {
			payload = json.RawMessage(`{}`)
		}
		var serverUpdatedAt time.Time
		err := tx.QueryRowContext(ctx,
			`INSERT INTO sync_records
			   (collection, record_id, user_id, payload, deleted,
			    client_updated_at, server_updated_at, version, last_change_id)
			 VALUES ($1,$2,$3,$4,$5,$6, now(), $7, $8)
			 ON CONFLICT (collection, record_id, user_id) DO UPDATE SET
			   payload=EXCLUDED.payload, deleted=EXCLUDED.deleted,
			   client_updated_at=EXCLUDED.client_updated_at,
			   server_updated_at=now(), version=EXCLUDED.version,
			   last_change_id=EXCLUDED.last_change_id
			 RETURNING server_updated_at`,
			ch.Collection, ch.RecordID, owner, payload, ch.Deleted,
			ch.UpdatedAt, version, ch.ChangeID,
		).Scan(&serverUpdatedAt)
		if err != nil {
			return nil, 0, fmt.Errorf("upsert record: %w", err)
		}

		res.Status = "applied"
		res.Version = version
		res.ServerUpdatedAt = serverUpdatedAt
		results = append(results, res)
		if version > cursor {
			cursor = version
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, 0, err
	}
	return results, cursor, nil
}

// Pull returns the user's records with version > since, ordered ascending. It
// includes the user's own per-user records AND tenant-wide (user_id="") records.
// If collection is non-empty it filters to that collection. limit caps the page;
// hasMore is true when more records remain past the page.
func (s *Store) Pull(ctx context.Context, schema, userID, collection string, since int64, limit int) ([]Record, int64, bool, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, 0, false, err
	}
	defer tx.Rollback() //nolint:errcheck

	if _, err := tx.ExecContext(ctx, fmt.Sprintf("SET LOCAL search_path TO %q", schema)); err != nil {
		return nil, 0, false, fmt.Errorf("set search_path: %w", err)
	}

	// own records (user_id = caller) + tenant-wide records (user_id = '')
	rows, err := tx.QueryContext(ctx,
		`SELECT collection, record_id, deleted, payload, version, server_updated_at
		   FROM sync_records
		  WHERE (user_id = $1 OR user_id = '')
		    AND version > $2
		    AND ($3 = '' OR collection = $3)
		  ORDER BY version ASC
		  LIMIT $4`,
		userID, since, collection, limit+1)
	if err != nil {
		return nil, 0, false, fmt.Errorf("pull query: %w", err)
	}
	defer rows.Close()

	out := make([]Record, 0, limit)
	for rows.Next() {
		var r Record
		if err := rows.Scan(&r.Collection, &r.RecordID, &r.Deleted, &r.Payload, &r.Version, &r.ServerUpdatedAt); err != nil {
			return nil, 0, false, fmt.Errorf("scan record: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, false, err
	}

	hasMore := len(out) > limit
	if hasMore {
		out = out[:limit]
	}
	cursor := since
	if len(out) > 0 {
		cursor = out[len(out)-1].Version
	}
	return out, cursor, hasMore, nil
}

// GCTombstones permanently deletes tombstone rows (deleted=true) older than the
// retention window, across every tenant schema (tenant_*). A device offline
// longer than the window must do a full re-sync (since=0) to avoid resurrecting
// deleted records. Returns the number of rows removed.
func (s *Store) GCTombstones(ctx context.Context, retention time.Duration) (int64, error) {
	schemas, err := s.tenantSchemas(ctx)
	if err != nil {
		return 0, err
	}
	cutoff := time.Now().Add(-retention)
	var total int64
	for _, schema := range schemas {
		n, err := s.gcSchema(ctx, schema, cutoff)
		if err != nil {
			return total, fmt.Errorf("gc %s: %w", schema, err)
		}
		total += n
	}
	return total, nil
}

func (s *Store) tenantSchemas(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT schema_name FROM information_schema.schemata WHERE schema_name LIKE 'tenant\_%'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

func (s *Store) gcSchema(ctx context.Context, schema string, cutoff time.Time) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("SET LOCAL search_path TO %q", schema)); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx,
		`DELETE FROM sync_records WHERE deleted = true AND server_updated_at < $1`, cutoff)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return n, nil
}

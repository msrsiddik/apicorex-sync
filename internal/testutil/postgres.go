// Package testutil provides a Postgres test container for the sync integration
// tests. It requires a running container engine (Docker or Podman); see
// TESTING.md. Tests skip when no engine is available.
package testutil

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// PG holds a running Postgres container and a connection pool.
type PG struct {
	DSN       string
	DB        *sql.DB
	container testcontainers.Container
}

// NewPostgres starts a Postgres container and returns a connection. Skips the
// test if no container engine is reachable.
func NewPostgres(t *testing.T) *PG {
	t.Helper()
	ctx := context.Background()

	container, err := tcpostgres.Run(ctx,
		"postgres:16-alpine",
		tcpostgres.WithDatabase("sync_test"),
		tcpostgres.WithUsername("test"),
		tcpostgres.WithPassword("test"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		t.Skipf("skipping integration test: no container engine (%v)", err)
	}

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	if h := os.Getenv("TEST_DB_HOST"); h != "" {
		dsn = strings.Replace(dsn, "@localhost:", "@"+h+":", 1)
		dsn = strings.Replace(dsn, "@127.0.0.1:", "@"+h+":", 1)
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}

	pg := &PG{DSN: dsn, DB: db, container: container}
	t.Cleanup(func() {
		db.Close()
		_ = pg.container.Terminate(context.Background())
	})
	return pg
}

// CreateTenantSchema creates a tenant schema and runs the given migration SQL
// inside it (mirrors how Identity installs a plugin's migration per tenant).
func (pg *PG) CreateTenantSchema(t *testing.T, schema, migrationSQL string) {
	t.Helper()
	ctx := context.Background()
	if _, err := pg.DB.ExecContext(ctx, fmt.Sprintf("CREATE SCHEMA %q", schema)); err != nil {
		t.Fatalf("create schema %s: %v", schema, err)
	}
	tx, err := pg.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback() //nolint:errcheck
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("SET LOCAL search_path TO %q", schema)); err != nil {
		t.Fatalf("set search_path: %v", err)
	}
	if _, err := tx.ExecContext(ctx, migrationSQL); err != nil {
		t.Fatalf("run migration: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit migration: %v", err)
	}
}

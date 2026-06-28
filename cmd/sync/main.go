package main

import (
	"context"
	"database/sql"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/joho/godotenv"
	"github.com/msrsiddik/apicorex-sync/internal/plugin"
	"github.com/msrsiddik/apicorex-sync/internal/syncdb"
)

func main() {
	godotenv.Load()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		log.Fatal("DATABASE_URL is required")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer db.Close()

	store := syncdb.New(db, splitCSV(os.Getenv("SYNC_SHARED_COLLECTIONS"))...)

	p := plugin.New(plugin.Config{
		Name:          "sync",
		Version:       "1.0.0",
		CoreURL:       envOr("CORE_URL", "http://localhost:8080"),
		PluginAddr:    envOr("PLUGIN_ADDR", ":50052"),
		PluginBaseURL: envOr("PLUGIN_BASE_URL", ""),
		APIKey:        os.Getenv("PLUGIN_API_KEY"),
	})
	registerRoutes(p, &handlers{store: store})
	syncdb.DeclareMigrations(p)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// background tombstone GC (default 90d retention, daily sweep)
	go runTombstoneGC(ctx, store)

	if err := p.Run(ctx); err != nil {
		log.Fatalf("plugin stopped: %v", err)
	}
}

// runTombstoneGC periodically purges old tombstones across all tenant schemas.
func runTombstoneGC(ctx context.Context, store *syncdb.Store) {
	retention := 90 * 24 * time.Hour
	if v, err := time.ParseDuration(os.Getenv("SYNC_TOMBSTONE_RETENTION")); err == nil && v > 0 {
		retention = v
	}
	t := time.NewTicker(24 * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n, err := store.GCTombstones(ctx, retention); err != nil {
				log.Printf("[sync] tombstone GC error: %v", err)
			} else if n > 0 {
				log.Printf("[sync] tombstone GC removed %d rows", n)
			}
		}
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

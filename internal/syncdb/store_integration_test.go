package syncdb_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/msrsiddik/apicorex-sync/internal/syncdb"
	"github.com/msrsiddik/apicorex-sync/internal/testutil"
)

const schema = "tenant_test"

func newStore(t *testing.T) (*syncdb.Store, *testutil.PG) {
	t.Helper()
	pg := testutil.NewPostgres(t)
	pg.CreateTenantSchema(t, schema, syncdb.InitSyncUp)
	return syncdb.New(pg.DB), pg
}

func change(changeID, coll, recID string, payload string, at time.Time) syncdb.Change {
	return syncdb.Change{
		ChangeID: changeID, Collection: coll, RecordID: recID,
		Payload: json.RawMessage(payload), UpdatedAt: at,
	}
}

func push(t *testing.T, s *syncdb.Store, user string, changes ...syncdb.Change) ([]syncdb.Result, int64) {
	t.Helper()
	res, cursor, err := s.Push(context.Background(), schema, user, changes)
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	return res, cursor
}

func pull(t *testing.T, s *syncdb.Store, user string, since int64, limit int) ([]syncdb.Record, int64, bool) {
	t.Helper()
	recs, cursor, more, err := s.Pull(context.Background(), schema, user, "", since, limit)
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	return recs, cursor, more
}

// 1. round-trip: push 3 → pull from 0 returns all 3 in version order.
func TestRoundTrip(t *testing.T) {
	s, _ := newStore(t)
	now := time.Now()
	push(t, s, "u1",
		change("c1", "notes", "r1", `{"t":"a"}`, now),
		change("c2", "notes", "r2", `{"t":"b"}`, now),
		change("c3", "tasks", "r3", `{"t":"c"}`, now),
	)
	recs, cursor, more := pull(t, s, "u1", 0, 100)
	if len(recs) != 3 {
		t.Fatalf("want 3 records, got %d", len(recs))
	}
	if more {
		t.Error("has_more should be false")
	}
	// versions strictly increasing
	for i := 1; i < len(recs); i++ {
		if recs[i].Version <= recs[i-1].Version {
			t.Errorf("versions not increasing: %d then %d", recs[i-1].Version, recs[i].Version)
		}
	}
	if cursor != recs[len(recs)-1].Version {
		t.Errorf("cursor %d != last version %d", cursor, recs[len(recs)-1].Version)
	}
}

// 2. last-write-wins: older update is stale; newer wins.
func TestLastWriteWins(t *testing.T) {
	s, _ := newStore(t)
	t0 := time.Now()

	push(t, s, "u1", change("c1", "notes", "r1", `{"v":1}`, t0.Add(time.Minute)))

	// older change → stale
	res, _ := push(t, s, "u1", change("c2", "notes", "r1", `{"v":0}`, t0))
	if res[0].Status != "stale" {
		t.Fatalf("older change status = %q, want stale", res[0].Status)
	}

	// newer change → applied
	res, _ = push(t, s, "u1", change("c3", "notes", "r1", `{"v":2}`, t0.Add(2*time.Minute)))
	if res[0].Status != "applied" {
		t.Fatalf("newer change status = %q, want applied", res[0].Status)
	}

	recs, _, _ := pull(t, s, "u1", 0, 100)
	if len(recs) != 1 {
		t.Fatalf("want 1 record (one record_id), got %d", len(recs))
	}
	if string(recs[0].Payload) != `{"v": 2}` && string(recs[0].Payload) != `{"v":2}` {
		t.Errorf("winning payload = %s, want v:2", recs[0].Payload)
	}
}

// 3. delete propagation: a tombstone flows through pull.
func TestDeletePropagation(t *testing.T) {
	s, _ := newStore(t)
	now := time.Now()
	push(t, s, "u1", change("c1", "notes", "r1", `{"t":"a"}`, now))

	// device deletes the record (tombstone)
	del := syncdb.Change{ChangeID: "c2", Collection: "notes", RecordID: "r1", Deleted: true, UpdatedAt: now.Add(time.Minute)}
	push(t, s, "u1", del)

	recs, _, _ := pull(t, s, "u1", 0, 100)
	if len(recs) != 1 || !recs[0].Deleted {
		t.Fatalf("expected one tombstone, got %+v", recs)
	}
}

// 4. idempotent retry: same change_id → duplicate, no new version.
func TestIdempotentRetry(t *testing.T) {
	s, _ := newStore(t)
	now := time.Now()
	res1, _ := push(t, s, "u1", change("c1", "notes", "r1", `{"t":"a"}`, now))
	if res1[0].Status != "applied" {
		t.Fatalf("first push status = %q", res1[0].Status)
	}
	v1 := res1[0].Version

	res2, _ := push(t, s, "u1", change("c1", "notes", "r1", `{"t":"a"}`, now))
	if res2[0].Status != "duplicate" {
		t.Fatalf("retry status = %q, want duplicate", res2[0].Status)
	}
	if res2[0].Version != v1 {
		t.Errorf("duplicate version = %d, want unchanged %d", res2[0].Version, v1)
	}
	recs, _, _ := pull(t, s, "u1", 0, 100)
	if len(recs) != 1 {
		t.Errorf("retry created a second row: %d", len(recs))
	}
}

// 5. cursor ordering with pagination.
func TestCursorPagination(t *testing.T) {
	s, _ := newStore(t)
	now := time.Now()
	for i := 0; i < 5; i++ {
		id := string(rune('a' + i))
		push(t, s, "u1", change("c"+id, "notes", "r"+id, `{}`, now))
	}
	// page 1 (limit 2)
	recs, cursor, more := pull(t, s, "u1", 0, 2)
	if len(recs) != 2 || !more {
		t.Fatalf("page1: got %d recs, more=%v", len(recs), more)
	}
	// page 2
	recs2, cursor2, more2 := pull(t, s, "u1", cursor, 2)
	if len(recs2) != 2 || !more2 {
		t.Fatalf("page2: got %d recs, more=%v", len(recs2), more2)
	}
	// page 3 (last)
	recs3, _, more3 := pull(t, s, "u1", cursor2, 2)
	if len(recs3) != 1 || more3 {
		t.Fatalf("page3: got %d recs, more=%v", len(recs3), more3)
	}
	// no overlaps: all 5 versions distinct & increasing across pages
	all := append(append(recs, recs2...), recs3...)
	for i := 1; i < len(all); i++ {
		if all[i].Version <= all[i-1].Version {
			t.Errorf("version overlap/disorder at %d", i)
		}
	}
}

// 6. per-user isolation: user A never sees user B's records.
func TestPerUserIsolation(t *testing.T) {
	s, _ := newStore(t)
	now := time.Now()
	push(t, s, "userA", change("ca", "notes", "ra", `{}`, now))
	push(t, s, "userB", change("cb", "notes", "rb", `{}`, now))

	recsA, _, _ := pull(t, s, "userA", 0, 100)
	if len(recsA) != 1 || recsA[0].RecordID != "ra" {
		t.Errorf("userA should see only ra, got %+v", recsA)
	}
}

// 7. tenant isolation: a second schema is independent.
func TestTenantIsolation(t *testing.T) {
	s, pg := newStore(t)
	pg.CreateTenantSchema(t, "tenant_other", syncdb.InitSyncUp)
	now := time.Now()

	push(t, s, "u1", change("c1", "notes", "r1", `{}`, now))

	// pull from the other tenant's schema → empty
	recs, _, _, err := s.Pull(context.Background(), "tenant_other", "u1", "", 0, 100)
	if err != nil {
		t.Fatalf("pull other: %v", err)
	}
	if len(recs) != 0 {
		t.Errorf("tenant_other should be empty, got %d", len(recs))
	}
}

// 8. tenant-wide collection: shared records visible to all users; per-user
// records stay private.
func TestTenantWideCollection(t *testing.T) {
	pg := testutil.NewPostgres(t)
	pg.CreateTenantSchema(t, schema, syncdb.InitSyncUp)
	// "catalog" is shared; "notes" stays per-user
	s := syncdb.New(pg.DB, "catalog")
	now := time.Now()

	// Ada pushes a shared catalog item + her own note
	push(t, s, "ada",
		change("c1", "catalog", "item1", `{"name":"Laptop"}`, now),
		change("c2", "notes", "n1", `{"t":"ada note"}`, now),
	)
	// Bob pushes his own note
	push(t, s, "bob", change("c3", "notes", "n2", `{"t":"bob note"}`, now))

	// Bob pulls → sees the shared catalog (Ada pushed it) + his own note, NOT Ada's note
	recs, _, _, err := s.Pull(context.Background(), schema, "bob", "", 0, 100)
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	got := map[string]bool{}
	for _, r := range recs {
		got[r.RecordID] = true
	}
	if !got["item1"] {
		t.Error("bob should see shared catalog item1")
	}
	if !got["n2"] {
		t.Error("bob should see his own note n2")
	}
	if got["n1"] {
		t.Error("bob should NOT see ada's private note n1")
	}
}

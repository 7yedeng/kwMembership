package processor

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"kwmembership/internal/config"
	"kwmembership/internal/store"
)

func TestStaleLeaseCannotWriteAfterTakeover(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repository, err := store.Open(filepath.Join(t.TempDir(), "stale-takeover.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	if _, err := repository.DB().ExecContext(ctx, `CREATE TABLE membership_fulfillments (
      id TEXT PRIMARY KEY,order_id TEXT NOT NULL,order_no TEXT NOT NULL,target_tier TEXT NOT NULL,
      state TEXT NOT NULL,current_stage TEXT,run_mode TEXT,account_lock_key TEXT,state_revision INTEGER NOT NULL DEFAULT 0,
      retry_at TEXT,money_boundary_at TEXT,card_reservation_id TEXT,failure_code TEXT,created_at TEXT NOT NULL,
      updated_at TEXT NOT NULL,completed_at TEXT
    ); CREATE TABLE membership_outbox (
      id TEXT PRIMARY KEY,event_type TEXT NOT NULL,fulfillment_id TEXT,state_revision INTEGER,
      payload TEXT NOT NULL,created_at TEXT NOT NULL
    ); INSERT INTO membership_fulfillments
      (id,order_id,order_no,target_tier,state,state_revision,created_at,updated_at)
      VALUES ('mf-stale','order-stale','ORDER-STALE','plus','QUEUED',0,
        '2026-07-20T00:00:00.000Z','2026-07-20T00:00:00.000Z')`); err != nil {
		t.Fatal(err)
	}
	initial := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	if err := repository.EnsureLeaseTable(ctx, initial); err != nil {
		t.Fatal(err)
	}
	stale, err := repository.AcquireLease(ctx, "go", "stale-token", "test", initial, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	runner := New(config.Config{
		HTTPTimeout: time.Second, MaintenancePath: filepath.Join(t.TempDir(), "maintenance.json"),
	}, repository, stale, nil)
	runner.now = func() time.Time { return initial }
	if err := runner.assertWorkAllowed(ctx); err != nil {
		t.Fatalf("initial lease assertion: %v", err)
	}

	takeoverAt := initial.Add(2 * time.Minute)
	if _, err := repository.AcquireLease(ctx, "go", "replacement-token", "test", takeoverAt, time.Minute); err != nil {
		t.Fatalf("take over expired lease: %v", err)
	}
	runner.now = func() time.Time { return takeoverAt }
	_, err = runner.transition(ctx, "mf-stale", "BROWSER_LEASE_WAIT", takeoverAt, transitionOptions{Notify: true})
	if !errors.Is(err, store.ErrLeaseLost) {
		t.Fatalf("stale writer error = %v, want ErrLeaseLost", err)
	}

	var state string
	var revision int64
	if err := repository.DB().QueryRowContext(ctx, `SELECT state,state_revision
      FROM membership_fulfillments WHERE id='mf-stale'`).Scan(&state, &revision); err != nil {
		t.Fatal(err)
	}
	var outbox int
	if err := repository.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM membership_outbox`).Scan(&outbox); err != nil {
		t.Fatal(err)
	}
	if state != "QUEUED" || revision != 0 || outbox != 0 {
		t.Fatalf("stale epoch wrote state=%s revision=%d outbox=%d", state, revision, outbox)
	}
}

func TestNewRunOnceFailsClosedWithoutLease(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repository, err := store.Open(filepath.Join(t.TempDir(), "missing-lease.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	now := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	if err := repository.EnsureLeaseTable(ctx, now); err != nil {
		t.Fatal(err)
	}
	runner := New(config.Config{
		HTTPTimeout: time.Second, MaintenancePath: filepath.Join(t.TempDir(), "maintenance.json"),
	}, repository, store.Lease{}, nil)
	runner.now = func() time.Time { return now }
	if _, err := runner.RunOnce(ctx); !errors.Is(err, store.ErrLeaseLost) {
		t.Fatalf("RunOnce without lease error = %v, want ErrLeaseLost", err)
	}
	var owner sql.NullString
	if err := repository.DB().QueryRowContext(ctx, `SELECT owner FROM membership_processor_lease WHERE id='default'`).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if owner.Valid {
		t.Fatalf("RunOnce without lease changed owner to %q", owner.String)
	}
}

package processor

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"kwmembership/internal/store"
)

func TestAccountLockMatchesNodeContract(t *testing.T) {
	got, err := deriveAccountLock("0123456789abcdef", "", "User@Example.COM")
	if err != nil {
		t.Fatal(err)
	}
	const want = "mfalk_v1_5cc25c7618cd034f9852997ab97b2c906aa2f5f1164f49efb3ae1891c167df5c"
	if got != want {
		t.Fatalf("lock = %s, want %s", got, want)
	}
}

func TestTransitionCannotReviveCancelledFulfillment(t *testing.T) {
	ctx := context.Background()
	repository, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	repository.DB().Exec(`CREATE TABLE membership_fulfillments (
    id TEXT PRIMARY KEY, order_id TEXT NOT NULL, order_no TEXT NOT NULL, target_tier TEXT NOT NULL,
    state TEXT NOT NULL, current_stage TEXT, run_mode TEXT, account_lock_key TEXT,
    state_revision INTEGER NOT NULL, retry_at TEXT, money_boundary_at TEXT, card_reservation_id TEXT,
    failure_code TEXT, created_at TEXT NOT NULL, updated_at TEXT NOT NULL, completed_at TEXT
  ); CREATE TABLE membership_outbox (
    id TEXT PRIMARY KEY,event_type TEXT NOT NULL,fulfillment_id TEXT,state_revision INTEGER,
    payload TEXT NOT NULL,created_at TEXT NOT NULL
  );`)
	at := "2026-07-20T00:00:00.000Z"
	if _, err := repository.DB().Exec(`INSERT INTO membership_fulfillments
    (id,order_id,order_no,target_tier,state,state_revision,created_at,updated_at)
    VALUES ('mf-1','order-1','ORDER-1','plus','CANCELLED',2,?,?)`, at, at); err != nil {
		t.Fatal(err)
	}
	item, err := transition(ctx, repository, "mf-1", "BROWSER_LEASE_WAIT",
		time.Date(2026, 7, 20, 0, 1, 0, 0, time.UTC), transitionOptions{Notify: true})
	if err != nil {
		t.Fatal(err)
	}
	if item.State != "CANCELLED" || item.StateRevision != 2 {
		t.Fatalf("cancelled fulfillment changed: %+v", item)
	}
	var outboxCount int
	if err := repository.DB().QueryRow(`SELECT COUNT(*) FROM membership_outbox`).Scan(&outboxCount); err != nil {
		t.Fatal(err)
	}
	if outboxCount != 0 {
		t.Fatalf("cancelled transition emitted %d outbox rows", outboxCount)
	}
}

func TestTransitionRejectsStaleRevision(t *testing.T) {
	ctx := context.Background()
	repository, err := store.Open(filepath.Join(t.TempDir(), "state-revision.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	if _, err := repository.DB().Exec(`CREATE TABLE membership_fulfillments (
    id TEXT PRIMARY KEY, order_id TEXT NOT NULL, order_no TEXT NOT NULL, target_tier TEXT NOT NULL,
    state TEXT NOT NULL, current_stage TEXT, run_mode TEXT, account_lock_key TEXT,
    state_revision INTEGER NOT NULL, retry_at TEXT, money_boundary_at TEXT, card_reservation_id TEXT,
    failure_code TEXT, created_at TEXT NOT NULL, updated_at TEXT NOT NULL, completed_at TEXT
  ); CREATE TABLE membership_outbox (
    id TEXT PRIMARY KEY,event_type TEXT NOT NULL,fulfillment_id TEXT,state_revision INTEGER,
    payload TEXT NOT NULL,created_at TEXT NOT NULL
  ); INSERT INTO membership_fulfillments
    (id,order_id,order_no,target_tier,state,state_revision,created_at,updated_at)
    VALUES ('mf-1','order-1','ORDER-1','plus','ACCOUNT_CHECKING',2,
      '2026-07-20T00:00:00.000Z','2026-07-20T00:00:00.000Z')`); err != nil {
		t.Fatal(err)
	}
	expected := int64(1)
	_, err = transition(ctx, repository, "mf-1", "BROWSER_LEASE_WAIT",
		time.Date(2026, 7, 20, 0, 1, 0, 0, time.UTC), transitionOptions{ExpectedRevision: &expected, Notify: true})
	if got := errorCode(err); got != "MEMBERSHIP_REVISION_CONFLICT" {
		t.Fatalf("stale transition error code = %s, err=%v", got, err)
	}
	var state string
	var revision, outboxCount int
	if err := repository.DB().QueryRow(`SELECT state,state_revision FROM membership_fulfillments WHERE id='mf-1'`).
		Scan(&state, &revision); err != nil {
		t.Fatal(err)
	}
	if err := repository.DB().QueryRow(`SELECT COUNT(*) FROM membership_outbox`).Scan(&outboxCount); err != nil {
		t.Fatal(err)
	}
	if state != "ACCOUNT_CHECKING" || revision != 2 || outboxCount != 0 {
		t.Fatalf("stale transition wrote state=%s revision=%d outbox=%d", state, revision, outboxCount)
	}
	if _, err := repository.DB().Exec(`UPDATE membership_fulfillments SET state='ACCOUNT_ALREADY_SUBSCRIBED' WHERE id='mf-1'`); err != nil {
		t.Fatal(err)
	}
	frozen, err := transition(ctx, repository, "mf-1", "QUEUED",
		time.Date(2026, 7, 20, 0, 2, 0, 0, time.UTC), transitionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if frozen.State != "ACCOUNT_ALREADY_SUBSCRIBED" || frozen.StateRevision != 2 {
		t.Fatalf("terminal fulfillment was revived: %+v", frozen)
	}
}

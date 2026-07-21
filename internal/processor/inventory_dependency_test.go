package processor

import (
	"context"
	"database/sql"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kwmembership/internal/config"
	"kwmembership/internal/provider"
	"kwmembership/internal/secure"
	"kwmembership/internal/store"
)

func TestInventoryRejectsEmptyPageBeforeReportedTotal(t *testing.T) {
	ctx := context.Background()
	repository, err := store.Open(filepath.Join(t.TempDir(), "inventory-pagination.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	if _, err := repository.DB().Exec(`CREATE TABLE card_inventory_run_items (
    run_id TEXT NOT NULL,upstream_card_id INTEGER NOT NULL,status TEXT NOT NULL,attempt_count INTEGER NOT NULL,
    updated_at TEXT NOT NULL,PRIMARY KEY(run_id,upstream_card_id)
  ); CREATE TABLE card_inventory_runs (
    id TEXT PRIMARY KEY,status TEXT NOT NULL,next_page INTEGER NOT NULL,total_cards INTEGER,
    discovered_cards INTEGER NOT NULL,last_error_code TEXT,updated_at TEXT NOT NULL
  ); INSERT INTO card_inventory_runs
    (id,status,next_page,total_cards,discovered_cards,updated_at)
    VALUES ('run-1','discovering',2,3,0,'2026-07-20T01:00:00.000Z')`); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 20, 1, 0, 0, 0, time.UTC)
	if err := repository.EnsureLeaseTable(ctx, now); err != nil {
		t.Fatal(err)
	}
	lease, err := repository.AcquireLease(ctx, "go", "inventory-pagination", "test", now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	httpClient := &http.Client{Transport: inventoryRoundTripper(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"code":0,"data":{"total":3,"list":[]}}`)),
		}, nil
	})}
	client, err := provider.NewSpaceXClient(httpClient, "app", "secret")
	if err != nil {
		t.Fatal(err)
	}
	processor := &Processor{
		config: config.Config{MaintenancePath: filepath.Join(t.TempDir(), "standby")},
		store:  repository, lease: lease, now: func() time.Time { return now },
	}
	run := inventoryRun{ID: "run-1", Status: "discovering", NextPage: 2,
		Total: sql.NullInt64{Int64: 3, Valid: true}}
	err = processor.discoverInventoryPage(ctx, client, run, now)
	if got := errorCode(err); got != "SPACEXCARD_CONTRACT_DRIFT" {
		t.Fatalf("empty partial page error code = %s, err=%v", got, err)
	}
}

func TestInventoryReturnsPersistedProviderFailureWithoutCircuitAccountingOverride(t *testing.T) {
	ctx := context.Background()
	repository, err := store.Open(filepath.Join(t.TempDir(), "inventory-dependency.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	if _, err := repository.DB().Exec(`CREATE TABLE membership_fulfillment_settings (
    id TEXT PRIMARY KEY,inventory_status TEXT NOT NULL,spacexcard_app_id TEXT,
    spacexcard_app_secret_encrypted TEXT,last_inventory_error TEXT,updated_at TEXT,updated_by TEXT
  ); CREATE TABLE card_inventory_runs (
    id TEXT PRIMARY KEY,mode TEXT NOT NULL,status TEXT NOT NULL,next_page INTEGER NOT NULL,total_cards INTEGER,
    discovered_cards INTEGER NOT NULL DEFAULT 0,processed_cards INTEGER NOT NULL DEFAULT 0,held_cards INTEGER NOT NULL DEFAULT 0,
    last_error_code TEXT,started_at TEXT NOT NULL,updated_at TEXT NOT NULL,completed_at TEXT,locked_at TEXT,locked_by TEXT
  ); CREATE TABLE fulfillment_dependency_circuits (
    id TEXT PRIMARY KEY,dependency TEXT NOT NULL,scope_key TEXT NOT NULL,state TEXT NOT NULL,
    failure_count INTEGER NOT NULL DEFAULT 0,opened_at TEXT,retry_at TEXT,recovery_revision INTEGER NOT NULL DEFAULT 0,
    reason_code TEXT,updated_at TEXT NOT NULL,UNIQUE(dependency,scope_key)
  ); CREATE TABLE membership_outbox (
    id TEXT PRIMARY KEY,event_type TEXT NOT NULL,fulfillment_id TEXT,state_revision INTEGER,payload TEXT NOT NULL,created_at TEXT NOT NULL
  )`); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 20, 1, 0, 0, 0, time.UTC)
	if err := repository.EnsureLeaseTable(ctx, now); err != nil {
		t.Fatal(err)
	}
	lease, err := repository.AcquireLease(ctx, "go", "inventory-test", "test", now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	decrypter, err := secure.NewDecrypter("inventory-test-secret")
	if err != nil {
		t.Fatal(err)
	}
	credential, err := decrypter.Encrypt("openapi-secret")
	if err != nil {
		t.Fatal(err)
	}
	at := store.ISO(now)
	if _, err := repository.DB().Exec(`INSERT INTO membership_fulfillment_settings
    (id,inventory_status,spacexcard_app_id,spacexcard_app_secret_encrypted,updated_at)
		VALUES ('default','running','app',?,?)`, credential, at); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.DB().Exec(`INSERT INTO card_inventory_runs
		(id,mode,status,next_page,started_at,updated_at) VALUES ('run-1','full','discovering',1,?,?)`, at, at); err != nil {
		t.Fatal(err)
	}

	client := &http.Client{Transport: inventoryRoundTripper(func(*http.Request) (*http.Response, error) {
		// Simulate bookkeeping storage failing only after the provider call was acquired.
		if _, err := repository.DB().Exec(`DROP TABLE fulfillment_dependency_circuits`); err != nil {
			return nil, err
		}
		return &http.Response{
			StatusCode: http.StatusServiceUnavailable,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("")),
		}, nil
	}), Timeout: time.Second}
	processor := &Processor{
		config:     config.Config{MaintenancePath: filepath.Join(t.TempDir(), "standby")},
		store:      repository,
		lease:      lease,
		decrypter:  decrypter,
		httpClient: client,
		now:        func() time.Time { return now },
	}

	processed, err := processor.tickInventory(ctx)
	if !processed {
		t.Fatal("provider failure was not reported as processed inventory work")
	}
	if got := errorCode(err); got != "SPACEXCARD_UNAVAILABLE" {
		t.Fatalf("error code = %s, err=%v", got, err)
	}
	if !strings.Contains(err.Error(), "persist dependency circuit state") {
		t.Fatalf("circuit accounting failure is invisible: %v", err)
	}
	var runCode, settingsCode string
	var lockedAt, lockedBy sql.NullString
	if err := repository.DB().QueryRow(`SELECT last_error_code,locked_at,locked_by FROM card_inventory_runs WHERE id='run-1'`).
		Scan(&runCode, &lockedAt, &lockedBy); err != nil {
		t.Fatal(err)
	}
	if err := repository.DB().QueryRow(`SELECT last_inventory_error FROM membership_fulfillment_settings WHERE id='default'`).
		Scan(&settingsCode); err != nil {
		t.Fatal(err)
	}
	if runCode != "SPACEXCARD_UNAVAILABLE" || settingsCode != "SPACEXCARD_UNAVAILABLE" || lockedAt.Valid || lockedBy.Valid {
		t.Fatalf("inventory projection: run=%s settings=%s lockedAt=%v lockedBy=%v",
			runCode, settingsCode, lockedAt, lockedBy)
	}
}

type inventoryRoundTripper func(*http.Request) (*http.Response, error)

func (roundTrip inventoryRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

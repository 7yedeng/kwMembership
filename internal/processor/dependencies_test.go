package processor

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"kwmembership/internal/store"
)

func TestAcquireCircuitAllowsOneStaleHalfOpenTakeover(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repository, err := store.Open(filepath.Join(t.TempDir(), "circuit.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	if _, err := repository.DB().Exec(`CREATE TABLE fulfillment_dependency_circuits (
    id TEXT PRIMARY KEY,dependency TEXT NOT NULL,scope_key TEXT NOT NULL,state TEXT NOT NULL,
    failure_count INTEGER NOT NULL DEFAULT 0,opened_at TEXT,retry_at TEXT,recovery_revision INTEGER NOT NULL DEFAULT 0,
    reason_code TEXT,updated_at TEXT NOT NULL,UNIQUE(dependency,scope_key)
  )`); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 20, 1, 0, 0, 0, time.UTC)
	if _, err := repository.DB().Exec(`INSERT INTO fulfillment_dependency_circuits
    (id,dependency,scope_key,state,failure_count,retry_at,updated_at)
    VALUES ('circuit-1','membership_provider','default','half_open',3,?,?)`,
		store.ISO(now.Add(-time.Hour)), store.ISO(now.Add(-circuitProbeTimeout-time.Millisecond))); err != nil {
		t.Fatal(err)
	}

	processor := &Processor{store: repository}
	start := make(chan struct{})
	results := make(chan bool, 2)
	errorsSeen := make(chan error, 2)
	var workers sync.WaitGroup
	for range 2 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			allowed, err := processor.acquireCircuit(ctx, "membership_provider", "default", now)
			results <- allowed
			errorsSeen <- err
		}()
	}
	close(start)
	workers.Wait()
	close(results)
	close(errorsSeen)

	allowedCount := 0
	for allowed := range results {
		if allowed {
			allowedCount++
		}
	}
	for err := range errorsSeen {
		if err != nil {
			t.Fatal(err)
		}
	}
	if allowedCount != 1 {
		t.Fatalf("stale half-open circuit allowed %d probes, want 1", allowedCount)
	}
}

func TestCircuitAccountingErrorDoesNotReplacePrimaryCode(t *testing.T) {
	primary := coded("MEMBERSHIP_PROVIDER_UNAVAILABLE", "provider unavailable")
	combined := withCircuitAccounting(primary, errors.New("database is read-only"))
	if got := errorCode(combined); got != "MEMBERSHIP_PROVIDER_UNAVAILABLE" {
		t.Fatalf("combined error code = %s", got)
	}
	if !strings.Contains(combined.Error(), "persist dependency circuit state") {
		t.Fatalf("circuit accounting error is invisible: %v", combined)
	}

	accountingOnly := withCircuitAccounting(nil, errors.New("database is read-only"))
	if got := errorCode(accountingOnly); got != "DEPENDENCY_CIRCUIT_ACCOUNTING_FAILED" {
		t.Fatalf("accounting-only error code = %s", got)
	}
}

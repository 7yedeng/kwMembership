package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"kwmembership/internal/config"
	"kwmembership/internal/store"
)

func TestMaintenanceStatus(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "maintenance.json")

	status, err := maintenanceStatus(marker)
	if err != nil {
		t.Fatalf("read missing marker: %v", err)
	}
	if status != "active" {
		t.Fatalf("status without marker = %q, want active", status)
	}

	if err := os.WriteFile(marker, []byte("{}"), 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	status, err = maintenanceStatus(marker)
	if err != nil {
		t.Fatalf("read present marker: %v", err)
	}
	if status != "standby" {
		t.Fatalf("status with marker = %q, want standby", status)
	}
}

func TestMaintenanceDrainWaitsForInflightTick(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	marker := filepath.Join(t.TempDir(), "maintenance.json")
	repository, lease := testLease(t, ctx)
	defer repository.Close()
	cfg := config.Config{MaintenancePath: marker, LeaseTTL: time.Minute}
	drain := newMaintenanceDrain()

	started, err := drain.beginTick(ctx, cfg, repository, lease)
	if err != nil || !started {
		t.Fatalf("begin tick: started=%t err=%v", started, err)
	}
	if err := os.WriteFile(marker, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := drain.heartbeat(ctx, cfg, repository, lease); err != nil {
		t.Fatalf("heartbeat during tick: %v", err)
	}
	if status := testLeaseStatus(t, ctx, repository); status != "active" {
		t.Fatalf("lease status during tick = %q, want active", status)
	}

	drain.finishTick()
	if err := drain.heartbeat(ctx, cfg, repository, lease); err != nil {
		t.Fatalf("heartbeat after drain: %v", err)
	}
	if status := testLeaseStatus(t, ctx, repository); status != "standby" {
		t.Fatalf("lease status after tick = %q, want standby", status)
	}

	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	started, err = drain.beginTick(ctx, cfg, repository, lease)
	if err != nil || !started {
		t.Fatalf("resume tick: started=%t err=%v", started, err)
	}
	defer drain.finishTick()
	if status := testLeaseStatus(t, ctx, repository); status != "active" {
		t.Fatalf("lease status at resumed tick start = %q, want active", status)
	}
}

func TestMaintenanceMarkerAndTickAdmissionAreSerialized(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	marker := filepath.Join(t.TempDir(), "maintenance.json")
	repository, lease := testLease(t, ctx)
	defer repository.Close()
	cfg := config.Config{MaintenancePath: marker, LeaseTTL: time.Minute}
	drain := newMaintenanceDrain()

	// Hold the admission lock while the marker appears. beginTick cannot have
	// observed an old marker state and then start after maintenance begins.
	drain.mu.Lock()
	result := make(chan struct {
		started bool
		err     error
	}, 1)
	go func() {
		started, err := drain.beginTick(ctx, cfg, repository, lease)
		result <- struct {
			started bool
			err     error
		}{started: started, err: err}
	}()
	if err := os.WriteFile(marker, []byte("{}"), 0o600); err != nil {
		drain.mu.Unlock()
		t.Fatal(err)
	}
	drain.mu.Unlock()

	admission := <-result
	if admission.err != nil || admission.started {
		t.Fatalf("tick crossed maintenance marker: started=%t err=%v", admission.started, admission.err)
	}
	if err := drain.heartbeat(ctx, cfg, repository, lease); err != nil {
		t.Fatal(err)
	}
	if status := testLeaseStatus(t, ctx, repository); status != "standby" {
		t.Fatalf("lease status after fenced admission = %q, want standby", status)
	}
}

func testLease(t *testing.T, ctx context.Context) (*store.Store, store.Lease) {
	t.Helper()
	repository, err := store.Open(filepath.Join(t.TempDir(), "lease.db"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := repository.EnsureLeaseTable(ctx, now); err != nil {
		repository.Close()
		t.Fatal(err)
	}
	lease, err := repository.AcquireLease(ctx, "go", "maintenance-drain-test", "test", now, time.Minute)
	if err != nil {
		repository.Close()
		t.Fatal(err)
	}
	return repository, lease
}

func testLeaseStatus(t *testing.T, ctx context.Context, repository *store.Store) string {
	t.Helper()
	var status string
	if err := repository.DB().QueryRowContext(ctx, `SELECT status FROM membership_processor_lease WHERE id='default'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

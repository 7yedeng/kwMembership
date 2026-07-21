package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestLeaseFencesExpiredOwner(t *testing.T) {
	ctx := context.Background()
	repository, err := Open(filepath.Join(t.TempDir(), "lease.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	now := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	if err := repository.EnsureLeaseTable(ctx, now); err != nil {
		t.Fatal(err)
	}
	first, err := repository.AcquireLease(ctx, "go", "first", "test", now, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.AcquireLease(ctx, "node", "second", "test", now.Add(time.Second), 10*time.Second); !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("active takeover err = %v", err)
	}
	second, err := repository.AcquireLease(ctx, "node", "second", "test", now.Add(10*time.Second), 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if second.Epoch != first.Epoch+1 {
		t.Fatalf("epoch %d after %d", second.Epoch, first.Epoch)
	}
	if err := repository.HeartbeatLease(ctx, first, "active", now.Add(11*time.Second), 10*time.Second); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale heartbeat err = %v", err)
	}
}

func TestRecordTickPreservesLeaseAndErrorProjection(t *testing.T) {
	ctx := context.Background()
	repository, err := Open(filepath.Join(t.TempDir(), "lease.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	now := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	if err := repository.EnsureLeaseTable(ctx, now); err != nil {
		t.Fatal(err)
	}
	lease, err := repository.AcquireLease(ctx, "go", "holder", "test", now, 20*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.RecordTick(ctx, lease, now.Add(time.Second), codedTestError("UPSTREAM_TIMEOUT")); err != nil {
		t.Fatal(err)
	}
	var owner, token, status, code string
	if err := repository.DB().QueryRow(`SELECT owner,holder_token,status,last_error_code FROM membership_processor_lease WHERE id='default'`).
		Scan(&owner, &token, &status, &code); err != nil {
		t.Fatal(err)
	}
	if owner != "go" || token != "holder" || status != "active" || code != "UPSTREAM_TIMEOUT" {
		t.Fatalf("unexpected projection: %s %s %s %s", owner, token, status, code)
	}
}

func TestReleaseLeaseRejectsStaleHolder(t *testing.T) {
	ctx := context.Background()
	repository, err := Open(filepath.Join(t.TempDir(), "lease-release.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	now := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	if err := repository.EnsureLeaseTable(ctx, now); err != nil {
		t.Fatal(err)
	}
	lease, err := repository.AcquireLease(ctx, "go", "current-holder", "test", now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	stale := lease
	stale.Token = "stale-holder"
	if err := repository.ReleaseLease(ctx, stale, now.Add(time.Second)); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale release error = %v, want ErrLeaseLost", err)
	}
	if err := repository.ReleaseLease(ctx, lease, now.Add(2*time.Second)); err != nil {
		t.Fatalf("current release: %v", err)
	}
}

type codedTestError string

func (e codedTestError) Error() string { return string(e) }
func (e codedTestError) Code() string  { return string(e) }

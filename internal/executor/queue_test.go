package executor

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"kwmembership/internal/store"
)

func TestQueueLeasesHighestPriorityOneAtATimeAndHardExpires(t *testing.T) {
	ctx := context.Background()
	repository, err := store.Open(filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	if _, err := repository.DB().ExecContext(ctx, `CREATE TABLE membership_checkout_commands (
    id TEXT PRIMARY KEY, fulfillment_id TEXT, stage_key TEXT, attempt_no INTEGER, command_kind TEXT,
    priority_class TEXT, target_tier TEXT, adapter_version TEXT, price_contract_id TEXT,
    price_contract_version INTEGER, fulfillment_revision INTEGER, state TEXT,
    lease_epoch INTEGER DEFAULT 0, lease_token_sha256 TEXT, leased_by TEXT, leased_at TEXT,
    heartbeat_at TEXT, lease_expires_at TEXT, hard_deadline_at TEXT, material_claimed_at TEXT,
    available_at TEXT, payment_ready_at TEXT, outcome_code TEXT, sanitized_diagnostic TEXT,
    created_at TEXT, updated_at TEXT, ended_at TEXT,
    UNIQUE(fulfillment_id, stage_key, attempt_no, command_kind)
  )`); err != nil {
		t.Fatal(err)
	}
	queue := NewQueue(repository.DB())
	now := time.Date(2026, 8, 13, 1, 2, 3, 0, time.UTC)
	for _, item := range []CommandInput{
		{ID: "normal", FulfillmentID: "f-normal", StageKey: "plus", AttemptNo: 1, CommandKind: "payment", Priority: PriorityNormal, TargetTier: "plus", AdapterVersion: "python-session-card-checkout-v1", PriceContractID: "price", PriceContractVersion: 1, CreatedAt: now, AvailableAt: now, PaymentReadyAt: now},
		{ID: "upgrade", FulfillmentID: "f-upgrade", StageKey: "upgrade", AttemptNo: 1, CommandKind: "payment", Priority: PriorityUpgrade, TargetTier: "x5", AdapterVersion: "python-session-card-checkout-v1", PriceContractID: "price", PriceContractVersion: 1, CreatedAt: now.Add(time.Second), AvailableAt: now, PaymentReadyAt: now},
		{ID: "recovery", FulfillmentID: "f-recovery", StageKey: "plus", AttemptNo: 1, CommandKind: "payment", Priority: PriorityRecovery, TargetTier: "plus", AdapterVersion: "python-session-card-checkout-v1", PriceContractID: "price", PriceContractVersion: 1, CreatedAt: now.Add(2 * time.Second), AvailableAt: now, PaymentReadyAt: now},
	} {
		if _, err := queue.Enqueue(ctx, item); err != nil {
			t.Fatal(err)
		}
	}
	lease, err := queue.LeaseNext(ctx, "python-1", now)
	if err != nil {
		t.Fatal(err)
	}
	if lease.Command.ID != "recovery" || lease.Command.LeaseEpoch != 1 || lease.Command.HardDeadlineAt.Sub(now) != HardDeadline {
		t.Fatalf("unexpected lease: %+v", lease.Command)
	}
	if _, err := queue.LeaseNext(ctx, "python-2", now); !errors.Is(err, ErrNoCommand) {
		t.Fatalf("second active lease err = %v", err)
	}
	updated, err := queue.Heartbeat(ctx, lease, now.Add(10*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if updated.HardDeadlineAt.Sub(now) != HardDeadline {
		t.Fatalf("heartbeat extended hard deadline to %s", updated.HardDeadlineAt)
	}
	if err := queue.Report(ctx, lease, "PRE_SUBMIT_FIXTURE_SUCCESS", `{"state":"reported"}`, now.Add(15*time.Second)); err != nil {
		t.Fatal(err)
	}
	upgrade, err := queue.LeaseNext(ctx, "python-1", now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if upgrade.Command.ID != "upgrade" {
		t.Fatalf("next command = %s", upgrade.Command.ID)
	}
	if err := queue.SetActionRequired(ctx, upgrade, `{"state":"PAYMENT_ACTION_REQUIRED"}`, now.Add(70*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := queue.Heartbeat(ctx, upgrade, now.Add(6*time.Minute)); !errors.Is(err, ErrLeaseExpired) {
		t.Fatalf("expired heartbeat err = %v", err)
	}
	var state string
	var code *string
	if err := repository.DB().QueryRow(`SELECT state,outcome_code FROM membership_checkout_commands WHERE id='upgrade'`).Scan(&state, &code); err != nil {
		t.Fatal(err)
	}
	if state != "expired" || code == nil || *code != "EXECUTOR_LEASE_EXPIRED" {
		t.Fatalf("expired command = %s/%v", state, code)
	}
}

func TestQueueRejectsSensitiveCommandShape(t *testing.T) {
	ctx := context.Background()
	repository, err := store.Open(filepath.Join(t.TempDir(), "queue-shape.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	if _, err := repository.DB().ExecContext(ctx, `CREATE TABLE membership_checkout_commands (id TEXT PRIMARY KEY, fulfillment_id TEXT, stage_key TEXT, attempt_no INTEGER, command_kind TEXT, priority_class TEXT, target_tier TEXT, adapter_version TEXT, price_contract_id TEXT, price_contract_version INTEGER, fulfillment_revision INTEGER, state TEXT, lease_epoch INTEGER DEFAULT 0, lease_token_sha256 TEXT, leased_by TEXT, leased_at TEXT, heartbeat_at TEXT, lease_expires_at TEXT, hard_deadline_at TEXT, material_claimed_at TEXT, available_at TEXT, payment_ready_at TEXT, outcome_code TEXT, sanitized_diagnostic TEXT, created_at TEXT, updated_at TEXT, ended_at TEXT, UNIQUE(fulfillment_id, stage_key, attempt_no, command_kind))`); err != nil {
		t.Fatal(err)
	}
	queue := NewQueue(repository.DB())
	if _, err := queue.Enqueue(ctx, CommandInput{ID: "invalid", FulfillmentID: "f", StageKey: "plus", AttemptNo: 1, CommandKind: "payment", Priority: "unexpected", TargetTier: "plus", AdapterVersion: "v", PriceContractID: "p", PriceContractVersion: 1, CreatedAt: time.Now()}); err == nil {
		t.Fatal("invalid priority unexpectedly accepted")
	}
}

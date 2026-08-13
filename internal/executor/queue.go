// Package executor contains the durable boundary between the Go membership
// workflow and the single Python payment execution process.
package executor

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"kwmembership/internal/store"
)

const (
	LeaseDuration = 20 * time.Second
	HardDeadline  = 5 * time.Minute
)

var (
	ErrNoCommand       = errors.New("no checkout command available")
	ErrCommandConflict = errors.New("checkout command changed")
	ErrLeaseExpired    = errors.New("checkout execution lease expired")
)

type Priority string

const (
	PriorityRecovery Priority = "recovery"
	PriorityUpgrade  Priority = "upgrade"
	PriorityNormal   Priority = "normal"
)

func validPriority(value Priority) bool {
	return value == PriorityRecovery || value == PriorityUpgrade || value == PriorityNormal
}

type CommandInput struct {
	ID                   string
	FulfillmentID        string
	StageKey             string
	AttemptNo            int64
	CommandKind          string
	Priority             Priority
	TargetTier           string
	AdapterVersion       string
	PriceContractID      string
	PriceContractVersion int64
	FulfillmentRevision  int64
	AvailableAt          time.Time
	PaymentReadyAt       time.Time
	CreatedAt            time.Time
}

type Command struct {
	ID                   string
	FulfillmentID        string
	StageKey             string
	AttemptNo            int64
	CommandKind          string
	Priority             Priority
	TargetTier           string
	AdapterVersion       string
	PriceContractID      string
	PriceContractVersion int64
	FulfillmentRevision  int64
	State                string
	LeaseEpoch           int64
	LeasedBy             string
	LeasedAt             *time.Time
	HeartbeatAt          *time.Time
	LeaseExpiresAt       *time.Time
	HardDeadlineAt       *time.Time
	MaterialClaimedAt    *time.Time
	AvailableAt          time.Time
	PaymentReadyAt       time.Time
	OutcomeCode          string
	SanitizedDiagnostic  string
	CreatedAt            time.Time
	UpdatedAt            time.Time
	EndedAt              *time.Time
}

type Lease struct {
	Command Command
	Token   string
}

type Queue struct {
	db *sql.DB
}

func NewQueue(db *sql.DB) *Queue { return &Queue{db: db} }

func (q *Queue) Enqueue(ctx context.Context, input CommandInput) (Command, error) {
	if input.ID == "" || input.FulfillmentID == "" || input.StageKey == "" || input.AttemptNo < 0 ||
		(input.CommandKind != "preflight" && input.CommandKind != "payment") ||
		!validPriority(input.Priority) || input.TargetTier == "" || input.AdapterVersion == "" ||
		input.PriceContractID == "" || input.PriceContractVersion <= 0 || input.FulfillmentRevision < 0 {
		return Command{}, fmt.Errorf("invalid checkout command input")
	}
	if input.CreatedAt.IsZero() {
		input.CreatedAt = time.Now().UTC()
	}
	if input.AvailableAt.IsZero() {
		input.AvailableAt = input.CreatedAt
	}
	if input.PaymentReadyAt.IsZero() {
		input.PaymentReadyAt = input.AvailableAt
	}
	_, err := q.db.ExecContext(ctx, `
    INSERT OR IGNORE INTO membership_checkout_commands
      (id,fulfillment_id,stage_key,attempt_no,command_kind,priority_class,target_tier,adapter_version,
       price_contract_id,price_contract_version,fulfillment_revision,state,available_at,
       payment_ready_at,created_at,updated_at)
    VALUES (?,?,?,?,?,?,?,?,?,?,?,'queued',?,?,?,?)`, input.ID, input.FulfillmentID, input.StageKey,
		input.AttemptNo, input.CommandKind, string(input.Priority), input.TargetTier, input.AdapterVersion, input.PriceContractID,
		input.PriceContractVersion, input.FulfillmentRevision, store.ISO(input.AvailableAt), store.ISO(input.PaymentReadyAt),
		store.ISO(input.CreatedAt), store.ISO(input.CreatedAt))
	if err != nil {
		return Command{}, err
	}
	return q.GetForAttempt(ctx, input.FulfillmentID, input.StageKey, input.AttemptNo, input.CommandKind)
}

func (q *Queue) Get(ctx context.Context, id string) (Command, error) {
	if strings.TrimSpace(id) == "" {
		return Command{}, ErrCommandConflict
	}
	return scanCommand(q.db.QueryRowContext(ctx, `SELECT
      id,fulfillment_id,stage_key,attempt_no,command_kind,priority_class,target_tier,adapter_version,
      price_contract_id,price_contract_version,fulfillment_revision,state,lease_epoch,
      leased_by,leased_at,heartbeat_at,lease_expires_at,hard_deadline_at,material_claimed_at,
      available_at,payment_ready_at,outcome_code,sanitized_diagnostic,created_at,updated_at,ended_at
    FROM membership_checkout_commands WHERE id=?`, id))
}

func (q *Queue) GetForAttempt(ctx context.Context, fulfillmentID, stageKey string, attemptNo int64, commandKind string) (Command, error) {
	return scanCommand(q.db.QueryRowContext(ctx, `SELECT
      id,fulfillment_id,stage_key,attempt_no,command_kind,priority_class,target_tier,adapter_version,
      price_contract_id,price_contract_version,fulfillment_revision,state,lease_epoch,
      leased_by,leased_at,heartbeat_at,lease_expires_at,hard_deadline_at,material_claimed_at,
      available_at,payment_ready_at,outcome_code,sanitized_diagnostic,created_at,updated_at,ended_at
    FROM membership_checkout_commands WHERE fulfillment_id=? AND stage_key=? AND attempt_no=? AND command_kind=?`,
		fulfillmentID, stageKey, attemptNo, commandKind))
}

func (q *Queue) ValidateLease(ctx context.Context, id, executorID string, epoch int64, token string, now time.Time) (Lease, error) {
	command, err := q.Get(ctx, id)
	if err != nil {
		return Lease{}, err
	}
	if command.State != "leased" && command.State != "action_required" || command.LeasedBy != executorID ||
		command.LeaseEpoch != epoch || token == "" || command.HardDeadlineAt == nil || command.LeaseExpiresAt == nil ||
		!command.HardDeadlineAt.After(now.UTC()) || !command.LeaseExpiresAt.After(now.UTC()) {
		return Lease{}, ErrLeaseExpired
	}
	var storedHash string
	if err := q.db.QueryRowContext(ctx, `SELECT lease_token_sha256 FROM membership_checkout_commands WHERE id=?`, id).Scan(&storedHash); err != nil {
		return Lease{}, err
	}
	if storedHash != hashToken(token) {
		return Lease{}, ErrLeaseExpired
	}
	return Lease{Command: command, Token: token}, nil
}

func (q *Queue) ClaimMaterial(ctx context.Context, lease Lease, now time.Time) (Command, error) {
	return q.ClaimMaterialWith(ctx, lease, now, nil)
}

func (q *Queue) ClaimMaterialWith(ctx context.Context, lease Lease, now time.Time, validate func(*sql.Tx) error) (Command, error) {
	now = now.UTC()
	var command Command
	err := withImmediate(ctx, q.db, func(tx *sql.Tx) error {
		if validate != nil {
			if err := validate(tx); err != nil {
				return err
			}
		}
		result, err := tx.ExecContext(ctx, `UPDATE membership_checkout_commands
      SET material_claimed_at=?, updated_at=?
      WHERE id=? AND state IN ('leased','action_required') AND leased_by=? AND lease_epoch=?
        AND lease_token_sha256=? AND material_claimed_at IS NULL
        AND hard_deadline_at>? AND lease_expires_at>?`, store.ISO(now), store.ISO(now), lease.Command.ID,
			lease.Command.LeasedBy, lease.Command.LeaseEpoch, hashToken(lease.Token), store.ISO(now), store.ISO(now))
		if err != nil {
			return err
		}
		changed, _ := result.RowsAffected()
		if changed != 1 {
			return ErrCommandConflict
		}
		var loadErr error
		command, loadErr = scanCommand(tx.QueryRowContext(ctx, `SELECT
        id,fulfillment_id,stage_key,attempt_no,command_kind,priority_class,target_tier,adapter_version,
        price_contract_id,price_contract_version,fulfillment_revision,state,lease_epoch,
        leased_by,leased_at,heartbeat_at,lease_expires_at,hard_deadline_at,material_claimed_at,
        available_at,payment_ready_at,outcome_code,sanitized_diagnostic,created_at,updated_at,ended_at
      FROM membership_checkout_commands WHERE id=?`, lease.Command.ID))
		return loadErr
	})
	return command, err
}

func (q *Queue) LeaseNext(ctx context.Context, executorID string, now time.Time) (Lease, error) {
	if strings.TrimSpace(executorID) == "" {
		return Lease{}, ErrCommandConflict
	}
	now = now.UTC()
	var leased Lease
	err := withImmediate(ctx, q.db, func(tx *sql.Tx) error {
		// A lost process is never silently requeued. It becomes an unknown
		// outcome and must be reconciled by the workflow owner.
		if _, err := tx.ExecContext(ctx, `UPDATE membership_checkout_commands
      SET state='expired', outcome_code=COALESCE(outcome_code,'EXECUTOR_LEASE_EXPIRED'),
          ended_at=COALESCE(ended_at,?), updated_at=?
      WHERE state IN ('leased','action_required')
        AND (hard_deadline_at<=? OR lease_expires_at<=?)`, store.ISO(now), store.ISO(now), store.ISO(now), store.ISO(now)); err != nil {
			return err
		}
		var active int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM membership_checkout_commands
      WHERE state IN ('leased','action_required')`).Scan(&active); err != nil {
			return err
		}
		if active != 0 {
			return ErrNoCommand
		}
		var id string
		err := tx.QueryRowContext(ctx, `SELECT id FROM membership_checkout_commands
      WHERE state='queued' AND available_at<=? ORDER BY
        CASE priority_class WHEN 'recovery' THEN 0 WHEN 'upgrade' THEN 1 ELSE 2 END,
        payment_ready_at, available_at, created_at, id LIMIT 1`, store.ISO(now)).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNoCommand
		}
		if err != nil {
			return err
		}
		token, err := randomToken()
		if err != nil {
			return err
		}
		leaseAt := now
		expires := now.Add(LeaseDuration)
		deadline := now.Add(HardDeadline)
		result, err := tx.ExecContext(ctx, `UPDATE membership_checkout_commands
      SET state='leased', lease_epoch=lease_epoch+1, lease_token_sha256=?, leased_by=?,
          leased_at=?, heartbeat_at=?, lease_expires_at=?, hard_deadline_at=?, updated_at=?
      WHERE id=? AND state='queued'`, hashToken(token), executorID, store.ISO(leaseAt), store.ISO(leaseAt),
			store.ISO(expires), store.ISO(deadline), store.ISO(now), id)
		if err != nil {
			return err
		}
		changed, _ := result.RowsAffected()
		if changed != 1 {
			return ErrCommandConflict
		}
		command, err := scanCommand(tx.QueryRowContext(ctx, `SELECT
        id,fulfillment_id,stage_key,attempt_no,command_kind,priority_class,target_tier,adapter_version,
        price_contract_id,price_contract_version,fulfillment_revision,state,lease_epoch,
        leased_by,leased_at,heartbeat_at,lease_expires_at,hard_deadline_at,material_claimed_at,
        available_at,payment_ready_at,outcome_code,sanitized_diagnostic,created_at,updated_at,ended_at
      FROM membership_checkout_commands WHERE id=?`, id))
		if err != nil {
			return err
		}
		leased = Lease{Command: command, Token: token}
		return nil
	})
	return leased, err
}

func (q *Queue) Heartbeat(ctx context.Context, lease Lease, now time.Time) (Command, error) {
	now = now.UTC()
	var command Command
	var logicalErr error
	err := withImmediate(ctx, q.db, func(tx *sql.Tx) error {
		expired, err := tx.ExecContext(ctx, `UPDATE membership_checkout_commands
      SET state='expired', outcome_code=COALESCE(outcome_code,'EXECUTOR_LEASE_EXPIRED'),
          ended_at=COALESCE(ended_at,?), updated_at=?
      WHERE id=? AND state IN ('leased','action_required') AND leased_by=?
        AND lease_epoch=? AND lease_token_sha256=?
        AND (hard_deadline_at<=? OR lease_expires_at<=?)`, store.ISO(now), store.ISO(now),
			lease.Command.ID, lease.Command.LeasedBy, lease.Command.LeaseEpoch, hashToken(lease.Token),
			store.ISO(now), store.ISO(now))
		if err != nil {
			return err
		}
		if changed, _ := expired.RowsAffected(); changed == 1 {
			logicalErr = ErrLeaseExpired
			return nil
		}
		result, err := tx.ExecContext(ctx, `UPDATE membership_checkout_commands
      SET heartbeat_at=?, lease_expires_at=MIN(hard_deadline_at,?), updated_at=?
      WHERE id=? AND state IN ('leased','action_required') AND leased_by=?
        AND lease_epoch=? AND lease_token_sha256=? AND hard_deadline_at>?`, store.ISO(now),
			store.ISO(now.Add(LeaseDuration)), store.ISO(now), lease.Command.ID, lease.Command.LeasedBy,
			lease.Command.LeaseEpoch, hashToken(lease.Token), store.ISO(now))
		if err != nil {
			return err
		}
		changed, _ := result.RowsAffected()
		if changed != 1 {
			logicalErr = ErrLeaseExpired
			return nil
		}
		var errGet error
		command, errGet = scanCommand(tx.QueryRowContext(ctx, `SELECT
      id,fulfillment_id,stage_key,attempt_no,command_kind,priority_class,target_tier,adapter_version,
      price_contract_id,price_contract_version,fulfillment_revision,state,lease_epoch,
      leased_by,leased_at,heartbeat_at,lease_expires_at,hard_deadline_at,material_claimed_at,
      available_at,payment_ready_at,outcome_code,sanitized_diagnostic,created_at,updated_at,ended_at
    FROM membership_checkout_commands WHERE id=?`, lease.Command.ID))
		return errGet
	})
	if err != nil {
		return command, err
	}
	return command, logicalErr
}

func (q *Queue) SetActionRequired(ctx context.Context, lease Lease, diagnostic string, now time.Time) error {
	return q.updateOwned(ctx, lease, now, `state='action_required', sanitized_diagnostic=?, updated_at=?`,
		diagnostic, store.ISO(now.UTC()))
}

func (q *Queue) Report(ctx context.Context, lease Lease, outcomeCode, diagnostic string, now time.Time) error {
	return q.updateOwned(ctx, lease, now, `state='reported', outcome_code=?, sanitized_diagnostic=?, ended_at=?, updated_at=?`,
		outcomeCode, diagnostic, store.ISO(now.UTC()), store.ISO(now.UTC()))
}

func (q *Queue) updateOwned(ctx context.Context, lease Lease, now time.Time, setClause string, args ...any) error {
	args = append(args, lease.Command.ID, lease.Command.LeasedBy, lease.Command.LeaseEpoch, hashToken(lease.Token), store.ISO(now.UTC()), store.ISO(now.UTC()))
	result, err := q.db.ExecContext(ctx, `UPDATE membership_checkout_commands SET `+setClause+`
    WHERE id=? AND state IN ('leased','action_required') AND leased_by=? AND lease_epoch=? AND lease_token_sha256=?
      AND hard_deadline_at>? AND lease_expires_at>?`, args...)
	if err != nil {
		return err
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		return ErrLeaseExpired
	}
	return nil
}

func randomToken() (string, error) {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}

func hashToken(token string) string {
	digest := sha256.Sum256([]byte(token))
	return hex.EncodeToString(digest[:])
}

func withImmediate(ctx context.Context, db *sql.DB, fn func(*sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	committed = true
	return nil
}

type scanner interface{ Scan(...any) error }

func scanCommand(row scanner) (Command, error) {
	var command Command
	var priority string
	var leasedBy sql.NullString
	var leasedAt, heartbeatAt, expiresAt, deadlineAt, materialAt, availableAt, readyAt, createdAt, updatedAt, endedAt sql.NullString
	var outcome, diagnostic sql.NullString
	if err := row.Scan(&command.ID, &command.FulfillmentID, &command.StageKey, &command.AttemptNo, &command.CommandKind,
		&priority, &command.TargetTier, &command.AdapterVersion, &command.PriceContractID,
		&command.PriceContractVersion, &command.FulfillmentRevision, &command.State, &command.LeaseEpoch,
		&leasedBy, &leasedAt, &heartbeatAt, &expiresAt, &deadlineAt, &materialAt, &availableAt,
		&readyAt, &outcome, &diagnostic, &createdAt, &updatedAt, &endedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Command{}, ErrNoCommand
		}
		return Command{}, err
	}
	command.Priority = Priority(priority)
	command.LeasedBy = leasedBy.String
	command.OutcomeCode = outcome.String
	command.SanitizedDiagnostic = diagnostic.String
	parse := func(value sql.NullString) (*time.Time, error) {
		if !value.Valid || value.String == "" {
			return nil, nil
		}
		parsed, err := time.Parse(time.RFC3339Nano, value.String)
		if err != nil {
			return nil, err
		}
		return &parsed, nil
	}
	var err error
	if command.AvailableAt, err = parseRequired(availableAt); err != nil {
		return Command{}, err
	}
	if command.PaymentReadyAt, err = parseRequired(readyAt); err != nil {
		return Command{}, err
	}
	if command.CreatedAt, err = parseRequired(createdAt); err != nil {
		return Command{}, err
	}
	if command.UpdatedAt, err = parseRequired(updatedAt); err != nil {
		return Command{}, err
	}
	if command.LeasedAt, err = parse(leasedAt); err != nil {
		return Command{}, err
	}
	if command.HeartbeatAt, err = parse(heartbeatAt); err != nil {
		return Command{}, err
	}
	if command.LeaseExpiresAt, err = parse(expiresAt); err != nil {
		return Command{}, err
	}
	if command.HardDeadlineAt, err = parse(deadlineAt); err != nil {
		return Command{}, err
	}
	if command.MaterialClaimedAt, err = parse(materialAt); err != nil {
		return Command{}, err
	}
	if command.EndedAt, err = parse(endedAt); err != nil {
		return Command{}, err
	}
	return command, nil
}

func parseRequired(value sql.NullString) (time.Time, error) {
	if !value.Valid || value.String == "" {
		return time.Time{}, fmt.Errorf("checkout command timestamp is missing")
	}
	return time.Parse(time.RFC3339Nano, value.String)
}

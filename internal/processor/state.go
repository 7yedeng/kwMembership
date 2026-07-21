package processor

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"

	"kwmembership/internal/store"
)

var terminalStates = map[string]bool{
	"ACCOUNT_ALREADY_SUBSCRIBED":  true,
	"PAYMENT_DECLINED":            true,
	"PARTIAL_FULFILLMENT_EXPIRED": true,
	"CANCELLED":                   true,
	"COMPLETED":                   true,
}

type Fulfillment struct {
	ID                string
	OrderID           string
	OrderNo           string
	TargetTier        string
	State             string
	CurrentStage      sql.NullString
	RunMode           sql.NullString
	AccountLockKey    sql.NullString
	StateRevision     int64
	RetryAt           sql.NullString
	MoneyBoundaryAt   sql.NullString
	CardReservationID sql.NullString
	CreatedAt         string
	UpdatedAt         string
}

const fulfillmentColumns = `id, order_id, order_no, target_tier, state, current_stage, run_mode,
  account_lock_key, state_revision, retry_at, money_boundary_at, card_reservation_id, created_at, updated_at`

type scanner interface{ Scan(...any) error }

func scanFulfillment(row scanner) (Fulfillment, error) {
	var item Fulfillment
	err := row.Scan(&item.ID, &item.OrderID, &item.OrderNo, &item.TargetTier, &item.State,
		&item.CurrentStage, &item.RunMode, &item.AccountLockKey, &item.StateRevision,
		&item.RetryAt, &item.MoneyBoundaryAt, &item.CardReservationID, &item.CreatedAt, &item.UpdatedAt)
	return item, err
}

func loadFulfillment(ctx context.Context, query store.Execer, id string) (Fulfillment, error) {
	item, err := scanFulfillment(query.QueryRowContext(ctx, `SELECT `+fulfillmentColumns+` FROM membership_fulfillments WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Fulfillment{}, coded("MEMBERSHIP_FULFILLMENT_NOT_FOUND", "membership fulfillment not found")
	}
	return item, err
}

type transitionOptions struct {
	CurrentStage     *string
	FailureCode      *string
	RetryAt          *string
	ExpectedRevision *int64
	Notify           bool
}

func transition(ctx context.Context, repository *store.Store, id, next string, now time.Time, options transitionOptions) (Fulfillment, error) {
	var updated Fulfillment
	err := repository.WithImmediate(ctx, func(tx *sql.Tx) error {
		var err error
		updated, err = transitionWithTx(ctx, tx, id, next, now, options)
		return err
	})
	return updated, err
}

func (p *Processor) transition(ctx context.Context, id, next string, now time.Time, options transitionOptions) (Fulfillment, error) {
	var updated Fulfillment
	err := p.withFencedImmediate(ctx, func(tx *sql.Tx) error {
		var err error
		updated, err = transitionWithTx(ctx, tx, id, next, now, options)
		return err
	})
	return updated, err
}

func transitionWithTx(ctx context.Context, tx *sql.Tx, id, next string, now time.Time, options transitionOptions) (Fulfillment, error) {
	current, err := loadFulfillment(ctx, tx, id)
	if err != nil {
		return Fulfillment{}, err
	}
	if terminalStates[current.State] && next != current.State {
		return current, nil
	}
	if options.ExpectedRevision != nil && current.StateRevision != *options.ExpectedRevision {
		return Fulfillment{}, coded("MEMBERSHIP_REVISION_CONFLICT", "membership revision changed")
	}
	stage := any(nil)
	if current.CurrentStage.Valid {
		stage = current.CurrentStage.String
	}
	if options.CurrentStage != nil {
		stage = *options.CurrentStage
	}
	failure := any(nil)
	if options.FailureCode != nil {
		failure = *options.FailureCode
	}
	retry := any(nil)
	if options.RetryAt != nil && *options.RetryAt != "" {
		retry = *options.RetryAt
	}
	at := store.ISO(now)
	completed := 0
	if terminalStates[next] {
		completed = 1
	}
	if _, err := tx.ExecContext(ctx, `
      UPDATE membership_fulfillments
      SET state = ?, current_stage = ?, failure_code = ?, retry_at = ?,
          state_revision = state_revision + 1, updated_at = ?,
          completed_at = CASE WHEN ? = 1 THEN COALESCE(completed_at, ?) ELSE completed_at END
      WHERE id = ?`, next, stage, failure, retry, at, completed, at, id); err != nil {
		return Fulfillment{}, err
	}
	updated, err := loadFulfillment(ctx, tx, id)
	if err != nil {
		return Fulfillment{}, err
	}
	if options.Notify || next == "BROWSER_LEASE_WAIT" {
		outboxID, err := store.NewID("mfo_")
		if err != nil {
			return Fulfillment{}, err
		}
		payload, _ := json.Marshal(map[string]string{"createdAt": at})
		if _, err := tx.ExecContext(ctx, `
        INSERT INTO membership_outbox (id, event_type, fulfillment_id, state_revision, payload, created_at)
        VALUES (?, 'membership.available', ?, ?, ?, ?)`, outboxID, updated.ID, updated.StateRevision, string(payload), at); err != nil {
			return Fulfillment{}, err
		}
	}
	return updated, nil
}

func deriveAccountLock(secret, accountID, email string) (string, error) {
	normalized := ""
	if accountID = strings.TrimSpace(accountID); accountID != "" {
		normalized = "account:" + accountID
	} else if email = strings.ToLower(strings.TrimSpace(email)); validEmail(email) {
		normalized = "email:" + email
	}
	if len(secret) < 12 || normalized == "" {
		return "", coded("MEMBERSHIP_IDENTITY_INVALID", "verified ChatGPT identity is missing")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte("kwredeem:membership-account-lock:v1\x00" + normalized))
	return "mfalk_v1_" + hex.EncodeToString(mac.Sum(nil)), nil
}

func validEmail(value string) bool {
	if len(value) == 0 || len(value) > 254 {
		return false
	}
	address, err := mail.ParseAddress(value)
	return err == nil && strings.EqualFold(address.Address, value) && strings.Contains(value, "@")
}

type processorError struct {
	code, message string
	cause         error
}

func (e *processorError) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("%s: %v", e.message, e.cause)
	}
	return e.message
}
func (e *processorError) Unwrap() error { return e.cause }
func (e *processorError) Code() string  { return e.code }

func coded(code, message string) error { return &processorError{code: code, message: message} }
func codedWrap(code, message string, cause error) error {
	return &processorError{code: code, message: message, cause: cause}
}

func errorCode(err error) string {
	var coded interface{ Code() string }
	if errors.As(err, &coded) {
		return coded.Code()
	}
	return "MEMBERSHIP_PROCESSOR_FAILED"
}

func pointer(value string) *string { return &value }

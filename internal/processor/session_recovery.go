package processor

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"kwmembership/internal/provider"
	"kwmembership/internal/store"
)

func isSessionFailureCode(code string) bool {
	switch code {
	case "SESSION_INVALID", "EXPECTED_IDENTITY_MISSING", "CONVERTER_IDENTITY_MISMATCH",
		"SESSION_COOKIE_MISSING", "CHATGPT_SESSION_UNAUTHORIZED", "CHATGPT_SESSION_IDENTITY_MISMATCH",
		"MEMBERSHIP_IDENTITY_INVALID":
		return true
	default:
		return false
	}
}

// handleSessionFailure is the sole release decision for invalid customer
// sessions. Once any money exposure exists, the original order and CDK remain
// locked and only the same-order recovery endpoint may replace the session.
func (p *Processor) handleSessionFailure(ctx context.Context, fulfillmentID, code, stage string, now time.Time) error {
	if !isSessionFailureCode(code) {
		return coded("SESSION_FAILURE_CODE_INVALID", "session failure code is invalid")
	}
	return p.withFencedImmediate(ctx, func(tx *sql.Tx) error {
		current, err := loadFulfillment(ctx, tx, fulfillmentID)
		if err != nil {
			return err
		}
		if current.State == "CANCELLED" || current.State == "COMPLETED" {
			return nil
		}

		exposed := current.MoneyBoundaryAt.Valid
		if !exposed && current.CardReservationID.Valid {
			var cardID sql.NullString
			err := tx.QueryRowContext(ctx, `SELECT card_id FROM card_capacity_reservations
        WHERE id=? AND fulfillment_id=? AND state IN ('reserved','consumed','retained_partial')`,
				current.CardReservationID.String, current.ID).Scan(&cardID)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			exposed = err == nil && cardID.Valid && cardID.String != ""
		}
		if !exposed {
			var evidenceCount int
			if err := tx.QueryRowContext(ctx, `SELECT
		    (SELECT COUNT(*) FROM membership_checkout_commands
		      WHERE fulfillment_id=? AND command_kind='payment' AND material_claimed_at IS NOT NULL)
		    + (SELECT COUNT(*) FROM funding_intents
		      WHERE fulfillment_id=? AND submitted_at IS NOT NULL)
		    + (SELECT COUNT(*) FROM membership_action_permits
		      WHERE fulfillment_id=? AND (activated_at IS NOT NULL OR state IN ('activated','challenge_locked','outcome_uncertain')))`,
				current.ID, current.ID, current.ID).Scan(&evidenceCount); err != nil {
				return err
			}
			exposed = evidenceCount > 0
		}
		if exposed {
			at := store.ISO(now)
			if _, err := tx.ExecContext(ctx, `UPDATE membership_checkout_commands
        SET state='cancelled',outcome_code='SESSION_RECOVERY_REQUIRED',ended_at=COALESCE(ended_at,?),updated_at=?
        WHERE fulfillment_id=? AND state IN ('queued','leased','action_required')`, at, at, current.ID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE membership_action_permits
        SET state='blocked',reported_at=COALESCE(reported_at,?),outcome_code='SESSION_RECOVERY_REQUIRED'
        WHERE fulfillment_id=? AND state='issued'`, at, current.ID); err != nil {
				return err
			}
			updated, err := transitionWithTx(ctx, tx, current.ID, "SESSION_RECOVERY_REQUIRED", now, transitionOptions{
				CurrentStage: pointer(stage), FailureCode: pointer(code),
			})
			if err != nil {
				return err
			}
			return insertCheckoutIntervention(ctx, tx, updated, "SESSION_RECOVERY_REQUIRED", now)
		}

		at := store.ISO(now)
		if _, err := tx.ExecContext(ctx, `UPDATE membership_checkout_commands
      SET state='cancelled',outcome_code='SESSION_INVALID_PRE_BOUNDARY',ended_at=COALESCE(ended_at,?),updated_at=?
      WHERE fulfillment_id=? AND state IN ('queued','leased','action_required')`, at, at, current.ID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE membership_action_permits
      SET state='blocked',reported_at=COALESCE(reported_at,?),outcome_code='SESSION_INVALID_PRE_BOUNDARY'
      WHERE fulfillment_id=? AND state='issued'`, at, current.ID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE card_capacity_reservations
      SET state='released',released_at=COALESCE(released_at,?),release_evidence_revision=?
      WHERE fulfillment_id=? AND state='reserved' AND card_id IS NULL`, at, current.StateRevision, current.ID); err != nil {
			return err
		}
		if err := releaseAutomaticQuotaBeforeBoundary(ctx, tx, current.ID, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE activation_jobs
      SET status='cancelled',last_error='SESSION_INVALID_PRE_BOUNDARY',locked_at=NULL,locked_by=NULL,updated_at=?
      WHERE order_id=? AND status IN ('pending','processing')`, at, current.OrderID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE redeem_orders
      SET status='failed',error_message='SESSION_INVALID',completed_at=COALESCE(completed_at,?),updated_at=?
      WHERE id=?`, at, at, current.OrderID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE cdkeys SET status='active',locked_at=NULL,
      locked_by_order_id=NULL,updated_at=? WHERE locked_by_order_id=? AND status='locked'`, at, current.OrderID); err != nil {
			return err
		}
		_, err = transitionWithTx(ctx, tx, current.ID, "CANCELLED", now, transitionOptions{
			CurrentStage: pointer(stage), FailureCode: pointer("SESSION_INVALID_PRE_BOUNDARY"),
		})
		return err
	})
}

func (p *Processor) rcReconcileRecoveredSession(ctx context.Context, fulfillment Fulfillment, now time.Time) error {
	key := p.rcStageKey(fulfillment)
	stage, err := p.rcLoadPaymentStage(ctx, p.store.DB(), fulfillment.ID, key, true)
	if err != nil {
		return err
	}
	if !stage.CardID.Valid || !stage.UpstreamCardID.Valid || !fulfillment.MoneyBoundaryAt.Valid {
		return coded("PAYMENT_STAGE_EVIDENCE_INCOMPLETE", "session recovery lacks immutable card evidence")
	}
	boundary, err := time.Parse(time.RFC3339Nano, fulfillment.MoneyBoundaryAt.String)
	if err != nil {
		return coded("PAYMENT_STAGE_EVIDENCE_INCOMPLETE", "session recovery money boundary is invalid")
	}
	session, err := p.rcLoadSession(ctx, fulfillment.ID)
	if err != nil {
		return err
	}
	client, err := p.cardPlatform(ctx, stage.ProviderKey.String)
	if err != nil {
		return err
	}
	events, externalErr := p.loadAllTransactions(ctx, client, stage.UpstreamCardID.Int64)
	if guardErr := p.assertWorkAllowed(ctx); guardErr != nil {
		return &rcWorkGuardError{cause: guardErr}
	}
	if externalErr != nil {
		return externalErr
	}
	observation, externalErr := p.rcFetchMembershipObservation(ctx, session, now)
	if guardErr := p.assertWorkAllowed(ctx); guardErr != nil {
		return &rcWorkGuardError{cause: guardErr}
	}
	if externalErr != nil {
		return externalErr
	}

	return p.withFencedImmediate(ctx, func(tx *sql.Tx) error {
		current, err := loadFulfillment(ctx, tx, fulfillment.ID)
		if err != nil || current.State != "SESSION_RECOVERY_RECONCILING" {
			return err
		}
		if _, err := p.rcPersistManagedTransactions(ctx, tx, stage.CardID.String, events, now); err != nil {
			return err
		}
		if _, err := p.rcInsertObservation(ctx, tx, current.ID, key, "session_recovery_reconciliation", observation, now); err != nil {
			return err
		}

		var unresolvedActions int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM membership_action_permits
		  WHERE fulfillment_id=? AND (
		    state IN ('activated','challenge_locked','outcome_uncertain')
		    OR (action_type='submit' AND activated_at IS NOT NULL)
		  )`, current.ID).
			Scan(&unresolvedActions); err != nil {
			return err
		}
		hasTransaction := false
		for _, event := range events {
			if sessionRecoveryTransactionAfterBoundary(event, boundary) {
				hasTransaction = true
				break
			}
		}
		if unresolvedActions > 0 || hasTransaction || !p.rcMembershipRemainsPreStage(observation, key) {
			updated, err := transitionWithTx(ctx, tx, current.ID, "SESSION_RECOVERY_EVIDENCE_HOLD", now, transitionOptions{
				CurrentStage: pointer(key), FailureCode: pointer("SESSION_RECOVERY_EVIDENCE_PRESENT"),
			})
			if err != nil {
				return err
			}
			return insertCheckoutIntervention(ctx, tx, updated, "SESSION_RECOVERY_EVIDENCE_PRESENT", now)
		}

		at := store.ISO(now)
		if _, err := tx.ExecContext(ctx, `UPDATE membership_fulfillment_attempts
      SET ended_at=COALESCE(ended_at,?),outcome_code=COALESCE(outcome_code,'SESSION_RECOVERED_NO_PAYMENT')
      WHERE fulfillment_id=? AND stage=? AND ended_at IS NULL`, at, current.ID, key); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE membership_payment_stages SET state='checkout_pending',
      attempt_no=NULL,page_fingerprint=NULL,page_permit_kind=NULL,page_control_id=NULL,page_ready_at=NULL,
      page_facts_json=NULL,progression_permitted_at=NULL,progression_reported_at=NULL,
      submit_permitted_at=NULL,submit_reported_at=NULL,updated_at=? WHERE id=?`, at, stage.ID); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE membership_fulfillments
      SET state='CHECKOUT_EXECUTION_WAIT',failure_code=NULL,retry_at=NULL,
          state_revision=state_revision+1,updated_at=? WHERE id=? AND state='SESSION_RECOVERY_RECONCILING'`, at, current.ID)
		return err
	})
}

func sessionRecoveryTransactionAfterBoundary(event provider.Transaction, boundary time.Time) bool {
	if !strings.EqualFold(strings.TrimSpace(event.MerchantNormalized), "OPENAI") {
		return false
	}
	value := strings.TrimSpace(event.AuthTime)
	if value == "" {
		value = strings.TrimSpace(event.CreatedAt)
	}
	observedAt, valid := sessionRecoveryProviderTime(value)
	return !valid || !observedAt.Before(boundary)
}

func sessionRecoveryProviderTime(value string) (time.Time, bool) {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05Z07:00", "2006-01-02 15:04:05"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			if layout == "2006-01-02 15:04:05" {
				parsed = time.Date(parsed.Year(), parsed.Month(), parsed.Day(), parsed.Hour(), parsed.Minute(), parsed.Second(), 0,
					time.FixedZone("UTC+8", 8*60*60))
			}
			return parsed.UTC(), true
		}
	}
	return time.Time{}, false
}

func releaseAutomaticQuotaBeforeBoundary(ctx context.Context, tx *sql.Tx, fulfillmentID string, now time.Time) error {
	var id, scopeID, date, state string
	var units int64
	var risk float64
	err := tx.QueryRowContext(ctx, `SELECT id,scope_id,business_date,order_units,risk_reserved_usd,state
    FROM automatic_checkout_quota_reservations WHERE fulfillment_id=?`, fulfillmentID).
		Scan(&id, &scopeID, &date, &units, &risk, &state)
	if errors.Is(err, sql.ErrNoRows) || state == "released" {
		return nil
	}
	if err != nil {
		return err
	}
	at := store.ISO(now)
	if _, err := tx.ExecContext(ctx, `UPDATE automatic_checkout_daily_usage
    SET order_units=MAX(0,order_units-?),risk_reserved_usd=MAX(0,risk_reserved_usd-?),updated_at=?
    WHERE scope_id=? AND business_date=?`, units, risk, at, scopeID, date); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE automatic_checkout_quota_reservations
    SET state='released',released_at=COALESCE(released_at,?) WHERE id=? AND state='reserved'`, at, id)
	return err
}

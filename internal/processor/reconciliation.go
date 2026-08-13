package processor

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"kwmembership/internal/domain"
	"kwmembership/internal/provider"
	"kwmembership/internal/store"
)

const (
	rcReconcileRetry    = 30 * time.Second
	rcFailureRetry      = 5 * time.Minute
	rcPartialRetry      = time.Hour
	rcPartialRenewalDue = 72 * time.Hour
)

type rcPaymentStage struct {
	ID                string
	FulfillmentID     string
	StageKey          string
	ExpectedTier      string
	AttemptNo         sql.NullInt64
	CardID            sql.NullString
	ProviderKey       sql.NullString
	PriceMin          sql.NullFloat64
	PriceMax          sql.NullFloat64
	SubmitPermittedAt sql.NullString
	MatchedAuthID     sql.NullString
	UpstreamCardID    sql.NullInt64
}

type rcAuthorizationSnapshot struct {
	PermitID string
	AuthIDs  []string
}

type rcTransitionOptions struct {
	CurrentStage *string
	FailureCode  *string
	RetryAt      *string
}

// rcWorkGuardError distinguishes an ownership/standby check from an ordinary
// provider failure. The runner must never project even a retry after it can no
// longer prove that this process owns the work.
type rcWorkGuardError struct{ cause error }

func (e *rcWorkGuardError) Error() string { return e.cause.Error() }
func (e *rcWorkGuardError) Unwrap() error { return e.cause }

// tickReconciliation owns every post-submit state. None of these states is
// eligible for the payment runner, which is the durable no-resubmit boundary.
func (p *Processor) tickReconciliation(ctx context.Context) (bool, error) {
	now := p.now().UTC()
	fulfillment, found, err := p.rcDueFulfillment(ctx, now)
	if err != nil || !found {
		return false, err
	}
	if err := p.rcProcessFulfillment(ctx, fulfillment, now); err != nil {
		var guardErr *rcWorkGuardError
		if errors.As(err, &guardErr) {
			return true, guardErr.cause
		}
		if persistErr := p.rcRecordFailure(ctx, fulfillment.ID, err, now); persistErr != nil {
			return true, persistErr
		}
		return true, err
	}
	return true, nil
}

func (p *Processor) rcDueFulfillment(ctx context.Context, now time.Time) (Fulfillment, bool, error) {
	fulfillment, err := scanFulfillment(p.store.DB().QueryRowContext(ctx, `SELECT `+fulfillmentColumns+`
    FROM membership_fulfillments f
    WHERE (
      (
        (
          f.state IN (
            'PLUS_SUBMIT_PERMITTED','PLUS_RECONCILING','UPGRADE_SUBMIT_PERMITTED',
            'UPGRADE_RECONCILING','PAYMENT_OUTCOME_UNCERTAIN','PAYMENT_ACTION_REQUIRED',
			'ACTION_REQUIRED_CONTEXT_LOST','UNEXPECTED_PREAUTH',
			'SESSION_RECOVERY_RECONCILING',
            'FINAL_TIER_CONFIRMED','RENEWAL_CANCELLING','PARTIALLY_FULFILLED'
          )
          OR (f.state='COMPLETED' AND EXISTS (
            SELECT 1 FROM membership_payment_stages stage
            WHERE stage.fulfillment_id=f.id AND stage.settlement_state='PENDING'
          ))
        )
        AND (f.retry_at IS NULL OR f.retry_at<=?)
        AND (f.state<>'ACTION_REQUIRED_CONTEXT_LOST' OR f.browser_lease_epoch IS NULL)
        AND (
          f.state<>'UNEXPECTED_PREAUTH' OR f.retry_at IS NOT NULL OR f.failure_code='UNEXPECTED_PREAUTH'
        )
      )
      OR (
        f.state IN ('UPGRADE_CHECKOUT_UNAVAILABLE','CHECKOUT_UI_UNSUPPORTED')
        AND (f.retry_at IS NULL OR f.retry_at<=?)
        AND (
          NOT EXISTS (
            SELECT 1 FROM fulfillment_interventions intervention
            WHERE intervention.fulfillment_id=f.id AND intervention.state=f.state
              AND intervention.state_revision=f.state_revision
          )
          OR (
            f.browser_lease_epoch IS NULL AND EXISTS (
              SELECT 1 FROM membership_payment_stages stage
              WHERE stage.fulfillment_id=f.id AND stage.settlement_state='PENDING'
            )
          )
        )
      )
    )
    ORDER BY f.updated_at,f.id LIMIT 1`, store.ISO(now), store.ISO(now)))
	if errors.Is(err, sql.ErrNoRows) {
		return Fulfillment{}, false, nil
	}
	return fulfillment, err == nil, err
}

func (p *Processor) rcProcessFulfillment(ctx context.Context, fulfillment Fulfillment, now time.Time) error {
	switch fulfillment.State {
	case "PLUS_SUBMIT_PERMITTED", "PLUS_RECONCILING", "UPGRADE_SUBMIT_PERMITTED",
		"UPGRADE_RECONCILING", "PAYMENT_OUTCOME_UNCERTAIN":
		return p.rcReconcilePayment(ctx, fulfillment, now)
	case "SESSION_RECOVERY_RECONCILING":
		return p.rcReconcileRecoveredSession(ctx, fulfillment, now)
	case "UNEXPECTED_PREAUTH":
		if err := p.rcEnsureIntervention(ctx, fulfillment, "UNEXPECTED_PREAUTH", now); err != nil {
			return err
		}
		return p.rcReconcileProgressionEvidence(ctx, fulfillment, true, now)
	case "ACTION_REQUIRED_CONTEXT_LOST":
		if err := p.rcEnsureIntervention(ctx, fulfillment, "ACTION_REQUIRED_CONTEXT_LOST", now); err != nil {
			return err
		}
		return p.rcReconcilePayment(ctx, fulfillment, now)
	case "PAYMENT_ACTION_REQUIRED":
		var id string
		err := p.store.DB().QueryRowContext(ctx, `SELECT id FROM fulfillment_interventions
      WHERE fulfillment_id=? AND state='PAYMENT_ACTION_REQUIRED' AND acknowledged_at IS NOT NULL
      ORDER BY state_revision DESC LIMIT 1`, fulfillment.ID).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		return p.rcReconcilePayment(ctx, fulfillment, now)
	case "FINAL_TIER_CONFIRMED", "RENEWAL_CANCELLING", "PARTIALLY_FULFILLED":
		return p.rcProtectRenewal(ctx, fulfillment, now)
	case "COMPLETED":
		return p.rcRefreshSettlement(ctx, fulfillment, now)
	case "UPGRADE_CHECKOUT_UNAVAILABLE", "CHECKOUT_UI_UNSUPPORTED":
		if err := p.rcEnsureIntervention(ctx, fulfillment, fulfillment.State, now); err != nil {
			return err
		}
		var browserLeaseEpoch sql.NullInt64
		if err := p.store.DB().QueryRowContext(ctx, `SELECT browser_lease_epoch
      FROM membership_fulfillments WHERE id=?`, fulfillment.ID).Scan(&browserLeaseEpoch); err != nil {
			return err
		}
		if browserLeaseEpoch.Valid {
			return nil
		}
		var pendingSettlements int
		if err := p.store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM membership_payment_stages
      WHERE fulfillment_id=? AND settlement_state='PENDING'`, fulfillment.ID).Scan(&pendingSettlements); err != nil {
			return err
		}
		if pendingSettlements == 0 {
			return nil
		}
		if err := p.rcRefreshSettlement(ctx, fulfillment, now); err != nil {
			return err
		}
		if guardErr := p.assertWorkAllowed(ctx); guardErr != nil {
			return &rcWorkGuardError{cause: guardErr}
		}
		_, err := p.fencedExec(ctx, `UPDATE membership_fulfillments
      SET retry_at=?,updated_at=?
      WHERE id=? AND state=? AND EXISTS (
        SELECT 1 FROM membership_payment_stages stage
        WHERE stage.fulfillment_id=membership_fulfillments.id AND stage.settlement_state='PENDING'
      )`, store.ISO(now.Add(rcReconcileRetry)), store.ISO(now), fulfillment.ID, fulfillment.State)
		return err
	default:
		return nil
	}
}

func (p *Processor) rcReconcilePayment(ctx context.Context, fulfillment Fulfillment, now time.Time) error {
	key := p.rcStageKey(fulfillment)
	stage, err := p.rcLoadPaymentStage(ctx, p.store.DB(), fulfillment.ID, key, true)
	if err != nil {
		return err
	}
	if !stage.CardID.Valid || stage.CardID.String == "" || !stage.UpstreamCardID.Valid || stage.UpstreamCardID.Int64 <= 0 ||
		!stage.PriceMin.Valid || !stage.PriceMax.Valid {
		return coded("PAYMENT_STAGE_EVIDENCE_INCOMPLETE", "payment stage evidence is incomplete")
	}
	// Check the immutable pre-submit boundary before doing provider work. A
	// missing snapshot can never be repaired by resubmitting checkout.
	if _, err := p.rcLoadAuthorizationSnapshot(ctx, p.store.DB(), fulfillment.ID, key, stage.AttemptNo); err != nil {
		return err
	}
	session, err := p.rcLoadSession(ctx, fulfillment.ID)
	if err != nil {
		return err
	}
	client, err := p.cardPlatform(ctx, stage.ProviderKey.String)
	if err != nil {
		return err
	}
	transactions, externalErr := p.loadAllTransactions(ctx, client, stage.UpstreamCardID.Int64)
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
	return p.rcApplyPaymentEvidence(ctx, fulfillment.ID, key, transactions, observation, now)
}

// rcReconcileProgressionEvidence only reads provider truth. It never grants
// another browser action, completes payment, or releases capacity.
func (p *Processor) rcReconcileProgressionEvidence(ctx context.Context, fulfillment Fulfillment, requireUnexpected bool, now time.Time) error {
	key := p.rcStageKey(fulfillment)
	stage, err := p.rcLoadPaymentStage(ctx, p.store.DB(), fulfillment.ID, key, true)
	if err != nil {
		return err
	}
	if !stage.CardID.Valid || stage.CardID.String == "" || !stage.UpstreamCardID.Valid || stage.UpstreamCardID.Int64 <= 0 ||
		!stage.PriceMin.Valid || !stage.PriceMax.Valid {
		return coded("PAYMENT_STAGE_EVIDENCE_INCOMPLETE", "preauthorization stage evidence is incomplete")
	}
	if _, err := p.rcLoadAuthorizationSnapshotForAction(ctx, p.store.DB(), fulfillment.ID, key, "progression", stage.AttemptNo, requireUnexpected); err != nil {
		return err
	}
	session, err := p.rcLoadSession(ctx, fulfillment.ID)
	if err != nil {
		return err
	}
	client, err := p.cardPlatform(ctx, stage.ProviderKey.String)
	if err != nil {
		return err
	}
	transactions, externalErr := p.loadAllTransactions(ctx, client, stage.UpstreamCardID.Int64)
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
	return p.rcApplyProgressionEvidence(ctx, fulfillment.ID, fulfillment.State, key, requireUnexpected, transactions, observation, now)
}

func (p *Processor) rcStageKey(fulfillment Fulfillment) string {
	if (fulfillment.CurrentStage.Valid && fulfillment.CurrentStage.String == "upgrade") ||
		strings.HasPrefix(fulfillment.State, "UPGRADE_") {
		return "upgrade"
	}
	return "plus"
}

func (p *Processor) rcLoadPaymentStage(ctx context.Context, query store.Execer, fulfillmentID, key string, includeCard bool) (rcPaymentStage, error) {
	if key != "plus" && key != "upgrade" {
		return rcPaymentStage{}, coded("PAYMENT_STAGE_EVIDENCE_INCOMPLETE", "payment stage key is invalid")
	}
	cardColumns, cardJoin := "NULL,NULL", ""
	if includeCard {
		hasProvider, err := tableHasColumn(ctx, query, "managed_cards", "provider_key")
		if err != nil {
			return rcPaymentStage{}, err
		}
		if hasProvider {
			cardColumns = "card.provider_key,card.upstream_card_id"
		} else {
			cardColumns = "'spacexcard',card.upstream_card_id"
		}
		cardJoin = "LEFT JOIN managed_cards card ON card.id=stage.card_id"
	}
	row := query.QueryRowContext(ctx, `SELECT stage.id,stage.fulfillment_id,stage.stage_key,stage.expected_tier,
      stage.attempt_no,stage.card_id,stage.price_signal_min,stage.price_signal_max,stage.submit_permitted_at,
	      stage.matched_auth_id,`+cardColumns+`
    FROM membership_payment_stages stage `+cardJoin+`
    WHERE stage.fulfillment_id=? AND stage.stage_key=?`, fulfillmentID, key)
	var stage rcPaymentStage
	err := row.Scan(&stage.ID, &stage.FulfillmentID, &stage.StageKey, &stage.ExpectedTier, &stage.AttemptNo,
		&stage.CardID, &stage.PriceMin, &stage.PriceMax, &stage.SubmitPermittedAt,
		&stage.MatchedAuthID, &stage.ProviderKey, &stage.UpstreamCardID)
	if errors.Is(err, sql.ErrNoRows) {
		return rcPaymentStage{}, coded("PAYMENT_STAGE_EVIDENCE_INCOMPLETE", "payment stage evidence is incomplete")
	}
	return stage, err
}

func (p *Processor) rcLoadAuthorizationSnapshot(ctx context.Context, query store.Execer, fulfillmentID, key string, attemptNo sql.NullInt64) (rcAuthorizationSnapshot, error) {
	return p.rcLoadAuthorizationSnapshotForAction(ctx, query, fulfillmentID, key, "submit", attemptNo, false)
}

func (p *Processor) rcLoadAuthorizationSnapshotForAction(ctx context.Context, query store.Execer, fulfillmentID, key, actionType string, attemptNo sql.NullInt64, requireUnexpected bool) (rcAuthorizationSnapshot, error) {
	if actionType != "submit" && actionType != "progression" {
		return rcAuthorizationSnapshot{}, coded("PAYMENT_STAGE_EVIDENCE_INCOMPLETE", "payment authorization action is invalid")
	}
	if !attemptNo.Valid || attemptNo.Int64 <= 0 {
		return rcAuthorizationSnapshot{}, coded("PAYMENT_STAGE_EVIDENCE_INCOMPLETE", "payment stage attempt is missing")
	}
	stateClause := ""
	if actionType == "progression" {
		stateClause = " AND state IN ('activated','outcome_uncertain','blocked')"
		if requireUnexpected {
			stateClause = " AND state='blocked' AND outcome_code='UNEXPECTED_PREAUTH'"
		}
	}
	var snapshot rcAuthorizationSnapshot
	err := query.QueryRowContext(ctx, `SELECT id FROM membership_action_permits
    WHERE fulfillment_id=? AND stage_key=? AND attempt_no=? AND action_type=?`+stateClause+`
    ORDER BY sequence_no DESC LIMIT 1`, fulfillmentID, key, attemptNo.Int64, actionType).Scan(&snapshot.PermitID)
	if errors.Is(err, sql.ErrNoRows) {
		code, message := "SUBMIT_AUTH_SNAPSHOT_MISSING", "pre-submit authorization snapshot is missing"
		if actionType == "progression" {
			code, message = "PROGRESSION_AUTH_SNAPSHOT_MISSING", "pre-progression authorization snapshot is missing"
		}
		return snapshot, coded(code, message)
	}
	if err != nil {
		return snapshot, err
	}
	rows, err := query.QueryContext(ctx, `SELECT auth_id FROM membership_action_auth_snapshots
    WHERE permit_id=? ORDER BY auth_id`, snapshot.PermitID)
	if err != nil {
		return snapshot, err
	}
	defer rows.Close()
	for rows.Next() {
		var authID string
		if err := rows.Scan(&authID); err != nil {
			return snapshot, err
		}
		snapshot.AuthIDs = append(snapshot.AuthIDs, authID)
	}
	return snapshot, rows.Err()
}

func (p *Processor) rcLoadSession(ctx context.Context, fulfillmentID string) (json.RawMessage, error) {
	var encrypted string
	err := p.store.DB().QueryRowContext(ctx, `SELECT o.session_payload
    FROM membership_fulfillments f JOIN redeem_orders o ON o.id=f.order_id
    WHERE f.id=?`, fulfillmentID).Scan(&encrypted)
	if err != nil {
		return nil, codedWrap("SESSION_INVALID", "load membership session", err)
	}
	plain, err := p.decrypter.Decrypt(encrypted)
	if err != nil {
		return nil, codedWrap("SESSION_INVALID", "decrypt membership session", err)
	}
	var session map[string]any
	if json.Unmarshal([]byte(plain), &session) != nil || session == nil {
		return nil, coded("SESSION_INVALID", "membership session is invalid")
	}
	compact, err := json.Marshal(session)
	if err != nil {
		return nil, codedWrap("SESSION_INVALID", "normalize membership session", err)
	}
	return json.RawMessage(compact), nil
}

func (p *Processor) rcFetchMembershipObservation(ctx context.Context, session json.RawMessage, now time.Time) (*domain.MembershipObservation, error) {
	raw, err := p.membership.Fetch(ctx, session)
	if err != nil {
		return nil, err
	}
	observation, err := domain.NormalizeMembershipEnvelope(raw, now)
	if err == nil {
		return observation, nil
	}
	code := domain.ErrorCode(err)
	if code == "" {
		code = "MEMBERSHIP_CONTRACT_UNKNOWN"
	}
	return nil, codedWrap(code, "normalize membership observation", err)
}

func (p *Processor) rcApplyPaymentEvidence(ctx context.Context, fulfillmentID, key string, events []provider.Transaction, observation *domain.MembershipObservation, now time.Time) error {
	if observation == nil {
		return coded("MEMBERSHIP_CONTRACT_UNKNOWN", "membership observation is missing")
	}
	return p.withFencedImmediate(ctx, func(tx *sql.Tx) error {
		fulfillment, err := loadFulfillment(ctx, tx, fulfillmentID)
		if err != nil {
			return err
		}
		if fulfillment.State == "CANCELLED" {
			return nil
		}
		stage, err := p.rcLoadPaymentStage(ctx, tx, fulfillmentID, key, false)
		if err != nil {
			return err
		}
		if !stage.CardID.Valid || stage.CardID.String == "" || !stage.PriceMin.Valid || !stage.PriceMax.Valid {
			return coded("PAYMENT_STAGE_EVIDENCE_INCOMPLETE", "payment stage evidence is incomplete")
		}
		minCents, minErr := domain.CentsFromUSD(stage.PriceMin.Float64)
		maxCents, maxErr := domain.CentsFromUSD(stage.PriceMax.Float64)
		if minErr != nil || maxErr != nil || minCents < 0 || maxCents < minCents {
			return coded("PAYMENT_STAGE_EVIDENCE_INCOMPLETE", "payment stage price evidence is invalid")
		}
		snapshot, err := p.rcLoadAuthorizationSnapshot(ctx, tx, fulfillmentID, key, stage.AttemptNo)
		if err != nil {
			return err
		}
		transactions, err := p.rcPersistManagedTransactions(ctx, tx, stage.CardID.String, events, now)
		if err != nil {
			return err
		}
		matched, err := domain.MatchPaymentTransactionDelta(domain.PaymentDeltaOptions{
			BeforeAuthIDs: snapshot.AuthIDs,
			Tier:          domain.Tier(stage.ExpectedTier),
			MinCents:      minCents,
			MaxCents:      maxCents,
			Transactions:  transactions,
		})
		if err != nil {
			return codedWrap("PAYMENT_STAGE_EVIDENCE_INCOMPLETE", "match payment transaction", err)
		}
		observationID, err := p.rcInsertObservation(ctx, tx, fulfillmentID, key,
			"payment_"+key+"_reconciliation", observation, now)
		if err != nil {
			return err
		}

		switch matched.Outcome {
		case domain.PaymentOutcomeMatched:
			return p.rcApplyMatchedPayment(ctx, tx, fulfillment, stage, matched.Transaction, observation, observationID, now)
		case domain.PaymentOutcomeDeclined:
			if p.rcMembershipRemainsPreStage(observation, key) {
				return p.rcApplyDeclinedPayment(ctx, tx, fulfillment, stage, matched.Transaction, observationID, now)
			}
		}

		reason := matched.Reason
		if reason == "" {
			reason = "PAYMENT_MEMBERSHIP_MISMATCH"
		}
		retryAt := store.ISO(now.Add(rcFailureRetry))
		updated, err := p.rcTransitionWithTx(ctx, tx, fulfillment, "PAYMENT_OUTCOME_UNCERTAIN", now,
			rcTransitionOptions{CurrentStage: pointer(key), FailureCode: pointer(reason), RetryAt: &retryAt})
		if err != nil {
			return err
		}
		if err := p.rcInsertIntervention(ctx, tx, updated, reason, now); err != nil {
			return err
		}
		if reason == "NO_MATCH" {
			return p.rcRecordDueNoPaymentChecks(ctx, tx, fulfillmentID, key, stage.SubmitPermittedAt,
				snapshot.AuthIDs, events, observation, now)
		}
		return nil
	})
}

func (p *Processor) rcApplyProgressionEvidence(ctx context.Context, fulfillmentID, holdingState, key string, requireUnexpected bool, events []provider.Transaction, observation *domain.MembershipObservation, now time.Time) error {
	if observation == nil {
		return coded("MEMBERSHIP_CONTRACT_UNKNOWN", "membership observation is missing")
	}
	return p.withFencedImmediate(ctx, func(tx *sql.Tx) error {
		fulfillment, err := loadFulfillment(ctx, tx, fulfillmentID)
		if err != nil {
			return err
		}
		if fulfillment.State == "CANCELLED" || fulfillment.State != holdingState {
			return nil
		}
		stage, err := p.rcLoadPaymentStage(ctx, tx, fulfillmentID, key, false)
		if err != nil {
			return err
		}
		if !stage.CardID.Valid || stage.CardID.String == "" || !stage.PriceMin.Valid || !stage.PriceMax.Valid {
			return coded("PAYMENT_STAGE_EVIDENCE_INCOMPLETE", "preauthorization stage evidence is incomplete")
		}
		minCents, minErr := domain.CentsFromUSD(stage.PriceMin.Float64)
		maxCents, maxErr := domain.CentsFromUSD(stage.PriceMax.Float64)
		if minErr != nil || maxErr != nil || minCents < 0 || maxCents < minCents {
			return coded("PAYMENT_STAGE_EVIDENCE_INCOMPLETE", "preauthorization stage price evidence is invalid")
		}
		snapshot, err := p.rcLoadAuthorizationSnapshotForAction(ctx, tx, fulfillmentID, key, "progression", stage.AttemptNo, requireUnexpected)
		if err != nil {
			return err
		}
		transactions, err := p.rcPersistManagedTransactions(ctx, tx, stage.CardID.String, events, now)
		if err != nil {
			return err
		}
		matched, err := domain.MatchPaymentTransactionDelta(domain.PaymentDeltaOptions{
			BeforeAuthIDs: snapshot.AuthIDs,
			Tier:          domain.Tier(stage.ExpectedTier),
			MinCents:      minCents,
			MaxCents:      maxCents,
			Transactions:  transactions,
		})
		if err != nil {
			return codedWrap("PAYMENT_STAGE_EVIDENCE_INCOMPLETE", "match preauthorization transaction", err)
		}
		if _, err := p.rcInsertObservation(ctx, tx, fulfillmentID, key,
			"unexpected_preauth_reconciliation", observation, now); err != nil {
			return err
		}

		expectedTier := domain.Tier(fulfillment.TargetTier)
		if key == "plus" {
			expectedTier = domain.TierPlus
		}
		pending, noPayment := rcClassifyProgressionTransactions(snapshot.AuthIDs, transactions)
		reason := "UNEXPECTED_PREAUTH_UNRESOLVED"
		retryAt := any(nil)
		if matched.Outcome == domain.PaymentOutcomeMatched &&
			domain.IsStrictMembershipStageConfirmed(observation, expectedTier, false) {
			reason = "UNEXPECTED_PREAUTH_STRICT_PAYMENT_EVIDENCE"
		} else if matched.Outcome == domain.PaymentOutcomeMatched {
			reason = "MEMBERSHIP_NOT_YET_CONFIRMED"
			retryAt = store.ISO(now.Add(rcReconcileRetry))
		} else if pending {
			reason = "UNEXPECTED_PREAUTH_PENDING"
			retryAt = store.ISO(now.Add(rcFailureRetry))
		} else if noPayment && p.rcMembershipRemainsPreStage(observation, key) {
			reason = "UNEXPECTED_PREAUTH_NO_PAYMENT_EVIDENCE"
		} else if !p.rcMembershipRemainsPreStage(observation, key) {
			reason = "UNEXPECTED_PREAUTH_MEMBERSHIP_MISMATCH"
		}
		if _, err := tx.ExecContext(ctx, `UPDATE membership_fulfillments
      SET failure_code=?,retry_at=?,updated_at=?
      WHERE id=? AND state=?`, reason, retryAt, store.ISO(now), fulfillmentID, holdingState); err != nil {
			return err
		}
		current, err := loadFulfillment(ctx, tx, fulfillmentID)
		if err != nil {
			return err
		}
		return p.rcInsertIntervention(ctx, tx, current, reason, now)
	})
}

func rcClassifyProgressionTransactions(beforeAuthIDs []string, transactions []domain.CardTransaction) (pending, noPayment bool) {
	before := make(map[string]struct{}, len(beforeAuthIDs))
	for _, authID := range beforeAuthIDs {
		before[authID] = struct{}{}
	}
	noPayment = true
	for _, transaction := range transactions {
		if _, existed := before[transaction.AuthID]; existed {
			continue
		}
		if transaction.Status == domain.TransactionStatusPending {
			pending, noPayment = true, false
			continue
		}
		if transaction.Status == domain.TransactionStatusDeclined ||
			(transaction.Type == domain.TransactionTypeReversal && transaction.Status == domain.TransactionStatusComplete) {
			continue
		}
		// Refunds, completed authorizations/settlements, and unknown terminal
		// shapes are contradictory evidence, not proof of no payment.
		noPayment = false
	}
	return pending, noPayment
}

func (p *Processor) rcApplyMatchedPayment(ctx context.Context, tx *sql.Tx, fulfillment Fulfillment, stage rcPaymentStage, matched *domain.CardTransaction, observation *domain.MembershipObservation, observationID string, now time.Time) error {
	if matched == nil {
		return coded("PAYMENT_MATCH_MISSING", "matched payment transaction is missing")
	}
	expectedTier := domain.Tier(fulfillment.TargetTier)
	if stage.StageKey == "plus" {
		expectedTier = domain.TierPlus
	}
	if !domain.IsStrictMembershipStageConfirmed(observation, expectedTier, false) {
		next := "UPGRADE_RECONCILING"
		if stage.StageKey == "plus" {
			next = "PLUS_RECONCILING"
		}
		retryAt := store.ISO(now.Add(rcReconcileRetry))
		_, err := p.rcTransitionWithTx(ctx, tx, fulfillment, next, now, rcTransitionOptions{
			CurrentStage: pointer(stage.StageKey), FailureCode: pointer("MEMBERSHIP_NOT_YET_CONFIRMED"), RetryAt: &retryAt,
		})
		return err
	}
	if stage.MatchedAuthID.Valid && stage.MatchedAuthID.String != "" && stage.MatchedAuthID.String != matched.AuthID {
		return coded("PAYMENT_MATCH_CONFLICT", "payment stage is already bound to another authorization")
	}
	at := store.ISO(now)
	if _, err := tx.ExecContext(ctx, `UPDATE membership_payment_stages
    SET state='confirmed',matched_auth_id=?,settlement_state=?,membership_observation_id=?,
        confirmed_at=COALESCE(confirmed_at,?),updated_at=? WHERE id=?`,
		matched.AuthID, matched.Status, observationID, at, at, stage.ID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE membership_action_permits
    SET state='reported',reported_at=COALESCE(reported_at,?),outcome_code='PAYMENT_CONFIRMED'
    WHERE fulfillment_id=? AND stage_key=? AND action_type='submit'
      AND state IN ('activated','challenge_locked','outcome_uncertain')`, at, fulfillment.ID, stage.StageKey); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE card_capacity_reservations
    SET state='consumed',consumed_at=COALESCE(consumed_at,?)
    WHERE fulfillment_id=? AND state='reserved'`, at, fulfillment.ID); err != nil {
		return err
	}
	next := "FINAL_TIER_CONFIRMED"
	if stage.StageKey == "plus" && fulfillment.TargetTier != string(domain.TierPlus) {
		next = "PLUS_CONFIRMED"
	}
	_, err := p.rcTransitionWithTx(ctx, tx, fulfillment, next, now,
		rcTransitionOptions{CurrentStage: pointer(stage.StageKey)})
	return err
}

func (p *Processor) rcApplyDeclinedPayment(ctx context.Context, tx *sql.Tx, fulfillment Fulfillment, stage rcPaymentStage, matched *domain.CardTransaction, observationID string, now time.Time) error {
	if matched == nil {
		return coded("PAYMENT_MATCH_MISSING", "declined payment transaction is missing")
	}
	at := store.ISO(now)
	if _, err := tx.ExecContext(ctx, `UPDATE membership_payment_stages
    SET state='declined',matched_auth_id=?,settlement_state='DECLINED',
        membership_observation_id=?,updated_at=? WHERE id=?`, matched.AuthID, observationID, at, stage.ID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE membership_action_permits
    SET state='reported',reported_at=COALESCE(reported_at,?),outcome_code='PAYMENT_DECLINED_CONFIRMED'
    WHERE fulfillment_id=? AND stage_key=? AND action_type='submit'
      AND state IN ('activated','challenge_locked','outcome_uncertain')`, at, fulfillment.ID, stage.StageKey); err != nil {
		return err
	}
	next := "PAYMENT_DECLINED"
	if stage.StageKey == "upgrade" {
		next = "PARTIALLY_FULFILLED"
		if _, err := tx.ExecContext(ctx, `UPDATE card_capacity_reservations SET state='retained_partial'
      WHERE fulfillment_id=? AND state IN ('reserved','consumed')`, fulfillment.ID); err != nil {
			return err
		}
	}
	updated, err := p.rcTransitionWithTx(ctx, tx, fulfillment, next, now, rcTransitionOptions{
		CurrentStage: pointer(stage.StageKey), FailureCode: pointer("PAYMENT_DECLINED"),
	})
	if err != nil {
		return err
	}
	if stage.StageKey == "plus" {
		if err := p.rcReleaseDeclinedPlusReservation(ctx, tx, updated, stage, now); err != nil {
			return err
		}
	}
	return p.rcInsertIntervention(ctx, tx, updated, "PAYMENT_DECLINED", now)
}

func (p *Processor) rcReleaseDeclinedPlusReservation(ctx context.Context, tx *sql.Tx, fulfillment Fulfillment, stage rcPaymentStage, now time.Time) error {
	reservation, found, err := paymentLoadReservationWith(ctx, tx, fulfillment.ID)
	if err != nil {
		return err
	}
	if !found {
		return coded("CARD_RESERVATION_NOT_FOUND", "declined Plus payment has no capacity reservation")
	}
	if reservation.State != "reserved" {
		return coded("RESERVATION_HAS_PAYMENT_EVIDENCE", "declined Plus capacity reservation is not releasable")
	}
	if !reservation.CardID.Valid || reservation.CardID.String == "" ||
		!stage.CardID.Valid || reservation.CardID.String != stage.CardID.String {
		return coded("PAYMENT_STAGE_EVIDENCE_INCOMPLETE", "declined Plus reservation card does not match its payment stage")
	}
	at := store.ISO(now)
	result, err := tx.ExecContext(ctx, `UPDATE card_capacity_reservations
    SET state='released',released_at=?,release_evidence_revision=?
    WHERE id=? AND state='reserved'`, at, fulfillment.StateRevision, reservation.ID)
	if err != nil {
		return err
	}
	if changed, err := result.RowsAffected(); err != nil {
		return err
	} else if changed != 1 {
		return coded("RESERVATION_RELEASE_CONFLICT", "declined Plus capacity reservation changed concurrently")
	}

	card, err := p.paymentLoadCard(ctx, tx, reservation.CardID.String)
	if err != nil {
		return err
	}
	var activeReservations, effectivePayments int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM card_capacity_reservations
    WHERE card_id=? AND state IN ('reserved','consumed','retained_partial')`, card.ID).Scan(&activeReservations); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM managed_card_transactions
    WHERE card_id=? AND UPPER(merchant_normalized)='OPENAI'
      AND status IN ('PENDING','COMPLETE')
      AND type NOT IN ('Refund','Reversal')`, card.ID).Scan(&effectivePayments); err != nil {
		return err
	}
	if card.Lane.Valid && card.Lane.String == reservation.TargetLane && card.ConsumedSlots == 0 &&
		activeReservations == 0 && effectivePayments == 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE managed_cards SET lane=NULL,updated_at=? WHERE id=?`, at, card.ID); err != nil {
			return err
		}
		card.Lane = sql.NullString{}
	}
	return paymentUpdateCapacityState(ctx, tx, card, domain.Tier(reservation.TargetLane), now)
}

func (p *Processor) rcMembershipRemainsPreStage(observation *domain.MembershipObservation, key string) bool {
	if observation == nil {
		return false
	}
	if key == "plus" {
		return observation.AccountType == domain.TierFree && !observation.IsOverdue && !observation.IsDelinquent
	}
	return domain.IsStrictMembershipStageConfirmed(observation, domain.TierPlus, false)
}

func (p *Processor) rcPersistManagedTransactions(ctx context.Context, tx *sql.Tx, cardID string, events []provider.Transaction, now time.Time) ([]domain.CardTransaction, error) {
	if cardID == "" || len(events) > 10_000 {
		return nil, coded("CARD_TRANSACTION_LIST_INVALID", "card transaction list is invalid")
	}
	for _, event := range events {
		if !rcSafeID(event.AuthID) {
			return nil, coded("SPACEXCARD_CONTRACT_DRIFT", "transaction authorization ID is invalid")
		}
	}
	collapsed := collapseProviderTransactions(events)
	at := store.ISO(now)
	result := make([]domain.CardTransaction, 0, len(collapsed))
	for _, item := range collapsed {
		transaction := item.Transaction
		var currentType, currentStatus string
		err := tx.QueryRowContext(ctx, `SELECT type,status FROM managed_card_transactions
      WHERE card_id=? AND auth_id=?`, cardID, transaction.AuthID).Scan(&currentType, &currentStatus)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if err == nil {
			canonical := domain.SelectCanonicalCardTransactionState(
				domain.TransactionState{Type: currentType, Status: currentStatus},
				domain.TransactionState{Type: transaction.Type, Status: transaction.Status},
			)
			transaction.Type, transaction.Status = canonical.Type, canonical.Status
		}
		var authAmount, settleAmount any = float64(0), float64(0)
		if transaction.AuthAmountCents != nil {
			authAmount = domain.USDFromCents(*transaction.AuthAmountCents)
		}
		if transaction.SettleAmountCents != nil {
			settleAmount = domain.USDFromCents(*transaction.SettleAmountCents)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO managed_card_transactions
      (card_id,auth_id,auth_time,auth_amount,auth_currency,settle_amount,settle_currency,
       type,status,merchant_normalized,authorization_seen,settlement_seen,refund_seen,
       reversal_seen,first_seen_at,last_seen_at)
      VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
      ON CONFLICT(card_id,auth_id) DO UPDATE SET
        auth_time=COALESCE(excluded.auth_time,managed_card_transactions.auth_time),
        auth_amount=CASE WHEN excluded.auth_amount>0 THEN excluded.auth_amount ELSE managed_card_transactions.auth_amount END,
        auth_currency=COALESCE(excluded.auth_currency,managed_card_transactions.auth_currency),
        settle_amount=CASE WHEN excluded.settle_amount>0 THEN excluded.settle_amount ELSE managed_card_transactions.settle_amount END,
        settle_currency=COALESCE(excluded.settle_currency,managed_card_transactions.settle_currency),
        type=excluded.type,status=excluded.status,
        merchant_normalized=CASE WHEN excluded.merchant_normalized='OPENAI' THEN 'OPENAI' ELSE managed_card_transactions.merchant_normalized END,
        authorization_seen=MAX(managed_card_transactions.authorization_seen,excluded.authorization_seen),
        settlement_seen=MAX(managed_card_transactions.settlement_seen,excluded.settlement_seen),
        refund_seen=MAX(managed_card_transactions.refund_seen,excluded.refund_seen),
        reversal_seen=MAX(managed_card_transactions.reversal_seen,excluded.reversal_seen),
        last_seen_at=excluded.last_seen_at`, cardID, transaction.AuthID, nullString(transaction.AuthTime),
			authAmount, nullString(transaction.AuthCurrency), settleAmount, nullString(transaction.SettleCurrency),
			transaction.Type, transaction.Status, transaction.MerchantNormalized, item.AuthorizationSeen,
			item.SettlementSeen, item.RefundSeen, item.ReversalSeen, at, at); err != nil {
			return nil, err
		}
		result = append(result, transaction)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE managed_cards SET last_transaction_sync_at=?,updated_at=? WHERE id=?`, at, at, cardID); err != nil {
		return nil, err
	}
	return result, nil
}

func rcSafeID(value string) bool {
	value = strings.TrimSpace(value)
	if len(value) == 0 || len(value) > 256 {
		return false
	}
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || strings.ContainsRune("._:-", char) {
			continue
		}
		return false
	}
	return true
}

func (p *Processor) rcInsertObservation(ctx context.Context, tx *sql.Tx, fulfillmentID, stage, purpose string, observation *domain.MembershipObservation, now time.Time) (string, error) {
	id, err := store.NewID("mfo_")
	if err != nil {
		return "", err
	}
	autoRenew := any(nil)
	if observation.AutoRenew != nil {
		autoRenew = boolInt(*observation.AutoRenew)
	}
	expireTime := any(nil)
	if observation.ExpireTime != nil {
		expireTime = store.ISO(*observation.ExpireTime)
	}
	observedAt := observation.ObservedAt
	if observedAt.IsZero() {
		observedAt = now
	}
	stageValue := any(nil)
	if stage != "" {
		stageValue = stage
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO membership_observations
    (id,fulfillment_id,stage_key,purpose,provider_code,account_type,currency,auto_renew,
     is_overdue,is_delinquent,expire_time,observed_at)
    VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`, id, fulfillmentID, stageValue, purpose,
		observation.ProviderCode, string(observation.AccountType), nullString(observation.Currency), autoRenew,
		boolInt(observation.IsOverdue), boolInt(observation.IsDelinquent), expireTime, store.ISO(observedAt))
	return id, err
}

func (p *Processor) rcTransitionWithTx(ctx context.Context, tx *sql.Tx, fulfillment Fulfillment, next string, now time.Time, options rcTransitionOptions) (Fulfillment, error) {
	current, err := loadFulfillment(ctx, tx, fulfillment.ID)
	if err != nil {
		return Fulfillment{}, err
	}
	if current.State == "CANCELLED" && next != "CANCELLED" {
		return current, nil
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
	retryAt := any(nil)
	if options.RetryAt != nil && *options.RetryAt != "" {
		retryAt = *options.RetryAt
	}
	at := store.ISO(now)
	completed := 0
	if terminalStates[next] {
		completed = 1
	}
	if _, err := tx.ExecContext(ctx, `UPDATE membership_fulfillments
    SET state=?,current_stage=?,failure_code=?,retry_at=?,state_revision=state_revision+1,
        updated_at=?,completed_at=CASE WHEN ?=1 THEN COALESCE(completed_at,?) ELSE completed_at END
    WHERE id=?`, next, stage, failure, retryAt, at, completed, at, current.ID); err != nil {
		return Fulfillment{}, err
	}
	return loadFulfillment(ctx, tx, current.ID)
}

func (p *Processor) rcInsertIntervention(ctx context.Context, tx *sql.Tx, fulfillment Fulfillment, reason string, now time.Time) error {
	id, err := store.NewID("fi_")
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO fulfillment_interventions
    (id,fulfillment_id,state,state_revision,reason_code,created_at) VALUES (?,?,?,?,?,?)`,
		id, fulfillment.ID, fulfillment.State, fulfillment.StateRevision, reason, store.ISO(now))
	return err
}

func (p *Processor) rcEnsureIntervention(ctx context.Context, fulfillment Fulfillment, fallbackReason string, now time.Time) error {
	return p.withFencedImmediate(ctx, func(tx *sql.Tx) error {
		current, err := loadFulfillment(ctx, tx, fulfillment.ID)
		if err != nil {
			return err
		}
		if current.State != fulfillment.State || current.StateRevision != fulfillment.StateRevision {
			return nil
		}
		reason := fallbackReason
		var failure sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT failure_code FROM membership_fulfillments WHERE id=?`, current.ID).Scan(&failure); err != nil {
			return err
		}
		if failure.Valid && strings.TrimSpace(failure.String) != "" {
			reason = failure.String
		}
		return p.rcInsertIntervention(ctx, tx, current, reason, now)
	})
}

func (p *Processor) rcRecordDueNoPaymentChecks(ctx context.Context, tx *sql.Tx, fulfillmentID, key string,
	submitAt sql.NullString, beforeAuthIDs []string, transactions []provider.Transaction,
	observation *domain.MembershipObservation, now time.Time,
) error {
	if !submitAt.Valid {
		return nil
	}
	boundary, err := time.Parse(time.RFC3339Nano, submitAt.String)
	if err != nil || now.Sub(boundary) < 5*time.Minute {
		return nil
	}
	before := make(map[string]bool, len(beforeAuthIDs))
	for _, authID := range beforeAuthIDs {
		before[authID] = true
	}
	effective, pending := false, false
	for _, transaction := range transactions {
		if before[transaction.AuthID] {
			continue
		}
		if strings.EqualFold(transaction.MerchantNormalized, "OPENAI") &&
			(transaction.Status == domain.TransactionStatusPending || transaction.Status == domain.TransactionStatusComplete) &&
			transaction.Type != domain.TransactionTypeRefund && transaction.Type != domain.TransactionTypeReversal {
			effective = true
		}
		if transaction.Status == domain.TransactionStatusPending && transaction.Type == domain.TransactionTypeAuthorization {
			pending = true
		}
	}
	membershipUnchanged := p.rcMembershipRemainsPreStage(observation, key)
	checks := []struct {
		name      string
		threshold time.Duration
	}{{"5m", 5 * time.Minute}, {"1h", time.Hour}, {"24h", 24 * time.Hour}}
	for _, check := range checks {
		if now.Sub(boundary) < check.threshold {
			continue
		}
		var existing string
		err := tx.QueryRowContext(ctx, `SELECT id FROM membership_no_payment_checks
      WHERE fulfillment_id=? AND stage_key=? AND checkpoint=?`, fulfillmentID, key, check.name).Scan(&existing)
		if err == nil {
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		id, err := store.NewID("mnpc_")
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO membership_no_payment_checks
      (id,fulfillment_id,stage_key,checkpoint,membership_unchanged,no_effective_transaction,
       no_pending_authorization,observed_at) VALUES (?,?,?,?,?,?,?,?)`, id, fulfillmentID, key,
			check.name, boolInt(membershipUnchanged), boolInt(!effective), boolInt(!pending), store.ISO(now)); err != nil {
			return err
		}
	}
	return nil
}

func (p *Processor) rcProtectRenewal(ctx context.Context, fulfillment Fulfillment, now time.Time) error {
	session, err := p.rcLoadSession(ctx, fulfillment.ID)
	if err != nil {
		return err
	}
	observation, externalErr := p.rcFetchMembershipObservation(ctx, session, now)
	if guardErr := p.assertWorkAllowed(ctx); guardErr != nil {
		return &rcWorkGuardError{cause: guardErr}
	}
	if externalErr != nil {
		return externalErr
	}
	partial := fulfillment.State == "PARTIALLY_FULFILLED"
	shouldCancel := observation.AutoRenew != nil && *observation.AutoRenew &&
		(!partial || p.rcShouldCancelPartialRenewal(observation, now))
	if shouldCancel {
		token, err := p.renewalToken(ctx)
		if err != nil {
			return err
		}
		if guardErr := p.assertWorkAllowed(ctx); guardErr != nil {
			return &rcWorkGuardError{cause: guardErr}
		}
		cancelErr := p.renewal.Cancel(ctx, session, token)
		if guardErr := p.assertWorkAllowed(ctx); guardErr != nil {
			return &rcWorkGuardError{cause: guardErr}
		}
		if cancelErr != nil {
			return cancelErr
		}
		// The cancellation response is not membership truth. Completion always
		// requires a subsequent authoritative auto_renew=false observation.
		observation, externalErr = p.rcFetchMembershipObservation(ctx, session, now)
		if guardErr := p.assertWorkAllowed(ctx); guardErr != nil {
			return &rcWorkGuardError{cause: guardErr}
		}
		if externalErr != nil {
			return externalErr
		}
	}
	return p.rcApplyRenewalObservation(ctx, fulfillment.ID, observation, now)
}

func (p *Processor) rcShouldCancelPartialRenewal(observation *domain.MembershipObservation, now time.Time) bool {
	return observation != nil && observation.AccountType == domain.TierPlus &&
		observation.AutoRenew != nil && *observation.AutoRenew && observation.ExpireTime != nil &&
		observation.ExpireTime.Sub(now) <= rcPartialRenewalDue
}

func (p *Processor) rcApplyRenewalObservation(ctx context.Context, fulfillmentID string, observation *domain.MembershipObservation, now time.Time) error {
	if observation == nil {
		return coded("MEMBERSHIP_CONTRACT_UNKNOWN", "membership observation is missing")
	}
	return p.withFencedImmediate(ctx, func(tx *sql.Tx) error {
		fulfillment, err := loadFulfillment(ctx, tx, fulfillmentID)
		if err != nil {
			return err
		}
		if fulfillment.State == "CANCELLED" {
			return nil
		}
		partial := fulfillment.State == "PARTIALLY_FULFILLED"
		expectedTier := domain.Tier(fulfillment.TargetTier)
		stageKey, purpose := "upgrade", "final_renewal_guard"
		if partial {
			expectedTier, stageKey, purpose = domain.TierPlus, "plus", "partial_renewal_guard"
		} else if expectedTier == domain.TierPlus {
			stageKey = "plus"
		}
		observationID, err := p.rcInsertObservation(ctx, tx, fulfillmentID, stageKey, purpose, observation, now)
		if err != nil {
			return err
		}
		if !domain.IsStrictMembershipStageConfirmed(observation, expectedTier, false) {
			if partial && (observation.AccountType == domain.TierFree ||
				(observation.AccountType == domain.TierPlus && !observation.ExpireTimeFuture)) {
				updated, err := p.rcTransitionWithTx(ctx, tx, fulfillment, "PARTIAL_FULFILLMENT_EXPIRED", now,
					rcTransitionOptions{CurrentStage: pointer("renewal"), FailureCode: pointer("PARTIAL_MEMBERSHIP_EXPIRED")})
				if err != nil {
					return err
				}
				return p.rcInsertIntervention(ctx, tx, updated, "PARTIAL_MEMBERSHIP_EXPIRED", now)
			}
			retryAt := store.ISO(now.Add(rcReconcileRetry))
			updated, err := p.rcTransitionWithTx(ctx, tx, fulfillment, "PAYMENT_OUTCOME_UNCERTAIN", now,
				rcTransitionOptions{CurrentStage: pointer("renewal"), FailureCode: pointer("FINAL_MEMBERSHIP_CHANGED"), RetryAt: &retryAt})
			if err != nil {
				return err
			}
			return p.rcInsertIntervention(ctx, tx, updated, "FINAL_MEMBERSHIP_CHANGED", now)
		}
		if observation.AutoRenew == nil || *observation.AutoRenew {
			if partial {
				retryAt := store.ISO(now.Add(rcPartialRetry))
				_, err := p.rcTransitionWithTx(ctx, tx, fulfillment, "PARTIALLY_FULFILLED", now,
					rcTransitionOptions{CurrentStage: pointer("renewal"), FailureCode: pointer("PARTIAL_RENEWAL_STILL_ENABLED"), RetryAt: &retryAt})
				return err
			}
			retryAt := store.ISO(now.Add(rcReconcileRetry))
			_, err := p.rcTransitionWithTx(ctx, tx, fulfillment, "RENEWAL_CANCELLING", now,
				rcTransitionOptions{CurrentStage: pointer("renewal"), FailureCode: pointer("RENEWAL_STILL_ENABLED"), RetryAt: &retryAt})
			return err
		}
		if partial {
			retryAt := store.ISO(now.Add(rcPartialRetry))
			if observation.ExpireTime != nil {
				retryAt = store.ISO(*observation.ExpireTime)
			}
			_, err := p.rcTransitionWithTx(ctx, tx, fulfillment, "PARTIALLY_FULFILLED", now,
				rcTransitionOptions{CurrentStage: pointer("renewal"), FailureCode: pointer("UPGRADE_NOT_COMPLETED"), RetryAt: &retryAt})
			return err
		}
		completed, err := p.rcTransitionWithTx(ctx, tx, fulfillment, "COMPLETED", now,
			rcTransitionOptions{CurrentStage: pointer("renewal")})
		if err != nil {
			return err
		}
		finalKey := "upgrade"
		if fulfillment.TargetTier == string(domain.TierPlus) {
			finalKey = "plus"
		}
		at := store.ISO(now)
		if _, err := tx.ExecContext(ctx, `UPDATE membership_payment_stages
      SET membership_observation_id=?,updated_at=?
      WHERE fulfillment_id=? AND stage_key=? AND state='confirmed'`, observationID, at, fulfillmentID, finalKey); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE membership_fulfillment_attempts
      SET ended_at=COALESCE(ended_at,?),outcome_code=COALESCE(outcome_code,'COMPLETED')
      WHERE fulfillment_id=? AND ended_at IS NULL`, at, completed.ID)
		return err
	})
}

func (p *Processor) rcRefreshSettlement(ctx context.Context, fulfillment Fulfillment, now time.Time) error {
	rows, err := p.store.DB().QueryContext(ctx, `SELECT stage.stage_key,stage.card_id,card.provider_key,card.upstream_card_id
    FROM membership_payment_stages stage
    LEFT JOIN managed_cards card ON card.id=stage.card_id
    WHERE stage.fulfillment_id=? AND stage.settlement_state='PENDING'
    ORDER BY stage.stage_key`, fulfillment.ID)
	if err != nil {
		return err
	}
	type pendingStage struct {
		key         string
		cardID      sql.NullString
		providerKey sql.NullString
		upstreamID  sql.NullInt64
	}
	var stages []pendingStage
	for rows.Next() {
		var stage pendingStage
		if err := rows.Scan(&stage.key, &stage.cardID, &stage.providerKey, &stage.upstreamID); err != nil {
			rows.Close()
			return err
		}
		stages = append(stages, stage)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, stage := range stages {
		if !stage.cardID.Valid || !stage.upstreamID.Valid || stage.upstreamID.Int64 <= 0 {
			return coded("MANAGED_CARD_NOT_FOUND", "managed card is missing")
		}
		client, err := p.cardPlatform(ctx, stage.providerKey.String)
		if err != nil {
			return err
		}
		transactions, externalErr := p.loadAllTransactions(ctx, client, stage.upstreamID.Int64)
		if guardErr := p.assertWorkAllowed(ctx); guardErr != nil {
			return &rcWorkGuardError{cause: guardErr}
		}
		if externalErr != nil {
			return externalErr
		}
		reviewed, err := p.rcApplySettlementEvidence(ctx, fulfillment.ID, stage.key, transactions, now)
		if err != nil {
			return err
		}
		if reviewed {
			return nil
		}
	}
	return nil
}

func (p *Processor) rcApplySettlementEvidence(ctx context.Context, fulfillmentID, key string, events []provider.Transaction, now time.Time) (bool, error) {
	reviewed := false
	err := p.withFencedImmediate(ctx, func(tx *sql.Tx) error {
		stage, err := p.rcLoadPaymentStage(ctx, tx, fulfillmentID, key, false)
		if err != nil {
			return err
		}
		if !stage.CardID.Valid || stage.CardID.String == "" || !stage.MatchedAuthID.Valid || stage.MatchedAuthID.String == "" {
			return coded("PAYMENT_STAGE_EVIDENCE_INCOMPLETE", "settlement payment evidence is incomplete")
		}
		if _, err := p.rcPersistManagedTransactions(ctx, tx, stage.CardID.String, events, now); err != nil {
			return err
		}
		var status string
		var refundSeen, reversalSeen int
		err = tx.QueryRowContext(ctx, `SELECT status,refund_seen,reversal_seen
      FROM managed_card_transactions WHERE card_id=? AND auth_id=?`,
			stage.CardID.String, stage.MatchedAuthID.String).Scan(&status, &refundSeen, &reversalSeen)
		if errors.Is(err, sql.ErrNoRows) {
			return coded("PAYMENT_MATCH_MISSING", "matched transaction is missing")
		}
		if err != nil {
			return err
		}
		if status == domain.TransactionStatusComplete && refundSeen == 0 && reversalSeen == 0 {
			_, err = tx.ExecContext(ctx, `UPDATE membership_payment_stages
        SET settlement_state='COMPLETE',updated_at=? WHERE id=?`, store.ISO(now), stage.ID)
			return err
		}
		if status == domain.TransactionStatusPending && refundSeen == 0 && reversalSeen == 0 {
			return nil
		}
		current, err := loadFulfillment(ctx, tx, fulfillmentID)
		if err != nil {
			return err
		}
		updated, err := p.rcTransitionWithTx(ctx, tx, current, "PAYMENT_OUTCOME_UNCERTAIN", now,
			rcTransitionOptions{CurrentStage: pointer(key), FailureCode: pointer("POST_COMPLETION_TRANSACTION_CHANGED")})
		if err != nil {
			return err
		}
		if err := p.rcInsertIntervention(ctx, tx, updated, "POST_COMPLETION_TRANSACTION_CHANGED", now); err != nil {
			return err
		}
		reviewed = true
		return nil
	})
	return reviewed, err
}

func (p *Processor) rcRecordFailure(ctx context.Context, fulfillmentID string, cause error, now time.Time) error {
	code := errorCode(cause)
	if len(code) > 100 {
		code = code[:100]
	}
	if isSessionFailureCode(code) {
		return p.handleSessionFailure(ctx, fulfillmentID, code, "reconciliation", now)
	}
	return p.withFencedImmediate(ctx, func(tx *sql.Tx) error {
		at := store.ISO(now)
		if _, err := tx.ExecContext(ctx, `UPDATE membership_fulfillments
      SET failure_code=?,retry_at=?,updated_at=? WHERE id=?`, code,
			store.ISO(now.Add(rcFailureRetry)), at, fulfillmentID); err != nil {
			return err
		}
		if !strings.HasPrefix(code, "RENEWAL_CANCEL_") {
			return nil
		}
		current, err := loadFulfillment(ctx, tx, fulfillmentID)
		if err != nil {
			return err
		}
		return p.rcInsertIntervention(ctx, tx, current, code, now)
	})
}

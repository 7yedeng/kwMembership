package processor

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"kwmembership/internal/domain"
	"kwmembership/internal/store"
)

const (
	sharedRetry  = 5 * time.Minute
	accountRetry = time.Hour
)

func (p *Processor) tickEligibility(ctx context.Context) (bool, error) {
	now := p.now().UTC()
	if _, err := p.catchUpSessionActivation(ctx, 20, now); err != nil {
		return false, err
	}
	if err := p.promoteOneWaiter(ctx, now); err != nil {
		return false, err
	}
	fulfillment, found, err := p.claimEligibility(ctx, now)
	if err != nil || !found {
		return false, err
	}
	return true, p.processEligibility(ctx, fulfillment, now)
}

func (p *Processor) catchUpSessionActivation(ctx context.Context, limit int, now time.Time) (int, error) {
	rows, err := p.store.DB().QueryContext(ctx, `
	    SELECT f.id, f.order_no, f.state_revision, o.session_payload
    FROM membership_fulfillments f
    JOIN redeem_orders o ON o.id = f.order_id
    WHERE f.state IN ('WAITING_SESSION_VALIDATION','WAITING_SESSION_ACTIVATION') AND o.session_payload IS NOT NULL
    ORDER BY f.created_at LIMIT ?`, limit)
	if err != nil {
		return 0, err
	}
	type candidate struct {
		id, orderNo, encrypted string
		revision               int64
	}
	var candidates []candidate
	for rows.Next() {
		var item candidate
		if err := rows.Scan(&item.id, &item.orderNo, &item.revision, &item.encrypted); err != nil {
			rows.Close()
			return 0, err
		}
		candidates = append(candidates, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	changed := 0
	for _, item := range candidates {
		plain, err := p.decrypter.Decrypt(item.encrypted)
		if err != nil {
			if err := p.handleSessionFailure(ctx, item.id, "SESSION_INVALID", "eligibility", now); err != nil {
				return changed, err
			}
			changed++
			continue
		}
		var session map[string]any
		if json.Unmarshal([]byte(plain), &session) != nil {
			if err := p.handleSessionFailure(ctx, item.id, "SESSION_INVALID", "eligibility", now); err != nil {
				return changed, err
			}
			changed++
			continue
		}
		email := sessionEmail(session)
		lock, err := deriveAccountLock(p.config.EncryptionKey, "", email)
		if err != nil {
			if err := p.handleSessionFailure(ctx, item.id, "MEMBERSHIP_IDENTITY_INVALID", "eligibility", now); err != nil {
				return changed, err
			}
			changed++
			continue
		}
		activated, err := p.activateIdentity(ctx, item.orderNo, lock, now)
		if err != nil {
			return changed, err
		}
		if activated {
			changed++
		}
	}
	return changed, nil
}

func sessionEmail(session map[string]any) string {
	if user, ok := session["user"].(map[string]any); ok {
		if value, ok := user["email"].(string); ok {
			return value
		}
	}
	if value, ok := session["email"].(string); ok {
		return value
	}
	return ""
}

func (p *Processor) activateIdentity(ctx context.Context, orderNo, lock string, now time.Time) (bool, error) {
	changed := false
	err := p.withFencedImmediate(ctx, func(tx *sql.Tx) error {
		var id, state string
		err := tx.QueryRowContext(ctx, `
      SELECT id, state FROM membership_fulfillments
      WHERE order_no = ?`, orderNo).Scan(&id, &state)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if state != "WAITING_SESSION_VALIDATION" && state != "WAITING_SESSION_ACTIVATION" && state != "ACCOUNT_FULFILLMENT_WAIT" {
			return nil
		}
		var holder string
		err = tx.QueryRowContext(ctx, `
      SELECT id FROM membership_fulfillments
      WHERE account_lock_key = ? AND id <> ? AND state <> 'ACCOUNT_FULFILLMENT_WAIT'
        AND state NOT IN ('ACCOUNT_ALREADY_SUBSCRIBED','PAYMENT_DECLINED','PARTIAL_FULFILLMENT_EXPIRED','CANCELLED','COMPLETED')
      LIMIT 1`, lock, id).Scan(&holder)
		next := "QUEUED"
		if err == nil {
			next = "ACCOUNT_FULFILLMENT_WAIT"
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		result, err := tx.ExecContext(ctx, `
      UPDATE membership_fulfillments
      SET account_lock_key = ?, state = ?, failure_code = NULL, retry_at = NULL,
          state_revision = state_revision + 1, updated_at = ?
      WHERE id = ? AND state = ?`, lock, next, store.ISO(now), id, state)
		if err != nil {
			return err
		}
		count, _ := result.RowsAffected()
		changed = count == 1
		return nil
	})
	return changed, err
}

func (p *Processor) promoteOneWaiter(ctx context.Context, now time.Time) error {
	return p.withFencedImmediate(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
      SELECT id, account_lock_key FROM membership_fulfillments
      WHERE state = 'ACCOUNT_FULFILLMENT_WAIT' AND account_lock_key IS NOT NULL
        AND (retry_at IS NULL OR retry_at <= ?)
      ORDER BY created_at`, store.ISO(now))
		if err != nil {
			return err
		}
		type waiter struct{ id, lock string }
		var waiters []waiter
		for rows.Next() {
			var item waiter
			if err := rows.Scan(&item.id, &item.lock); err != nil {
				rows.Close()
				return err
			}
			waiters = append(waiters, item)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, item := range waiters {
			var holder string
			err := tx.QueryRowContext(ctx, `
        SELECT id FROM membership_fulfillments
        WHERE account_lock_key = ? AND id <> ? AND state <> 'ACCOUNT_FULFILLMENT_WAIT'
          AND state NOT IN ('ACCOUNT_ALREADY_SUBSCRIBED','PAYMENT_DECLINED','PARTIAL_FULFILLMENT_EXPIRED','CANCELLED','COMPLETED') LIMIT 1`, item.lock, item.id).Scan(&holder)
			if err == nil {
				continue
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			_, err = tx.ExecContext(ctx, `UPDATE membership_fulfillments
          SET state='QUEUED', failure_code=NULL, retry_at=NULL, state_revision=state_revision+1, updated_at=?
          WHERE id=? AND state='ACCOUNT_FULFILLMENT_WAIT'`, store.ISO(now), item.id)
			return err
		}
		return nil
	})
}

func (p *Processor) claimEligibility(ctx context.Context, now time.Time) (Fulfillment, bool, error) {
	var claimed Fulfillment
	found := false
	err := p.withFencedImmediate(ctx, func(tx *sql.Tx) error {
		candidate, err := scanFulfillment(tx.QueryRowContext(ctx, `SELECT `+fulfillmentColumns+` FROM membership_fulfillments
      WHERE state IN ('QUEUED','ACCOUNT_CHECKING','ACCOUNT_REPURCHASE_NOT_READY','INVENTORY_NOT_READY',
        'CARD_PRICE_UNAVAILABLE','CHECKOUT_PRICE_UNRECOGNIZED','CHECKOUT_PRE_SUBMIT_FAILED','MEMBERSHIP_CONTRACT_UNKNOWN')
        AND (retry_at IS NULL OR retry_at <= ?)
        AND (state <> 'CHECKOUT_PRE_SUBMIT_FAILED' OR run_mode IS NULL)
      ORDER BY created_at LIMIT 1`, store.ISO(now)))
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `
      UPDATE membership_fulfillments
      SET state='ACCOUNT_CHECKING', current_stage='eligibility', state_revision=state_revision+1,
          retry_at=NULL, failure_code=NULL, updated_at=?
      WHERE id=? AND state=? AND state_revision=?`, store.ISO(now), candidate.ID, candidate.State, candidate.StateRevision)
		if err != nil {
			return err
		}
		count, _ := result.RowsAffected()
		if count != 1 {
			return nil
		}
		claimed, err = loadFulfillment(ctx, tx, candidate.ID)
		found = err == nil
		return err
	})
	return claimed, found, err
}

func (p *Processor) processEligibility(ctx context.Context, fulfillment Fulfillment, now time.Time) error {
	var encrypted string
	if err := p.store.DB().QueryRowContext(ctx, `SELECT session_payload FROM redeem_orders WHERE id=?`, fulfillment.OrderID).Scan(&encrypted); err != nil {
		return p.handleSessionFailure(ctx, fulfillment.ID, "SESSION_INVALID", "eligibility", now)
	}
	plain, err := p.decrypter.Decrypt(encrypted)
	if err != nil || !json.Valid([]byte(plain)) {
		return p.handleSessionFailure(ctx, fulfillment.ID, "SESSION_INVALID", "eligibility", now)
	}
	allowed, err := p.acquireCircuit(ctx, "membership_provider", "default", now)
	if err != nil {
		return err
	}
	if !allowed {
		cause := coded("MEMBERSHIP_PROVIDER_CIRCUIT_OPEN", "membership provider dependency circuit is open")
		if persistErr := p.eligibilityFailure(ctx, fulfillment, errorCode(cause), "ACCOUNT_CHECKING", now, sharedRetry); persistErr != nil {
			return persistErr
		}
		return cause
	}
	raw, err := p.membership.Fetch(ctx, json.RawMessage(plain))
	if leaseErr := p.assertWorkAllowed(ctx); leaseErr != nil {
		return leaseErr
	}
	if err != nil {
		code := errorCode(err)
		state := "ACCOUNT_CHECKING"
		if code == "MEMBERSHIP_CONTRACT_UNKNOWN" {
			state = "MEMBERSHIP_CONTRACT_UNKNOWN"
		}
		return p.eligibilityDependencyFailure(ctx, fulfillment, err, state, now)
	}
	observation, err := domain.NormalizeMembershipEnvelope(raw, now)
	if err != nil {
		cause := codedWrap(domain.ErrorCode(err), "membership provider response contract is invalid", err)
		return p.eligibilityDependencyFailure(ctx, fulfillment, cause, "MEMBERSHIP_CONTRACT_UNKNOWN", now)
	}
	if observation.AutoRenew != nil && *observation.AutoRenew {
		if err := p.persistObservation(ctx, fulfillment.ID, "", "starting_before_renewal_cancel", observation); err != nil {
			return err
		}
		token, err := p.renewalToken(ctx)
		if err != nil {
			return p.eligibilityFailure(ctx, fulfillment, errorCode(err), "ACCOUNT_REPURCHASE_NOT_READY", now, sharedRetry)
		}
		if err := p.renewal.Cancel(ctx, json.RawMessage(plain), token); err != nil {
			if isSessionFailureCode(errorCode(err)) {
				return p.handleSessionFailure(ctx, fulfillment.ID, errorCode(err), "eligibility", now)
			}
			return p.eligibilityFailure(ctx, fulfillment, errorCode(err), "ACCOUNT_REPURCHASE_NOT_READY", now, sharedRetry)
		}
		if leaseErr := p.assertWorkAllowed(ctx); leaseErr != nil {
			return leaseErr
		}
		raw, err = p.membership.Fetch(ctx, json.RawMessage(plain))
		if leaseErr := p.assertWorkAllowed(ctx); leaseErr != nil {
			return leaseErr
		}
		if err != nil {
			return p.eligibilityDependencyFailure(ctx, fulfillment, err, "ACCOUNT_CHECKING", now)
		}
		observation, err = domain.NormalizeMembershipEnvelope(raw, now)
		if err != nil {
			cause := codedWrap(domain.ErrorCode(err), "post-cancellation membership response is invalid", err)
			return p.eligibilityDependencyFailure(ctx, fulfillment, cause, "MEMBERSHIP_CONTRACT_UNKNOWN", now)
		}
	}
	classification, err := domain.ClassifyStartingMembership(observation)
	if err != nil {
		cause := codedWrap(domain.ErrorCode(err), "membership provider response cannot be classified", err)
		return p.eligibilityDependencyFailure(ctx, fulfillment, cause, "MEMBERSHIP_CONTRACT_UNKNOWN", now)
	}
	if classification == domain.StartingMembershipUnknown {
		cause := coded("MEMBERSHIP_CONTRACT_UNKNOWN", "membership provider response classification is unknown")
		if err := p.persistObservation(ctx, fulfillment.ID, "", "starting_eligibility", observation); err != nil {
			return withCircuitAccounting(err,
				p.recordCircuitFailure(ctx, "membership_provider", "default", cause, now))
		}
		return p.eligibilityDependencyFailure(ctx, fulfillment, cause, "MEMBERSHIP_CONTRACT_UNKNOWN", now)
	}
	circuitErr := p.recordCircuitSuccess(ctx, "membership_provider", "default", now)
	if err := p.persistObservation(ctx, fulfillment.ID, "", "starting_eligibility", observation); err != nil {
		return withCircuitAccounting(err, circuitErr)
	}
	switch classification {
	case domain.StartingMembershipSubscribed:
		_, err = p.transition(ctx, fulfillment.ID, "ACCOUNT_ALREADY_SUBSCRIBED", now, transitionOptions{CurrentStage: pointer("eligibility"), FailureCode: pointer("ACCOUNT_ALREADY_SUBSCRIBED"), ExpectedRevision: &fulfillment.StateRevision})
		return withCircuitAccounting(err, circuitErr)
	case domain.StartingMembershipDelinquent:
		return withCircuitAccounting(
			p.eligibilityFailure(ctx, fulfillment, "ACCOUNT_REPURCHASE_NOT_READY", "ACCOUNT_REPURCHASE_NOT_READY", now, accountRetry),
			circuitErr,
		)
	case domain.StartingMembershipFree:
		return withCircuitAccounting(p.preflightReadiness(ctx, fulfillment, now), circuitErr)
	default:
		return withCircuitAccounting(
			p.eligibilityFailure(ctx, fulfillment, "MEMBERSHIP_CONTRACT_UNKNOWN", "MEMBERSHIP_CONTRACT_UNKNOWN", now, sharedRetry),
			circuitErr,
		)
	}
}

func (p *Processor) eligibilityDependencyFailure(ctx context.Context, fulfillment Fulfillment, cause error, state string, now time.Time) error {
	code := errorCode(cause)
	var persistErr error
	if isSessionFailureCode(code) {
		persistErr = p.handleSessionFailure(ctx, fulfillment.ID, code, "eligibility", now)
	} else {
		persistErr = p.eligibilityFailure(ctx, fulfillment, code, state, now, sharedRetry)
	}
	var circuitErr error
	if immediateCircuitCodes[code] || transientCircuitCodes[code] {
		circuitErr = p.recordCircuitFailure(ctx, "membership_provider", "default", cause, now)
	} else {
		// An order-scoped response (for example an expired Session) proves the
		// provider is reachable and must release a half-open global probe.
		circuitErr = p.recordCircuitSuccess(ctx, "membership_provider", "default", now)
	}
	if persistErr != nil {
		return withCircuitAccounting(persistErr, circuitErr)
	}
	return withCircuitAccounting(cause, circuitErr)
}

func (p *Processor) eligibilityFailure(ctx context.Context, fulfillment Fulfillment, code, state string, now time.Time, retry time.Duration) error {
	retryAt := store.ISO(now.Add(retry))
	_, err := p.transition(ctx, fulfillment.ID, state, now, transitionOptions{CurrentStage: pointer("eligibility"), FailureCode: pointer(code), RetryAt: &retryAt, ExpectedRevision: &fulfillment.StateRevision})
	return err
}

func (p *Processor) persistObservation(ctx context.Context, fulfillmentID, stage, purpose string, observation *domain.MembershipObservation) error {
	id, err := store.NewID("mfo_")
	if err != nil {
		return err
	}
	stageValue := any(nil)
	if stage != "" {
		stageValue = stage
	}
	autoRenew := any(nil)
	if observation.AutoRenew != nil {
		if *observation.AutoRenew {
			autoRenew = 1
		} else {
			autoRenew = 0
		}
	}
	expire := any(nil)
	if observation.ExpireTime != nil {
		expire = store.ISO(*observation.ExpireTime)
	}
	_, err = p.fencedExec(ctx, `
    INSERT INTO membership_observations (id, fulfillment_id, stage_key, purpose, provider_code,
      account_type, currency, auto_renew, is_overdue, is_delinquent, expire_time, observed_at)
    VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, id, fulfillmentID, stageValue, purpose,
		observation.ProviderCode, string(observation.AccountType), nullString(observation.Currency), autoRenew,
		boolInt(observation.IsOverdue), boolInt(observation.IsDelinquent), expire, store.ISO(observation.ObservedAt))
	return err
}

func (p *Processor) preflightReadiness(ctx context.Context, fulfillment Fulfillment, now time.Time) error {
	var inventoryStatus string
	if err := p.store.DB().QueryRowContext(ctx, `SELECT inventory_status FROM membership_fulfillment_settings WHERE id='default'`).Scan(&inventoryStatus); err != nil {
		return err
	}
	if inventoryStatus != "completed" {
		return p.eligibilityFailure(ctx, fulfillment, "INVENTORY_NOT_READY", "INVENTORY_NOT_READY", now, sharedRetry)
	}
	ready, err := p.inventoryPlanAvailable(ctx, domain.Tier(fulfillment.TargetTier), now)
	if err != nil {
		return err
	}
	if !ready {
		return p.eligibilityFailure(ctx, fulfillment, "CARD_PRICE_UNAVAILABLE", "CARD_PRICE_UNAVAILABLE", now, sharedRetry)
	}
	var contractCount int
	if err := p.store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM checkout_price_contracts WHERE tier='plus' AND currency='PHP' AND status='active'`).Scan(&contractCount); err != nil {
		return err
	}
	if contractCount != 1 {
		return p.eligibilityFailure(ctx, fulfillment, "CHECKOUT_PRICE_CONTRACT_MISSING", "CHECKOUT_PRICE_UNRECOGNIZED", now, sharedRetry)
	}
	_, err = p.transition(ctx, fulfillment.ID, "CHECKOUT_PREFLIGHT_READY", now, transitionOptions{
		CurrentStage: pointer("plus"), ExpectedRevision: &fulfillment.StateRevision,
	})
	return err
}

func (p *Processor) inventoryPlanAvailable(ctx context.Context, tier domain.Tier, now time.Time) (bool, error) {
	rows, err := p.store.DB().QueryContext(ctx, `
    SELECT id, lane, consumed_slots, capacity_state FROM managed_cards
    WHERE upstream_status='ACTIVE' AND reconciliation_state='READY' AND (lane IS NULL OR lane=?)
    ORDER BY CASE WHEN lane=? THEN 0 ELSE 1 END, id`, string(tier), string(tier))
	if err != nil {
		return false, err
	}
	type card struct {
		id       string
		lane     sql.NullString
		consumed int
		state    string
	}
	var cards []card
	for rows.Next() {
		var item card
		if err := rows.Scan(&item.id, &item.lane, &item.consumed, &item.state); err != nil {
			rows.Close()
			return false, err
		}
		cards = append(cards, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return false, err
	}
	if err := rows.Close(); err != nil {
		return false, err
	}
	capacity, ok := domain.CapacityForTier(tier)
	if !ok {
		return false, coded("TARGET_TIER_INVALID", "target tier is invalid")
	}
	for _, card := range cards {
		var active int
		if err := p.store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM card_capacity_reservations
      WHERE card_id=? AND target_lane=? AND state IN ('reserved','consumed','retained_partial')`, card.id, string(tier)).Scan(&active); err != nil {
			return false, err
		}
		if card.state == "CAPACITY_FULL" || max(card.consumed, active) >= capacity {
			continue
		}
		if ok, err := p.cardHasFreshBudget(ctx, card.id, tier, now); err != nil {
			return false, err
		} else if ok {
			return true, nil
		}
	}
	rows, err = p.store.DB().QueryContext(ctx, `SELECT DISTINCT c.id FROM card_product_policies p
    JOIN managed_cards c ON c.product_code=p.product_code
    WHERE p.enabled=1 AND `+productPriceEvidenceCardWhereSQL+`
    ORDER BY p.product_code,c.id`)
	if err != nil {
		return false, err
	}
	var evidenceCardIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return false, err
		}
		evidenceCardIDs = append(evidenceCardIDs, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return false, err
	}
	if err := rows.Close(); err != nil {
		return false, err
	}
	for _, id := range evidenceCardIDs {
		if ok, err := p.cardHasFreshBudget(ctx, id, tier, now); err != nil {
			return false, err
		} else if ok {
			return true, nil
		}
	}
	return false, nil
}

func (p *Processor) cardHasFreshBudget(ctx context.Context, cardID string, tier domain.Tier, now time.Time) (bool, error) {
	rows, err := p.store.DB().QueryContext(ctx, `SELECT tier,found,amount,provider_time FROM card_price_signals WHERE card_id=?`, cardID)
	if err != nil {
		return false, err
	}
	var signals []domain.PriceSignal
	for rows.Next() {
		var signal domain.PriceSignal
		var tier string
		var found int
		var amount float64
		var providerTime sql.NullString
		if err := rows.Scan(&tier, &found, &amount, &providerTime); err != nil {
			rows.Close()
			return false, err
		}
		signal.Tier = domain.Tier(tier)
		signal.Found = found == 1
		signal.AmountUSD = amount
		if providerTime.Valid {
			signal.Time = providerTime.String
		}
		signals = append(signals, signal)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return false, err
	}
	if err := rows.Close(); err != nil {
		return false, err
	}
	_, err = domain.CalculateMembershipBudget(signals, tier, now)
	return err == nil, nil
}

func nullString(value string) any {
	if value == "" {
		return nil
	}
	return value
}
func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
func max(left, right int) int {
	if left > right {
		return left
	}
	return right
}

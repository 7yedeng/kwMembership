package processor

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"kwmembership/internal/checkout"
	"kwmembership/internal/provider"
	"kwmembership/internal/store"
)

const (
	checkoutRetryDelay                 = 5 * time.Minute
	interactivePreflightAdapterVersion = "go-interactive-login-v0"
)

type checkoutStage struct {
	ID                string
	StageKey          string
	State             string
	CardID            sql.NullString
	ProviderKey       string
	UpstreamCardID    sql.NullInt64
	AttemptNo         sql.NullInt64
	AdapterVersion    sql.NullString
	PriceContractID   string
	PriceContractVer  int
	PriceContractTier string
	PriceContractCurr string
	PriceContractMin  float64
	PriceContractMax  float64
}

type checkoutExecution struct {
	Fulfillment Fulfillment
	Stage       checkoutStage
	AttemptNo   int64
}

func (p *Processor) tickCheckout(ctx context.Context) (bool, error) {
	now := p.now().UTC()
	fulfillment, found, err := p.checkoutDue(ctx, now)
	if err != nil || !found {
		return false, err
	}
	if fulfillment.State == "CHECKOUT_PREFLIGHT_READY" || fulfillment.State == "CHECKOUT_CHALLENGE_WAIT" ||
		fulfillment.State == "CHECKOUT_LOGIN_READY" || fulfillment.State == "CHECKOUT_LOGIN_WAIT" {
		return true, p.checkoutPreflight(ctx, fulfillment, now)
	}
	execution, ready, err := p.checkoutPrepareExecution(ctx, fulfillment, now)
	if err != nil || !ready {
		return true, err
	}
	return true, p.checkoutExecute(ctx, execution, now)
}

func (p *Processor) checkoutDue(ctx context.Context, now time.Time) (Fulfillment, bool, error) {
	item, err := scanFulfillment(p.store.DB().QueryRowContext(ctx, `SELECT `+fulfillmentColumns+`
    FROM membership_fulfillments
    WHERE state IN (
      'CHECKOUT_PREFLIGHT_READY','CHECKOUT_CHALLENGE_WAIT','CHECKOUT_LOGIN_READY','CHECKOUT_LOGIN_WAIT',
	  'CHECKOUT_EXECUTION_WAIT','CHECKOUT_PRE_SUBMIT_FAILED',
      'PLUS_CONFIRMED','PLUS_APPROVAL_WAIT','UPGRADE_APPROVAL_WAIT'
    )
      AND (retry_at IS NULL OR retry_at<=?)
      AND (state<>'CHECKOUT_PRE_SUBMIT_FAILED' OR run_mode IN ('canary','automatic'))
    ORDER BY updated_at,id LIMIT 1`, store.ISO(now)))
	if errors.Is(err, sql.ErrNoRows) {
		return Fulfillment{}, false, nil
	}
	return item, err == nil, err
}

func (p *Processor) checkoutPreflight(ctx context.Context, fulfillment Fulfillment, now time.Time) error {
	interactive := fulfillment.State == "CHECKOUT_LOGIN_READY" || fulfillment.State == "CHECKOUT_LOGIN_WAIT"
	contract, err := p.checkoutActiveContract(ctx, "plus")
	if err != nil {
		return p.checkoutRecordPreflightFailure(ctx, fulfillment, err, now)
	}
	request := checkout.Request{
		Mode: checkout.ModePreflight, Stage: "plus", TargetTier: fulfillment.TargetTier,
		PriceContract: contract,
	}
	if interactive {
		expectedEmail, err := p.checkoutSessionEmail(ctx, fulfillment.ID)
		if err != nil {
			return p.checkoutRecordPreflightFailure(ctx, fulfillment, err, now)
		}
		request.Mode = checkout.ModeInteractivePreflight
		request.ExpectedEmail = expectedEmail
		request.OnHandoff = func(ctx context.Context, handoff checkout.Handoff) error {
			reason := "INTERACTIVE_LOGIN_REQUIRED"
			if handoff.Type == "challenge-cloudflare" || handoff.Type == "cloudflare" {
				reason = "CLOUDFLARE_CHALLENGE_REQUIRED"
			}
			return p.checkoutInterventionTransition(ctx, fulfillment.ID, "CHECKOUT_LOGIN_WAIT", reason, "plus", p.now().UTC())
		}
	} else {
		session, err := p.rcLoadSession(ctx, fulfillment.ID)
		if err != nil {
			return p.checkoutRecordPreflightFailure(ctx, fulfillment, err, now)
		}
		browserSession, err := provider.BrowserSessionFromJSON(session, now)
		if err != nil {
			return p.checkoutRecordPreflightFailure(ctx, fulfillment, err, now)
		}
		request.Mode = checkout.ModeSessionPreflight
		request.ExpectedEmail = browserSession.Email
		request.Cookies = browserSession.Cookies
		request.Session = session
		request.Binding = checkout.ExecutionBinding{
			FulfillmentID: fulfillment.ID, FulfillmentRevision: fulfillment.StateRevision,
			AttemptNo: fulfillment.StateRevision, PriorityClass: "normal", AdapterVersion: paymentAdapterVersion,
		}
	}
	if p.config.VisibleBrowser && !interactive {
		request.OnHandoff = func(ctx context.Context, challenge checkout.Handoff) error {
			reason := "SECURITY_CHALLENGE_REQUIRED"
			if challenge.Type == "cloudflare" || challenge.Type == "challenge-cloudflare" {
				reason = "CLOUDFLARE_CHALLENGE_REQUIRED"
			}
			return p.checkoutInterventionTransition(ctx, fulfillment.ID, "CHECKOUT_CHALLENGE_WAIT", reason, "plus", p.now().UTC())
		}
	}
	result, err := p.executor.Execute(ctx, request)
	if guardErr := p.assertWorkAllowed(ctx); guardErr != nil {
		return guardErr
	}
	finishedAt := p.now().UTC()
	if errorCode(err) == "EXECUTOR_PENDING" {
		return nil
	}
	if err != nil {
		return p.checkoutRecordPreflightFailure(ctx, fulfillment, err, finishedAt)
	}
	if result.Challenge {
		return p.checkoutRecordPreflightFailure(ctx, fulfillment,
			coded("SECURITY_CHALLENGE_REQUIRED", "checkout preflight requires a security challenge"), finishedAt)
	}
	current, err := loadFulfillment(ctx, p.store.DB(), fulfillment.ID)
	if err != nil {
		return err
	}
	validCurrent := current.State == "CHECKOUT_PREFLIGHT_READY" || current.State == "CHECKOUT_CHALLENGE_WAIT"
	if interactive {
		validCurrent = current.State == "CHECKOUT_LOGIN_READY" || current.State == "CHECKOUT_LOGIN_WAIT"
	}
	if !validCurrent {
		return coded("MEMBERSHIP_REVISION_CONFLICT", "membership state changed during checkout preflight")
	}
	completedAt := finishedAt
	adapterVersion := paymentAdapterVersion
	nextState := "FUNDING_READY"
	if interactive {
		adapterVersion = interactivePreflightAdapterVersion
		nextState = "CHECKOUT_LOGIN_PREFLIGHT_PASSED"
	}
	if err := p.checkoutPersistPreflight(ctx, current, contract, result.Page, adapterVersion, completedAt); err != nil {
		return err
	}
	_, err = p.transition(ctx, current.ID, nextState, completedAt, transitionOptions{
		CurrentStage: pointer("plus"), ExpectedRevision: &current.StateRevision,
	})
	return err
}

func (p *Processor) checkoutPersistPreflight(ctx context.Context, fulfillment Fulfillment, contract checkout.PriceContract, page checkout.PageFacts, adapterVersion string, now time.Time) error {
	var siteID, productID string
	if err := p.store.DB().QueryRowContext(ctx, `SELECT o.site_id,o.product_id
    FROM membership_fulfillments f JOIN redeem_orders o ON o.id=f.order_id WHERE f.id=?`, fulfillment.ID).
		Scan(&siteID, &productID); err != nil {
		return err
	}
	id, err := store.NewID("cvr_")
	if err != nil {
		return err
	}
	sanitized, _ := json.Marshal(page)
	at := store.ISO(now)
	_, err = p.fencedExec(ctx, `INSERT INTO checkout_validation_runs
    (id,order_id,site_id,product_id,tier,adapter_version,price_contract_id,status,
     sanitized_result,started_at,completed_at,created_by)
	VALUES (?,?,?,?,?,?,?,'passed',?,?,?,'go')`, id, fulfillment.OrderID,
		siteID, productID, "plus", adapterVersion, contract.ID, string(sanitized), at, at)
	return err
}

func (p *Processor) checkoutRecordPreflightFailure(ctx context.Context, fulfillment Fulfillment, cause error, now time.Time) error {
	code := errorCode(cause)
	if isSessionFailureCode(code) {
		if err := p.handleSessionFailure(ctx, fulfillment.ID, code, "eligibility", now); err != nil {
			return err
		}
		return cause
	}
	if code == "CHATGPT_ACCOUNT_ALREADY_SUBSCRIBED" {
		if _, err := p.transition(ctx, fulfillment.ID, "ACCOUNT_ALREADY_SUBSCRIBED", now, transitionOptions{
			CurrentStage: pointer("eligibility"), FailureCode: pointer(code),
		}); err != nil {
			return err
		}
		return cause
	}
	if code == "CHECKOUT_UI_UNSUPPORTED" || code == "SECURITY_CHALLENGE_REQUIRED" || code == "SECURITY_CHALLENGE_TIMEOUT" ||
		sessionCheckoutIntervention(code) ||
		strings.HasPrefix(code, "INTERACTIVE_LOGIN_") {
		if err := p.checkoutInterventionTransition(ctx, fulfillment.ID, "CHECKOUT_UI_UNSUPPORTED", code, "plus", now); err != nil {
			return err
		}
		return cause
	}
	retryAt := store.ISO(now.Add(checkoutRetryDelay))
	if _, err := p.transition(ctx, fulfillment.ID, "CHECKOUT_PREFLIGHT_READY", now, transitionOptions{
		CurrentStage: pointer("plus"), FailureCode: pointer(code), RetryAt: &retryAt,
	}); err != nil {
		return err
	}
	return cause
}

func (p *Processor) checkoutSessionEmail(ctx context.Context, fulfillmentID string) (string, error) {
	session, err := p.rcLoadSession(ctx, fulfillmentID)
	if err != nil {
		return "", err
	}
	var identity map[string]any
	if json.Unmarshal(session, &identity) != nil {
		return "", coded("SESSION_INVALID", "session is invalid")
	}
	email := strings.ToLower(strings.TrimSpace(sessionEmail(identity)))
	if !validEmail(email) {
		return "", coded("EXPECTED_IDENTITY_MISSING", "session identity is missing")
	}
	return email, nil
}

func (p *Processor) checkoutPrepareExecution(ctx context.Context, fulfillment Fulfillment, now time.Time) (checkoutExecution, bool, error) {
	var execution checkoutExecution
	ready := false
	err := p.withFencedImmediate(ctx, func(tx *sql.Tx) error {
		current, err := loadFulfillment(ctx, tx, fulfillment.ID)
		if err != nil {
			return err
		}
		stageKey := "plus"
		if current.State == "PLUS_CONFIRMED" || current.CurrentStage.Valid && current.CurrentStage.String == "upgrade" || strings.HasPrefix(current.State, "UPGRADE_") {
			stageKey = "upgrade"
		}
		stage, err := loadCheckoutStage(ctx, tx, current.ID, stageKey)
		if err != nil {
			return err
		}
		expectedContractTier := "plus"
		if stageKey == "upgrade" {
			expectedContractTier = current.TargetTier
		}
		if stage.PriceContractTier != expectedContractTier {
			return coded("PAYMENT_PRICE_CONTRACT_INVALID", "checkout price contract tier is invalid")
		}
		if !stage.CardID.Valid || !stage.UpstreamCardID.Valid || stage.UpstreamCardID.Int64 <= 0 || stage.AdapterVersion.String != paymentAdapterVersion {
			return coded("PAYMENT_STAGE_SNAPSHOT_CONFLICT", "checkout payment-stage snapshot is incompatible")
		}
		var unresolvedID string
		var unresolvedAttempt int64
		err = tx.QueryRowContext(ctx, `SELECT id,attempt_no FROM membership_action_permits
      WHERE fulfillment_id=? AND stage_key=? AND action_type='progression'
        AND state IN ('issued','activated','outcome_uncertain')
      ORDER BY sequence_no DESC LIMIT 1`, current.ID, stageKey).Scan(&unresolvedID, &unresolvedAttempt)
		if err == nil {
			at := store.ISO(now)
			if _, err := tx.ExecContext(ctx, `UPDATE membership_action_permits
          SET state='blocked',reported_at=COALESCE(reported_at,?),outcome_code='UNEXPECTED_PREAUTH'
          WHERE id=?`, at, unresolvedID); err != nil {
				return err
			}
			updated, err := transitionWithTx(ctx, tx, current.ID, "UNEXPECTED_PREAUTH", now, transitionOptions{
				CurrentStage: pointer(stageKey), FailureCode: pointer("PROGRESSION_OUTCOME_UNKNOWN"),
			})
			if err != nil {
				return err
			}
			return insertCheckoutIntervention(ctx, tx, updated, "PROGRESSION_OUTCOME_UNKNOWN", now)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		attemptNo := int64(0)
		if stage.AttemptNo.Valid && stage.AttemptNo.Int64 > 0 && stage.State == "checkout_ready" {
			var open int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM membership_fulfillment_attempts
          WHERE fulfillment_id=? AND stage=? AND attempt_no=? AND ended_at IS NULL`, current.ID, stageKey, stage.AttemptNo.Int64).Scan(&open); err != nil {
				return err
			}
			if open == 1 {
				attemptNo = stage.AttemptNo.Int64
			}
		}
		if attemptNo == 0 {
			if stage.State != "checkout_pending" && stage.State != "preflight_pending" && stage.State != "checkout_ready" {
				return coded("PAYMENT_STAGE_SNAPSHOT_CONFLICT", "checkout payment stage is not ready")
			}
			if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(attempt_no),0)+1
          FROM membership_fulfillment_attempts WHERE fulfillment_id=? AND stage=?`, current.ID, stageKey).Scan(&attemptNo); err != nil {
				return err
			}
			attemptID, err := store.NewID("mfa_")
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO membership_fulfillment_attempts
          (id,fulfillment_id,stage,attempt_no,resume_revision,adapter_version,price_contract_version,started_at)
          VALUES (?,?,?,?,?,?,?,?)`, attemptID, current.ID, stageKey, attemptNo, 0, paymentAdapterVersion,
				stage.PriceContractVer, store.ISO(now)); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE membership_payment_stages
          SET state='checkout_ready',attempt_no=?,adapter_version=?,adapter_path=?,
              page_fingerprint=NULL,page_permit_kind=NULL,page_control_id=NULL,page_ready_at=NULL,
              page_facts_json=NULL,updated_at=? WHERE id=?`, attemptNo, paymentAdapterVersion,
				paymentAdapterVersion, store.ISO(now), stage.ID); err != nil {
				return err
			}
			stage.AttemptNo = sql.NullInt64{Int64: attemptNo, Valid: true}
			stage.State = "checkout_ready"
		}
		if current.State != "CHECKOUT_EXECUTION_WAIT" {
			updated, err := transitionWithTx(ctx, tx, current.ID, "CHECKOUT_EXECUTION_WAIT", now, transitionOptions{CurrentStage: pointer(stageKey)})
			if err != nil {
				return err
			}
			current = updated
		}
		execution = checkoutExecution{Fulfillment: current, Stage: stage, AttemptNo: attemptNo}
		ready = true
		return nil
	})
	return execution, ready, err
}

func loadCheckoutStage(ctx context.Context, query store.Execer, fulfillmentID, stageKey string) (checkoutStage, error) {
	var stage checkoutStage
	hasProvider, err := tableHasColumn(ctx, query, "managed_cards", "provider_key")
	if err != nil {
		return checkoutStage{}, err
	}
	if hasProvider {
		err = query.QueryRowContext(ctx, `SELECT stage.id,stage.stage_key,stage.state,stage.card_id,card.provider_key,card.upstream_card_id,
      stage.attempt_no,stage.adapter_version,contract.id,contract.version,contract.tier,contract.currency,
      contract.min_amount,contract.max_amount
    FROM membership_payment_stages stage
    JOIN managed_cards card ON card.id=stage.card_id
    JOIN checkout_price_contracts contract ON contract.id=stage.price_contract_id
    WHERE stage.fulfillment_id=? AND stage.stage_key=?`, fulfillmentID, stageKey).Scan(
			&stage.ID, &stage.StageKey, &stage.State, &stage.CardID, &stage.ProviderKey, &stage.UpstreamCardID,
			&stage.AttemptNo, &stage.AdapterVersion, &stage.PriceContractID, &stage.PriceContractVer,
			&stage.PriceContractTier, &stage.PriceContractCurr, &stage.PriceContractMin, &stage.PriceContractMax,
		)
	} else {
		stage.ProviderKey = provider.CardPlatformSpaceX
		err = query.QueryRowContext(ctx, `SELECT stage.id,stage.stage_key,stage.state,stage.card_id,card.upstream_card_id,
	      stage.attempt_no,stage.adapter_version,contract.id,contract.version,contract.tier,contract.currency,
	      contract.min_amount,contract.max_amount
	    FROM membership_payment_stages stage
	    JOIN managed_cards card ON card.id=stage.card_id
	    JOIN checkout_price_contracts contract ON contract.id=stage.price_contract_id
	    WHERE stage.fulfillment_id=? AND stage.stage_key=?`, fulfillmentID, stageKey).Scan(
			&stage.ID, &stage.StageKey, &stage.State, &stage.CardID, &stage.UpstreamCardID,
			&stage.AttemptNo, &stage.AdapterVersion, &stage.PriceContractID, &stage.PriceContractVer,
			&stage.PriceContractTier, &stage.PriceContractCurr, &stage.PriceContractMin, &stage.PriceContractMax,
		)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return checkoutStage{}, coded("PAYMENT_STAGE_EVIDENCE_INCOMPLETE", "checkout payment stage is missing")
	}
	if err != nil {
		return checkoutStage{}, err
	}
	if stage.PriceContractCurr != "PHP" || stage.PriceContractMin <= 0 || stage.PriceContractMax < stage.PriceContractMin {
		return checkoutStage{}, coded("PAYMENT_PRICE_CONTRACT_INVALID", "checkout price contract is invalid")
	}
	return stage, nil
}

func (p *Processor) checkoutExecute(ctx context.Context, execution checkoutExecution, now time.Time) error {
	session, err := p.rcLoadSession(ctx, execution.Fulfillment.ID)
	if err != nil {
		return p.checkoutRecordExecutionFailure(ctx, execution, err, now)
	}
	browserSession, err := provider.BrowserSessionFromJSON(session, now)
	if err != nil {
		return p.checkoutRecordExecutionFailure(ctx, execution, err, now)
	}
	client, err := p.cardPlatform(ctx, execution.Stage.ProviderKey)
	if err != nil {
		return p.checkoutRecordExecutionFailure(ctx, execution, err, now)
	}
	card, err := client.GetCardMaterial(ctx, execution.Stage.UpstreamCardID.Int64, now)
	if guardErr := p.assertWorkAllowed(ctx); guardErr != nil {
		return guardErr
	}
	if err != nil {
		return p.checkoutRecordExecutionFailure(ctx, execution, err, now)
	}
	billing, err := p.address.Billing(ctx)
	if guardErr := p.assertWorkAllowed(ctx); guardErr != nil {
		return guardErr
	}
	if err != nil {
		return p.checkoutRecordExecutionFailure(ctx, execution, err, now)
	}
	guard := &checkoutGuard{processor: p, execution: execution, client: client}
	request := checkout.Request{
		Mode: checkout.ModeSessionCheckout, Stage: execution.Stage.StageKey, TargetTier: execution.Fulfillment.TargetTier,
		Cookies: browserSession.Cookies, ExpectedEmail: browserSession.Email,
		Session: session,
		PriceContract: checkout.PriceContract{
			ID: execution.Stage.PriceContractID, Version: execution.Stage.PriceContractVer,
			Tier: execution.Stage.PriceContractTier, Currency: execution.Stage.PriceContractCurr,
			MinAmount: execution.Stage.PriceContractMin, MaxAmount: execution.Stage.PriceContractMax,
		},
		Material: &checkout.Material{
			Card:    checkout.CardMaterial{Number: card.Number, CVV: card.CVV, ExpiryMonth: card.ExpiryMonth, ExpiryYear: card.ExpiryYear},
			Billing: checkout.BillingAddress{Name: billing.Name, Line1: billing.Line1, City: billing.City, State: billing.State, PostalCode: billing.PostalCode, Country: billing.Country},
		},
		Guard: guard,
		Binding: checkout.ExecutionBinding{
			FulfillmentID: execution.Fulfillment.ID, FulfillmentRevision: execution.Fulfillment.StateRevision,
			AttemptNo: execution.AttemptNo, PriorityClass: checkoutPriority(execution), AdapterVersion: paymentAdapterVersion,
		},
	}
	if execution.Stage.StageKey == "upgrade" {
		request.PlanURL = checkout.PlanManagementURL
	}
	result, err := p.executor.Execute(ctx, request)
	request.Material = nil
	if guardErr := p.assertWorkAllowed(ctx); guardErr != nil {
		return guardErr
	}
	if errorCode(err) == "EXECUTOR_PENDING" {
		return nil
	}
	if err != nil {
		var browserError *checkout.Error
		if errors.As(err, &browserError) && browserError.Uncertain {
			if persistErr := p.checkoutMarkUncertain(ctx, execution, browserError, p.now().UTC()); persistErr != nil {
				return persistErr
			}
			return err
		}
		return p.checkoutRecordExecutionFailure(ctx, execution, err, p.now().UTC())
	}
	if result.Challenge {
		cause := coded("SECURITY_CHALLENGE_REQUIRED", "headless checkout requires a security challenge")
		if err := p.checkoutInterventionTransition(ctx, execution.Fulfillment.ID, "CHECKOUT_UI_UNSUPPORTED", errorCode(cause), execution.Stage.StageKey, p.now().UTC()); err != nil {
			return err
		}
		return cause
	}
	return nil
}

func checkoutPriority(execution checkoutExecution) string {
	if execution.Stage.StageKey == "upgrade" {
		return "upgrade"
	}
	if execution.Fulfillment.MoneyBoundaryAt.Valid {
		return "recovery"
	}
	return "normal"
}

func (p *Processor) checkoutActiveContract(ctx context.Context, tier string) (checkout.PriceContract, error) {
	var contract checkout.PriceContract
	err := p.store.DB().QueryRowContext(ctx, `SELECT id,version,tier,currency,min_amount,max_amount
    FROM checkout_price_contracts WHERE tier=? AND currency='PHP' AND status='active'`, tier).Scan(
		&contract.ID, &contract.Version, &contract.Tier, &contract.Currency, &contract.MinAmount, &contract.MaxAmount,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return checkout.PriceContract{}, coded("CHECKOUT_PRICE_CONTRACT_MISSING", "checkout price contract is missing")
	}
	return contract, err
}

func (p *Processor) checkoutRecordExecutionFailure(ctx context.Context, execution checkoutExecution, cause error, now time.Time) error {
	code := errorCode(cause)
	if isSessionFailureCode(code) {
		if err := p.handleSessionFailure(ctx, execution.Fulfillment.ID, code, execution.Stage.StageKey, now); err != nil {
			return err
		}
		return cause
	}
	if code == "CHECKOUT_UI_UNSUPPORTED" || code == "SECURITY_CHALLENGE_REQUIRED" || sessionCheckoutIntervention(code) {
		if err := p.checkoutInterventionTransition(ctx, execution.Fulfillment.ID, "CHECKOUT_UI_UNSUPPORTED", code, execution.Stage.StageKey, now); err != nil {
			return err
		}
		return cause
	}
	if code == "CANARY_AUTHORIZATION_REQUIRED" {
		state := "PLUS_APPROVAL_WAIT"
		if execution.Stage.StageKey == "upgrade" {
			state = "UPGRADE_APPROVAL_WAIT"
		}
		retryAt := store.ISO(now.Add(30 * time.Second))
		if _, err := p.transition(ctx, execution.Fulfillment.ID, state, now, transitionOptions{
			CurrentStage: pointer(execution.Stage.StageKey), FailureCode: pointer(code), RetryAt: &retryAt,
		}); err != nil {
			return err
		}
		return cause
	}
	retryAt := store.ISO(now.Add(checkoutRetryDelay))
	err := p.withFencedImmediate(ctx, func(tx *sql.Tx) error {
		current, err := loadFulfillment(ctx, tx, execution.Fulfillment.ID)
		if err != nil {
			return err
		}
		if current.MoneyBoundaryAt.Valid {
			updated, err := transitionWithTx(ctx, tx, current.ID, "EXECUTOR_OUTCOME_UNCERTAIN", now, transitionOptions{
				CurrentStage: pointer(execution.Stage.StageKey), FailureCode: pointer(code),
			})
			if err != nil {
				return err
			}
			return insertCheckoutIntervention(ctx, tx, updated, code, now)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE membership_payment_stages
      SET state='checkout_pending',updated_at=? WHERE id=? AND state IN ('checkout_ready','progression_permitted')`,
			store.ISO(now), execution.Stage.ID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE membership_fulfillment_attempts
      SET ended_at=COALESCE(ended_at,?),outcome_code=COALESCE(outcome_code,?)
      WHERE fulfillment_id=? AND stage=? AND attempt_no=?`, store.ISO(now), code,
			execution.Fulfillment.ID, execution.Stage.StageKey, execution.AttemptNo); err != nil {
			return err
		}
		_, err = transitionWithTx(ctx, tx, execution.Fulfillment.ID, "CHECKOUT_PRE_SUBMIT_FAILED", now, transitionOptions{
			CurrentStage: pointer(execution.Stage.StageKey), FailureCode: pointer(code), RetryAt: &retryAt,
		})
		return err
	})
	if err != nil {
		return err
	}
	return cause
}

func sessionCheckoutIntervention(code string) bool {
	switch code {
	case "SESSION_COOKIE_MISSING", "CHATGPT_SESSION_UNAUTHORIZED", "CHATGPT_SESSION_IDENTITY_MISMATCH",
		"CHECKOUT_ENTRY_UNAVAILABLE", "CHECKOUT_API_AUTH_FAILED",
		"CHECKOUT_API_CONTRACT_DRIFT", "CHECKOUT_CONTEXT_INVALID":
		return true
	default:
		return false
	}
}

func (p *Processor) checkoutInterventionTransition(ctx context.Context, fulfillmentID, state, reason, stage string, now time.Time) error {
	return p.withFencedImmediate(ctx, func(tx *sql.Tx) error {
		updated, err := transitionWithTx(ctx, tx, fulfillmentID, state, now, transitionOptions{
			CurrentStage: pointer(stage), FailureCode: pointer(reason),
		})
		if err != nil {
			return err
		}
		return insertCheckoutIntervention(ctx, tx, updated, reason, now)
	})
}

func insertCheckoutIntervention(ctx context.Context, tx *sql.Tx, fulfillment Fulfillment, reason string, now time.Time) error {
	id, err := store.NewID("fi_")
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO fulfillment_interventions
    (id,fulfillment_id,state,state_revision,reason_code,created_at) VALUES (?,?,?,?,?,?)`,
		id, fulfillment.ID, fulfillment.State, fulfillment.StateRevision, reason, store.ISO(now))
	return err
}

type checkoutGuard struct {
	processor *Processor
	execution checkoutExecution
	client    provider.CardPlatform
}

func (g *checkoutGuard) BeforeAction(ctx context.Context, action checkout.Action) (checkout.Permit, error) {
	p := g.processor
	if action.Stage != g.execution.Stage.StageKey || action.ControlID == "" || action.PageFingerprint == "" {
		return checkout.Permit{}, coded("CHECKOUT_ACTION_INVALID", "checkout action binding is invalid")
	}
	transactions, err := p.loadAllTransactions(ctx, g.client, g.execution.Stage.UpstreamCardID.Int64)
	if err != nil {
		return checkout.Permit{}, err
	}
	if err := p.assertWorkAllowed(ctx); err != nil {
		return checkout.Permit{}, err
	}
	authIDs := make([]string, 0, len(transactions))
	seen := map[string]bool{}
	for _, transaction := range transactions {
		if !rcSafeID(transaction.AuthID) || seen[transaction.AuthID] {
			return checkout.Permit{}, coded("CARD_PLATFORM_CONTRACT_DRIFT", "transaction authorization identity is invalid")
		}
		seen[transaction.AuthID] = true
		authIDs = append(authIDs, transaction.AuthID)
	}
	sort.Strings(authIDs)
	permit := checkout.Permit{}
	var logicalErr error
	err = p.withFencedImmediate(ctx, func(tx *sql.Tx) error {
		fulfillment, err := loadFulfillment(ctx, tx, g.execution.Fulfillment.ID)
		if err != nil {
			return err
		}
		if fulfillment.State != "CHECKOUT_EXECUTION_WAIT" || fulfillment.CurrentStage.String != action.Stage {
			return coded("CHECKOUT_ACTION_STATE_CHANGED", "checkout fulfillment state changed")
		}
		stage, err := loadCheckoutStage(ctx, tx, fulfillment.ID, action.Stage)
		if err != nil {
			return err
		}
		if !stage.AttemptNo.Valid || stage.AttemptNo.Int64 != g.execution.AttemptNo || stage.State != "checkout_ready" || stage.CardID.String != g.execution.Stage.CardID.String {
			return coded("CHECKOUT_ACTION_STATE_CHANGED", "checkout payment stage changed")
		}
		facts, _ := json.Marshal(action.Page)
		at := store.ISO(nowUTC(p))
		if _, err := tx.ExecContext(ctx, `UPDATE membership_payment_stages
      SET page_fingerprint=?,page_permit_kind=?,page_control_id=?,page_ready_at=?,page_facts_json=?,updated_at=?
      WHERE id=?`, action.PageFingerprint, string(action.Kind), action.ControlID, at, string(facts), at, stage.ID); err != nil {
			return err
		}
		mode, authorizationID, err := checkoutAuthorize(ctx, tx, fulfillment, stage, action.Kind, nowUTC(p))
		if err != nil {
			if errorCode(err) == "CANARY_AUTHORIZATION_REQUIRED" {
				next := "PLUS_APPROVAL_WAIT"
				if action.Stage == "upgrade" {
					next = "UPGRADE_APPROVAL_WAIT"
				}
				retryAt := store.ISO(nowUTC(p).Add(30 * time.Second))
				if _, transitionErr := transitionWithTx(ctx, tx, fulfillment.ID, next, nowUTC(p), transitionOptions{
					CurrentStage: pointer(action.Stage), FailureCode: pointer("CANARY_AUTHORIZATION_REQUIRED"), RetryAt: &retryAt,
				}); transitionErr != nil {
					return transitionErr
				}
				logicalErr = err
				return nil
			}
			return err
		}
		var sequence int64
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence_no),0)+1 FROM membership_action_permits
      WHERE fulfillment_id=? AND stage_key=? AND attempt_no=? AND action_type=?`, fulfillment.ID,
			action.Stage, g.execution.AttemptNo, string(action.Kind)).Scan(&sequence); err != nil {
			return err
		}
		if sequence < 1 || sequence > 5 {
			return coded("PAYMENT_TRANSITION_LIMIT", "checkout action sequence limit reached")
		}
		permitID, err := store.NewID("map_")
		if err != nil {
			return err
		}
		expiresAt := store.ISO(nowUTC(p).Add(30 * time.Second))
		if _, err := tx.ExecContext(ctx, `INSERT INTO membership_action_permits
      (id,fulfillment_id,stage_key,attempt_no,action_type,sequence_no,installation_id,
       browser_lease_epoch,adapter_version,price_contract_id,control_id,page_fingerprint,state,
       issued_at,expires_at,authorization_mode,authorization_id)
      VALUES (?,?,?,?,?,?,'python-executor',?,?,?,?,?,'issued',?,?,?,?)`, permitID, fulfillment.ID,
			action.Stage, g.execution.AttemptNo, string(action.Kind), sequence, p.lease.Epoch,
			paymentAdapterVersion, stage.PriceContractID, action.ControlID, action.PageFingerprint,
			at, expiresAt, mode, authorizationID); err != nil {
			return err
		}
		for _, authID := range authIDs {
			if _, err := tx.ExecContext(ctx, `INSERT INTO membership_action_auth_snapshots
        (permit_id,card_id,auth_id,snapshotted_at) VALUES (?,?,?,?)`, permitID, stage.CardID.String, authID, at); err != nil {
				return err
			}
		}
		if action.Kind == checkout.ActionProgression {
			if _, err := tx.ExecContext(ctx, `UPDATE membership_payment_stages
          SET state='progression_permitted',progression_permitted_at=?,page_fingerprint=?,
              page_permit_kind='progression',page_control_id=?,page_ready_at=?,page_facts_json=?,updated_at=?
          WHERE id=?`, at, action.PageFingerprint, action.ControlID, at, string(facts), at, stage.ID); err != nil {
				return err
			}
		} else {
			if _, err := tx.ExecContext(ctx, `UPDATE membership_payment_stages
          SET state='submit_permitted',submit_permitted_at=?,page_fingerprint=?,page_permit_kind='submit',
              page_control_id=?,page_ready_at=?,page_facts_json=?,updated_at=? WHERE id=?`, at,
				action.PageFingerprint, action.ControlID, at, string(facts), at, stage.ID); err != nil {
				return err
			}
			next := "PLUS_SUBMIT_PERMITTED"
			if action.Stage == "upgrade" {
				next = "UPGRADE_SUBMIT_PERMITTED"
			}
			if _, err := transitionWithTx(ctx, tx, fulfillment.ID, next, nowUTC(p), transitionOptions{CurrentStage: pointer(action.Stage)}); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE membership_fulfillments SET money_boundary_at=COALESCE(money_boundary_at,?) WHERE id=?`, at, fulfillment.ID); err != nil {
				return err
			}
		}
		permit.ID = permitID
		return nil
	})
	if err != nil {
		return permit, err
	}
	return permit, logicalErr
}

func checkoutAuthorize(ctx context.Context, tx *sql.Tx, fulfillment Fulfillment, stage checkoutStage, action checkout.ActionKind, now time.Time) (string, string, error) {
	var enabled int
	var rollout string
	if err := tx.QueryRowContext(ctx, `SELECT enabled,rollout_mode FROM membership_fulfillment_settings WHERE id='default'`).Scan(&enabled, &rollout); err != nil {
		return "", "", err
	}
	if enabled != 1 || !fulfillment.RunMode.Valid || fulfillment.RunMode.String != rollout {
		return "", "", coded("MEMBERSHIP_PAYMENT_GATE_LOCKED", "membership payment gate is locked")
	}
	if rollout == "automatic" {
		var id, state, tier, adapterVersion, priceContractID string
		err := tx.QueryRowContext(ctx, `SELECT q.id,q.state,s.tier,s.adapter_version,s.price_contract_id
      FROM automatic_checkout_quota_reservations q JOIN automatic_checkout_scopes s ON s.id=q.scope_id
	  WHERE q.fulfillment_id=?`, fulfillment.ID).Scan(&id, &state, &tier, &adapterVersion, &priceContractID)
		if errors.Is(err, sql.ErrNoRows) {
			return "", "", coded("AUTOMATIC_QUOTA_MISSING", "automatic checkout quota reservation is missing")
		}
		if err != nil {
			return "", "", err
		}
		contractMismatch := (stage.StageKey == "upgrade" || fulfillment.TargetTier == "plus") && priceContractID != stage.PriceContractID
		if state != "reserved" || tier != fulfillment.TargetTier || adapterVersion != paymentAdapterVersion || contractMismatch {
			return "", "", coded("AUTOMATIC_QUOTA_STATE_INVALID", "automatic checkout quota reservation is invalid")
		}
		return "automatic", id, nil
	}
	if rollout != "canary" {
		return "", "", coded("MEMBERSHIP_PAYMENT_GATE_LOCKED", "membership payment gate mode is invalid")
	}
	var id, state, snapshot, approvedAt string
	err := tx.QueryRowContext(ctx, `SELECT id,state,snapshot_fingerprint,approved_at FROM live_canary_authorizations
    WHERE fulfillment_id=? AND stage_key=? AND target_tier=? AND card_id=?
      AND price_contract_id=? AND adapter_version=? AND state IN ('approved','consumed')
    ORDER BY approved_at DESC LIMIT 1`, fulfillment.ID, stage.StageKey, fulfillment.TargetTier,
		stage.CardID.String, stage.PriceContractID, paymentAdapterVersion).Scan(&id, &state, &snapshot, &approvedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", coded("CANARY_AUTHORIZATION_REQUIRED", "live canary authorization is required")
	}
	if err != nil {
		return "", "", err
	}
	approvedTime, parseErr := time.Parse(time.RFC3339Nano, approvedAt)
	if parseErr != nil {
		return "", "", coded("CANARY_AUTHORIZATION_INVALID", "live canary approval time is invalid")
	}
	if state == "approved" && now.After(approvedTime.Add(15*time.Minute)) {
		if _, err := tx.ExecContext(ctx, `UPDATE live_canary_authorizations
	  SET state='expired',invalidated_at=COALESCE(invalidated_at,?) WHERE id=? AND state='approved'`, store.ISO(now), id); err != nil {
			return "", "", err
		}
		return "", "", coded("CANARY_AUTHORIZATION_REQUIRED", "live canary authorization expired")
	}
	var currentFingerprint string
	if err := tx.QueryRowContext(ctx, `SELECT page_fingerprint FROM membership_payment_stages WHERE id=?`, stage.ID).Scan(&currentFingerprint); err != nil {
		return "", "", err
	}
	if snapshot != currentFingerprint {
		var clearProgression int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM membership_action_permits
      WHERE fulfillment_id=? AND stage_key=? AND action_type='progression'
        AND state='reported' AND outcome_code='AUTHORIZATION_CLEAR' AND issued_at>=?`, fulfillment.ID,
			stage.StageKey, approvedAt).Scan(&clearProgression); err != nil {
			return "", "", err
		}
		if clearProgression == 0 {
			return "", "", coded("CANARY_PAGE_SNAPSHOT_STALE", "live canary page snapshot changed")
		}
	}
	if state == "approved" {
		result, err := tx.ExecContext(ctx, `UPDATE live_canary_authorizations
      SET state='consumed',consumed_at=? WHERE id=? AND state='approved'`, store.ISO(now), id)
		if err != nil {
			return "", "", err
		}
		if changed, _ := result.RowsAffected(); changed != 1 {
			return "", "", coded("CANARY_AUTHORIZATION_CONFLICT", "live canary authorization changed")
		}
	}
	_ = action
	return "canary", id, nil
}

func (g *checkoutGuard) AfterAction(ctx context.Context, action checkout.Action, permit checkout.Permit) (checkout.ActionOutcome, error) {
	p := g.processor
	if permit.ID == "" {
		return checkout.ActionOutcome{}, coded("ACTION_PERMIT_BINDING_MISMATCH", "checkout action permit is missing")
	}
	if action.Kind == checkout.ActionSubmit {
		return checkout.ActionOutcome{Continue: false}, nil
	}
	transactions, err := p.loadAllTransactions(ctx, g.client, g.execution.Stage.UpstreamCardID.Int64)
	if err != nil {
		return checkout.ActionOutcome{}, err
	}
	if err := p.assertWorkAllowed(ctx); err != nil {
		return checkout.ActionOutcome{}, err
	}
	current := make([]string, 0, len(transactions))
	for _, item := range transactions {
		current = append(current, item.AuthID)
	}
	outcome := checkout.ActionOutcome{}
	err = p.withFencedImmediate(ctx, func(tx *sql.Tx) error {
		return checkoutReportProgression(ctx, tx, g.execution, action, permit, nowUTC(p), func(before map[string]bool) error {
			newAuthorization := false
			for _, authID := range current {
				if !before[authID] {
					newAuthorization = true
					break
				}
			}
			if newAuthorization {
				at := store.ISO(nowUTC(p))
				if _, err := tx.ExecContext(ctx, `UPDATE membership_action_permits
          SET state='blocked',reported_at=?,outcome_code='UNEXPECTED_PREAUTH' WHERE id=?`, at, permit.ID); err != nil {
					return err
				}
				fulfillment, err := loadFulfillment(ctx, tx, g.execution.Fulfillment.ID)
				if err != nil {
					return err
				}
				updated, err := transitionWithTx(ctx, tx, fulfillment.ID, "UNEXPECTED_PREAUTH", nowUTC(p), transitionOptions{
					CurrentStage: pointer(action.Stage), FailureCode: pointer("UNEXPECTED_PREAUTH"),
				})
				if err != nil {
					return err
				}
				outcome.Continue = false
				return insertCheckoutIntervention(ctx, tx, updated, "UNEXPECTED_PREAUTH", nowUTC(p))
			}
			if _, err := tx.ExecContext(ctx, `UPDATE membership_action_permits
        SET state='reported',reported_at=?,outcome_code='AUTHORIZATION_CLEAR' WHERE id=?`, store.ISO(nowUTC(p)), permit.ID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE membership_payment_stages
        SET state='checkout_ready',progression_reported_at=?,updated_at=? WHERE id=?`, store.ISO(nowUTC(p)), store.ISO(nowUTC(p)), g.execution.Stage.ID); err != nil {
				return err
			}
			outcome.Continue = true
			return nil
		})
	})
	return outcome, err
}

func (g *checkoutGuard) ActivateAction(ctx context.Context, action checkout.Action, permit checkout.Permit) error {
	return g.processor.withFencedImmediate(ctx, func(tx *sql.Tx) error {
		return checkoutActivatePermit(ctx, tx, g.execution, action, permit, nowUTC(g.processor))
	})
}

func checkoutActivatePermit(ctx context.Context, tx *sql.Tx, execution checkoutExecution, action checkout.Action,
	permit checkout.Permit, now time.Time) error {
	var state, actionType, stageKey string
	var attemptNo int64
	err := tx.QueryRowContext(ctx, `SELECT state,action_type,stage_key,attempt_no FROM membership_action_permits WHERE id=?`, permit.ID).
		Scan(&state, &actionType, &stageKey, &attemptNo)
	if err != nil || state != "issued" || actionType != string(action.Kind) || stageKey != action.Stage || attemptNo != execution.AttemptNo {
		return coded("ACTION_PERMIT_BINDING_MISMATCH", "checkout action permit binding changed")
	}
	at := store.ISO(now)
	if _, err := tx.ExecContext(ctx, `UPDATE membership_action_permits SET state='activated',activated_at=? WHERE id=? AND state='issued'`, at, permit.ID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE membership_fulfillments
    SET money_boundary_at=COALESCE(money_boundary_at,?),updated_at=? WHERE id=?`, at, at, execution.Fulfillment.ID); err != nil {
		return err
	}
	if action.Kind == checkout.ActionProgression {
		return nil
	}
	next := "PLUS_RECONCILING"
	if action.Stage == "upgrade" {
		next = "UPGRADE_RECONCILING"
	}
	if _, err := tx.ExecContext(ctx, `UPDATE membership_payment_stages
    SET state='reconciling',submit_reported_at=?,updated_at=? WHERE id=?`, at, at, execution.Stage.ID); err != nil {
		return err
	}
	_, err = transitionWithTx(ctx, tx, execution.Fulfillment.ID, next, now, transitionOptions{CurrentStage: pointer(action.Stage)})
	return err
}

func checkoutReportProgression(ctx context.Context, tx *sql.Tx, execution checkoutExecution, action checkout.Action,
	permit checkout.Permit, now time.Time, progression func(map[string]bool) error) error {
	var state, actionType, stageKey string
	var attemptNo int64
	err := tx.QueryRowContext(ctx, `SELECT state,action_type,stage_key,attempt_no FROM membership_action_permits WHERE id=?`, permit.ID).
		Scan(&state, &actionType, &stageKey, &attemptNo)
	if err != nil || state != "activated" || actionType != string(checkout.ActionProgression) ||
		stageKey != action.Stage || attemptNo != execution.AttemptNo {
		return coded("ACTION_PERMIT_BINDING_MISMATCH", "checkout progression permit binding changed")
	}
	if action.Kind == checkout.ActionProgression {
		before := map[string]bool{}
		rows, err := tx.QueryContext(ctx, `SELECT auth_id FROM membership_action_auth_snapshots WHERE permit_id=?`, permit.ID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			before[id] = true
		}
		if err := rows.Close(); err != nil {
			return err
		}
		return progression(before)
	}
	return coded("CHECKOUT_ACTION_INVALID", "checkout progression action is invalid")
}

func (p *Processor) checkoutMarkUncertain(ctx context.Context, execution checkoutExecution, browserError *checkout.Error, now time.Time) error {
	if browserError.Permit == nil || browserError.Action == nil {
		return coded("PERMIT_OUTCOME_UNCERTAIN", "uncertain checkout action is missing its durable permit")
	}
	return p.withFencedImmediate(ctx, func(tx *sql.Tx) error {
		state, reason, permitState := "PAYMENT_OUTCOME_UNCERTAIN", browserError.ErrorCode, "outcome_uncertain"
		if browserError.Action.Kind == checkout.ActionProgression {
			state, reason, permitState = "UNEXPECTED_PREAUTH", "PROGRESSION_OUTCOME_UNKNOWN", "blocked"
		}
		at := store.ISO(now)
		if _, err := tx.ExecContext(ctx, `UPDATE membership_action_permits
      SET state=?,reported_at=COALESCE(reported_at,?),outcome_code=? WHERE id=?`, permitState, at, reason, browserError.Permit.ID); err != nil {
			return err
		}
		fulfillment, err := loadFulfillment(ctx, tx, execution.Fulfillment.ID)
		if err != nil {
			return err
		}
		updated, err := transitionWithTx(ctx, tx, fulfillment.ID, state, now, transitionOptions{
			CurrentStage: pointer(execution.Stage.StageKey), FailureCode: pointer(reason),
		})
		if err != nil {
			return err
		}
		return insertCheckoutIntervention(ctx, tx, updated, reason, now)
	})
}

func nowUTC(p *Processor) time.Time { return p.now().UTC() }

var _ checkout.ActionGuard = (*checkoutGuard)(nil)

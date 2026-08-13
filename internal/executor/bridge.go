package executor

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"kwmembership/internal/checkout"
	"kwmembership/internal/store"
)

const AdapterVersion = "python-session-card-checkout-v1"

var safeCode = regexp.MustCompile(`^[A-Z0-9_]{1,80}$`)

type executionResult struct {
	result checkout.Result
	err    error
}

type pendingAction struct {
	action    checkout.Action
	permit    checkout.Permit
	activated bool
	reported  bool
}

type pendingExecution struct {
	request          checkout.Request
	actions          map[string]*pendingAction
	progressionCount int
	finished         bool
	outcome          executionResult
}

type Bridge struct {
	db     *sql.DB
	queue  *Queue
	secret string
	now    func() time.Time

	mu      sync.Mutex
	pending map[string]*pendingExecution
}

func NewBridge(db *sql.DB, secret string) (*Bridge, error) {
	secret = strings.TrimSpace(secret)
	if db == nil || len(secret) < 32 || strings.ContainsAny(secret, "\r\n\t ") {
		return nil, fmt.Errorf("invalid Python executor bridge configuration")
	}
	bridge := &Bridge{db: db, queue: NewQueue(db), secret: secret, now: time.Now, pending: map[string]*pendingExecution{}}
	// Sensitive execution context is deliberately memory-only. A queued command
	// can be rebuilt by the next Go Execute call because Python never leased it;
	// leased or action-required work cannot prove that and must reconcile.
	now := store.ISO(bridge.now().UTC())
	if _, err := db.Exec(`UPDATE membership_checkout_commands
    SET state='expired',outcome_code=COALESCE(outcome_code,'EXECUTOR_CONTEXT_LOST'),
        ended_at=COALESCE(ended_at,?),updated_at=?
	    WHERE state IN ('leased','action_required')`, now, now); err != nil {
		return nil, err
	}
	return bridge, nil
}

func (b *Bridge) Execute(ctx context.Context, request checkout.Request) (checkout.Result, error) {
	binding := request.Binding
	if binding.FulfillmentID == "" || binding.AttemptNo < 0 || binding.AdapterVersion != AdapterVersion ||
		(request.Mode != checkout.ModeSessionPreflight && request.Mode != checkout.ModeSessionCheckout) ||
		len(request.Session) == 0 || request.ExpectedEmail == "" {
		return checkout.Result{}, bridgeError("CHECKOUT_REQUEST_INVALID", "Python executor binding is invalid")
	}
	kind := "payment"
	if request.Mode == checkout.ModeSessionPreflight {
		kind = "preflight"
	}
	priority := Priority(binding.PriorityClass)
	if !validPriority(priority) {
		priority = PriorityNormal
	}
	command, err := b.queue.GetForAttempt(ctx, binding.FulfillmentID, request.Stage, binding.AttemptNo, kind)
	if errors.Is(err, ErrNoCommand) {
		id, idErr := store.NewID("mcc_")
		if idErr != nil {
			return checkout.Result{}, idErr
		}
		now := b.now().UTC()
		command, err = b.queue.Enqueue(ctx, CommandInput{
			ID: id, FulfillmentID: binding.FulfillmentID, StageKey: request.Stage, AttemptNo: binding.AttemptNo,
			CommandKind: kind, Priority: priority, TargetTier: request.TargetTier, AdapterVersion: binding.AdapterVersion,
			PriceContractID: request.PriceContract.ID, PriceContractVersion: int64(request.PriceContract.Version),
			FulfillmentRevision: binding.FulfillmentRevision, AvailableAt: now, PaymentReadyAt: now, CreatedAt: now,
		})
	}
	if err != nil {
		return checkout.Result{}, err
	}
	if command.FulfillmentRevision != binding.FulfillmentRevision || command.AdapterVersion != binding.AdapterVersion ||
		command.PriceContractID != request.PriceContract.ID || command.PriceContractVersion != int64(request.PriceContract.Version) {
		return checkout.Result{}, bridgeError("EXECUTOR_COMMAND_CONFLICT", "checkout command binding changed")
	}
	b.mu.Lock()
	pending := b.pending[command.ID]
	if pending == nil && command.State == "queued" {
		pending = &pendingExecution{request: request, actions: map[string]*pendingAction{}}
		b.pending[command.ID] = pending
	}
	if pending != nil && pending.finished {
		outcome := pending.outcome
		delete(b.pending, command.ID)
		b.mu.Unlock()
		return outcome.result, outcome.err
	}
	b.mu.Unlock()
	if pending == nil {
		code := command.OutcomeCode
		if !safeCode.MatchString(code) {
			code = "EXECUTOR_CONTEXT_LOST"
		}
		return checkout.Result{}, bridgeError(code, "Python checkout execution context was lost")
	}
	switch command.State {
	case "queued", "leased", "action_required":
		return checkout.Result{}, bridgeError("EXECUTOR_PENDING", "Python checkout execution is pending")
	case "expired", "cancelled":
		code := command.OutcomeCode
		if !safeCode.MatchString(code) {
			code = "EXECUTOR_CONTEXT_LOST"
		}
		return checkout.Result{}, bridgeError(code, "Python checkout execution became unavailable")
	default:
		return checkout.Result{}, bridgeError("EXECUTOR_CONTEXT_LOST", "Python checkout result is unavailable")
	}
}

func (b *Bridge) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /internal/v1/executions/lease", b.handleLease)
	mux.HandleFunc("POST /internal/v1/executions/{id}/heartbeat", b.handleHeartbeat)
	mux.HandleFunc("POST /internal/v1/executions/{id}/material", b.handleMaterial)
	mux.HandleFunc("POST /internal/v1/executions/{id}/page-facts", b.handlePageFacts)
	mux.HandleFunc("POST /internal/v1/executions/{id}/actions/prepare", b.handlePrepareAction)
	mux.HandleFunc("POST /internal/v1/executions/{id}/actions/{permit}/activate", b.handleActivateAction)
	mux.HandleFunc("POST /internal/v1/executions/{id}/actions/{permit}/result", b.handleActionResult)
	mux.HandleFunc("POST /internal/v1/executions/{id}/handoff", b.handleHandoff)
	mux.HandleFunc("POST /internal/v1/executions/{id}/result", b.handleResult)
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Cache-Control", "no-store")
		response.Header().Set("Content-Type", "application/json")
		if !b.authenticated(request) {
			writeJSON(response, http.StatusUnauthorized, map[string]any{"code": "EXECUTOR_UNAUTHORIZED"})
			return
		}
		mux.ServeHTTP(response, request)
	})
}

func (b *Bridge) authenticated(request *http.Request) bool {
	want := "Bearer " + b.secret
	got := request.Header.Get("Authorization")
	return len(got) == len(want) && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func (b *Bridge) handleLease(response http.ResponseWriter, request *http.Request) {
	var body struct {
		ExecutorID string `json:"executorId"`
	}
	if !decodeJSON(response, request, &body) || !safeIdentity(body.ExecutorID) {
		return
	}
	lease, err := b.queue.LeaseNext(request.Context(), body.ExecutorID, b.now().UTC())
	if errors.Is(err, ErrNoCommand) {
		response.WriteHeader(http.StatusNoContent)
		return
	}
	if err != nil {
		writeBridgeError(response, err)
		return
	}
	b.mu.Lock()
	pending := b.pending[lease.Command.ID]
	b.mu.Unlock()
	if pending == nil {
		b.expireCommand(lease.Command.ID, "EXECUTOR_CONTEXT_LOST")
		writeJSON(response, http.StatusConflict, map[string]any{"code": "EXECUTOR_CONTEXT_LOST"})
		return
	}
	writeJSON(response, http.StatusOK, commandResponse(lease, pending.request))
}

func commandResponse(lease Lease, request checkout.Request) map[string]any {
	return map[string]any{
		"executionId": lease.Command.ID, "executorId": lease.Command.LeasedBy,
		"leaseEpoch": lease.Command.LeaseEpoch, "leaseToken": lease.Token,
		"hardDeadlineAt": lease.Command.HardDeadlineAt, "commandKind": lease.Command.CommandKind,
		"stage": lease.Command.StageKey, "attemptNo": lease.Command.AttemptNo,
		"targetTier": lease.Command.TargetTier, "adapterVersion": lease.Command.AdapterVersion,
		"priceContract": request.PriceContract,
	}
}

func (b *Bridge) handleHeartbeat(response http.ResponseWriter, request *http.Request) {
	lease, ok := b.requestLease(response, request)
	if !ok {
		return
	}
	command, err := b.queue.Heartbeat(request.Context(), lease, b.now().UTC())
	if err != nil {
		writeBridgeError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{"leaseExpiresAt": command.LeaseExpiresAt, "hardDeadlineAt": command.HardDeadlineAt})
}

func (b *Bridge) handleMaterial(response http.ResponseWriter, request *http.Request) {
	lease, pending, ok := b.requestPending(response, request)
	if !ok {
		return
	}
	now := b.now().UTC()
	_, err := b.queue.ClaimMaterialWith(request.Context(), lease, now, func(tx *sql.Tx) error {
		return validateMaterialClaim(request.Context(), tx, lease.Command, pending.request, now)
	})
	if err != nil {
		writeBridgeError(response, err)
		return
	}
	payload := map[string]any{
		"session": json.RawMessage(pending.request.Session), "expectedEmail": pending.request.ExpectedEmail,
		"targetTier": pending.request.TargetTier, "stage": pending.request.Stage,
		"priceContract": pending.request.PriceContract,
		"checkoutContract": map[string]any{
			"country": "PH", "currency": "PHP", "uiMode": "hosted",
			"entryPoint":     "all_plans_pricing_modal",
			"allowedOrigins": []string{"https://chatgpt.com", "https://pay.openai.com"},
		},
	}
	if pending.request.Material != nil {
		payload["card"] = pending.request.Material.Card
		payload["billing"] = pending.request.Material.Billing
	}
	writeJSON(response, http.StatusOK, payload)
}

func validateMaterialClaim(ctx context.Context, tx *sql.Tx, command Command, request checkout.Request, now time.Time) error {
	var state, runMode string
	var revision int64
	var reservation sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT state,COALESCE(run_mode,''),state_revision,card_reservation_id
    FROM membership_fulfillments WHERE id=?`, command.FulfillmentID).Scan(&state, &runMode, &revision, &reservation); err != nil {
		return err
	}
	if revision != command.FulfillmentRevision {
		return ErrCommandConflict
	}
	if command.CommandKind == "preflight" {
		if state != "CHECKOUT_PREFLIGHT_READY" && state != "CHECKOUT_CHALLENGE_WAIT" {
			return ErrCommandConflict
		}
		return nil
	}
	if state != "CHECKOUT_EXECUTION_WAIT" || !reservation.Valid || reservation.String == "" || request.Material == nil {
		return ErrCommandConflict
	}
	var enabled int
	var rollout string
	if err := tx.QueryRowContext(ctx, `SELECT enabled,rollout_mode FROM membership_fulfillment_settings WHERE id='default'`).Scan(&enabled, &rollout); err != nil {
		return err
	}
	if enabled != 1 || (runMode != "canary" && runMode != "automatic") || runMode != rollout {
		return bridgeError("MEMBERSHIP_PAYMENT_GATE_LOCKED", "membership payment gate is locked")
	}
	var attempt int64
	var adapter, contractID, stageState, cardID string
	if err := tx.QueryRowContext(ctx, `SELECT attempt_no,adapter_version,price_contract_id,state,card_id
    FROM membership_payment_stages WHERE fulfillment_id=? AND stage_key=?`, command.FulfillmentID, command.StageKey).
		Scan(&attempt, &adapter, &contractID, &stageState, &cardID); err != nil {
		return err
	}
	if attempt != command.AttemptNo || adapter != command.AdapterVersion || contractID != command.PriceContractID ||
		stageState != "checkout_ready" || cardID == "" {
		return ErrCommandConflict
	}
	var reservationState, reservedCard string
	if err := tx.QueryRowContext(ctx, `SELECT state,COALESCE(card_id,'') FROM card_capacity_reservations WHERE id=?`, reservation.String).
		Scan(&reservationState, &reservedCard); err != nil {
		return err
	}
	if reservationState != "reserved" || reservedCard != cardID {
		return ErrCommandConflict
	}
	var unsafeSubmit int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM membership_action_permits
    WHERE fulfillment_id=? AND stage_key=? AND attempt_no=? AND action_type='submit'
      AND state IN ('activated','outcome_uncertain','challenge_locked')`, command.FulfillmentID, command.StageKey, command.AttemptNo).Scan(&unsafeSubmit); err != nil {
		return err
	}
	if unsafeSubmit != 0 {
		return ErrCommandConflict
	}
	_, err := tx.ExecContext(ctx, `UPDATE membership_fulfillments
    SET money_boundary_at=COALESCE(money_boundary_at,?),updated_at=? WHERE id=?`, store.ISO(now), store.ISO(now), command.FulfillmentID)
	return err
}

func (b *Bridge) handlePageFacts(response http.ResponseWriter, request *http.Request) {
	_, pending, ok := b.requestPending(response, request)
	if !ok {
		return
	}
	var body struct {
		Page checkout.PageFacts `json:"page"`
	}
	if !decodeJSON(response, request, &body) {
		return
	}
	page, err := validatePageFacts(body.Page, pending.request)
	if err != nil {
		writeBridgeError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{"page": page})
}

func (b *Bridge) handlePrepareAction(response http.ResponseWriter, request *http.Request) {
	_, pending, ok := b.requestPending(response, request)
	if !ok {
		return
	}
	var body struct {
		Kind      checkout.ActionKind `json:"kind"`
		ControlID string              `json:"controlId"`
		Page      checkout.PageFacts  `json:"page"`
	}
	if !decodeJSON(response, request, &body) {
		return
	}
	if pending.request.Guard == nil || (body.Kind != checkout.ActionProgression && body.Kind != checkout.ActionSubmit) {
		writeJSON(response, http.StatusConflict, map[string]any{"code": "CHECKOUT_ACTION_INVALID"})
		return
	}
	page, err := validatePageFacts(body.Page, pending.request)
	if err != nil {
		writeBridgeError(response, err)
		return
	}
	expected := expectedActionControl(page, pending.request, body.Kind)
	if expected == "" || expected != body.ControlID {
		writeJSON(response, http.StatusConflict, map[string]any{"code": "CHECKOUT_ACTION_INVALID"})
		return
	}
	action := checkout.Action{Kind: body.Kind, Stage: pending.request.Stage, ControlID: body.ControlID, PageFingerprint: page.StructuralHash, Page: page}
	permit, err := pending.request.Guard.BeforeAction(request.Context(), action)
	if err != nil {
		writeBridgeError(response, err)
		return
	}
	b.mu.Lock()
	pending.actions[permit.ID] = &pendingAction{action: action, permit: permit}
	b.mu.Unlock()
	writeJSON(response, http.StatusOK, map[string]any{"permitId": permit.ID, "controlId": action.ControlID})
}

// Upgrade selection uses a plan-specific button before the hosted payment
// page exists. It is still a progression action, but must bind to the target
// tier rather than the generic payment-next control.
func expectedActionControl(page checkout.PageFacts, request checkout.Request, kind checkout.ActionKind) string {
	if kind != checkout.ActionProgression || page.StateID != "UPGRADE_SELECTION_READY" {
		return page.Controls[string(kind)]
	}
	if request.TargetTier == "x5" {
		return page.Controls["upgradeX5"]
	}
	if request.TargetTier == "x20" {
		return page.Controls["upgradeX20"]
	}
	return ""
}

func (b *Bridge) handleActivateAction(response http.ResponseWriter, request *http.Request) {
	lease, pending, ok := b.requestPending(response, request)
	if !ok {
		return
	}
	action, ok := b.pendingAction(response, pending, request.PathValue("permit"))
	if !ok {
		return
	}
	b.mu.Lock()
	activated, reported := action.activated, action.reported
	b.mu.Unlock()
	if activated || reported {
		writeJSON(response, http.StatusConflict, map[string]any{"code": "ACTION_PERMIT_BINDING_MISMATCH"})
		return
	}
	if err := pending.request.Guard.ActivateAction(request.Context(), action.action, action.permit); err != nil {
		b.finishUncertain(lease, pending, action, "PERMIT_ACTIVATION_UNCERTAIN", err)
		writeBridgeError(response, err)
		return
	}
	b.mu.Lock()
	action.activated = true
	b.mu.Unlock()
	writeJSON(response, http.StatusOK, map[string]any{"activated": true})
}

func (b *Bridge) handleActionResult(response http.ResponseWriter, request *http.Request) {
	lease, pending, ok := b.requestPending(response, request)
	if !ok {
		return
	}
	action, ok := b.pendingAction(response, pending, request.PathValue("permit"))
	if !ok {
		return
	}
	var body struct {
		Outcome string `json:"outcome"`
	}
	if !decodeJSON(response, request, &body) {
		return
	}
	b.mu.Lock()
	activated, reported := action.activated, action.reported
	b.mu.Unlock()
	if !activated || reported {
		writeJSON(response, http.StatusConflict, map[string]any{"code": "ACTION_PERMIT_BINDING_MISMATCH"})
		return
	}
	if body.Outcome != "clicked" {
		b.finishUncertain(lease, pending, action, "PAYMENT_OUTCOME_UNCERTAIN", nil)
		writeJSON(response, http.StatusAccepted, map[string]any{"continue": false})
		return
	}
	outcome, err := pending.request.Guard.AfterAction(request.Context(), action.action, action.permit)
	if err != nil {
		b.finishUncertain(lease, pending, action, "PAYMENT_OUTCOME_UNCERTAIN", err)
		writeBridgeError(response, err)
		return
	}
	b.mu.Lock()
	action.reported = true
	if action.action.Kind == checkout.ActionProgression {
		pending.progressionCount++
	}
	b.mu.Unlock()
	writeJSON(response, http.StatusOK, map[string]any{"continue": outcome.Continue})
}

func (b *Bridge) handleHandoff(response http.ResponseWriter, request *http.Request) {
	lease, pending, ok := b.requestPending(response, request)
	if !ok {
		return
	}
	var body struct {
		Type       string             `json:"type"`
		Page       checkout.PageFacts `json:"page"`
		Diagnostic map[string]string  `json:"diagnostic"`
	}
	if !decodeJSON(response, request, &body) || !safeHandoff(body.Type) {
		return
	}
	page, err := validateHandoffFacts(body.Page, pending.request)
	if err != nil {
		writeBridgeError(response, err)
		return
	}
	if pending.request.OnHandoff != nil {
		if err := pending.request.OnHandoff(request.Context(), checkout.Handoff{Type: body.Type, Page: page}); err != nil {
			writeBridgeError(response, err)
			return
		}
	}
	if err := b.queue.SetActionRequired(request.Context(), lease, sanitizeDiagnostic(body.Diagnostic), b.now().UTC()); err != nil {
		writeBridgeError(response, err)
		return
	}
	writeJSON(response, http.StatusAccepted, map[string]any{"accepted": true})
}

func (b *Bridge) handleResult(response http.ResponseWriter, request *http.Request) {
	lease, pending, ok := b.requestPending(response, request)
	if !ok {
		return
	}
	var body struct {
		Status     string             `json:"status"`
		ErrorCode  string             `json:"errorCode"`
		Page       checkout.PageFacts `json:"page"`
		Diagnostic map[string]string  `json:"diagnostic"`
	}
	if !decodeJSON(response, request, &body) {
		return
	}
	diagnostic := sanitizeDiagnostic(body.Diagnostic)
	if body.Status == "success" {
		page, err := validatePageFacts(body.Page, pending.request)
		if err != nil {
			writeBridgeError(response, err)
			return
		}
		if err := b.queue.Report(request.Context(), lease, "EXECUTOR_REPORTED", diagnostic, b.now().UTC()); err != nil {
			writeBridgeError(response, err)
			return
		}
		b.finish(pending, executionResult{result: checkout.Result{Page: page, ProgressionCount: b.progressionCount(pending)}})
		writeJSON(response, http.StatusAccepted, map[string]any{"accepted": true})
		return
	}
	if body.Status == "challenge" {
		page, err := validateHandoffFacts(body.Page, pending.request)
		if err != nil {
			writeBridgeError(response, err)
			return
		}
		if err := b.queue.Report(request.Context(), lease, "PAYMENT_ACTION_REQUIRED", diagnostic, b.now().UTC()); err != nil {
			writeBridgeError(response, err)
			return
		}
		b.finish(pending, executionResult{result: checkout.Result{Page: page, Challenge: true, ProgressionCount: b.progressionCount(pending)}})
		writeJSON(response, http.StatusAccepted, map[string]any{"accepted": true})
		return
	}
	if body.Status != "failed" || !safeCode.MatchString(body.ErrorCode) {
		writeJSON(response, http.StatusBadRequest, map[string]any{"code": "EXECUTOR_RESULT_INVALID"})
		return
	}
	if err := b.queue.Report(request.Context(), lease, body.ErrorCode, diagnostic, b.now().UTC()); err != nil {
		writeBridgeError(response, err)
		return
	}
	b.finish(pending, executionResult{err: bridgeError(body.ErrorCode, "Python checkout executor reported failure")})
	writeJSON(response, http.StatusAccepted, map[string]any{"accepted": true})
}

func validatePageFacts(page checkout.PageFacts, request checkout.Request) (checkout.PageFacts, error) {
	if page.Origin != "https://chatgpt.com" && page.Origin != "https://pay.openai.com" {
		return checkout.PageFacts{}, bridgeError("CHECKOUT_CONTEXT_INVALID", "checkout origin is outside the allowlist")
	}
	allowedRoute := map[string]bool{"/checkout": true, "/checkout/{id}": true, "/pay/{id}": true, "/settings/subscription": true, "/settings/billing": true, "/account/billing/overview": true}
	if !allowedRoute[page.RouteTemplate] || page.Country != "PH" || page.Currency != "PHP" || page.DisplayedAmount == nil ||
		*page.DisplayedAmount < request.PriceContract.MinAmount || *page.DisplayedAmount > request.PriceContract.MaxAmount {
		return checkout.PageFacts{}, bridgeError("CHECKOUT_PAGE_CONTRACT_INVALID", "checkout page contract is invalid")
	}
	expectedPlan := "plus"
	if request.Stage == "upgrade" {
		expectedPlan = map[string]string{"x5": "prolite", "x20": "pro"}[request.TargetTier]
	}
	if page.Plan != expectedPlan || !map[string]bool{"PAYMENT_PROGRESSION_READY": true, "PAYMENT_FINAL_READY": true, "PAYMENT_ACTION_REQUIRED": true, "UPGRADE_SELECTION_READY": true}[page.StateID] {
		return checkout.PageFacts{}, bridgeError("CHECKOUT_PAGE_CONTRACT_INVALID", "checkout page state is invalid")
	}
	allowedControls := map[string]map[string]bool{
		"progression": {"": true, "payment-next": true, "hosted-payment-next": true},
		"submit":      {"": true, "payment-submit": true, "hosted-payment-submit": true},
		"upgradeX5":   {"": true, "upgrade-x5": true}, "upgradeX20": {"": true, "upgrade-x20": true},
		"challenge": {"": true, "challenge-3ds": true, "challenge-captcha": true, "challenge-sms": true, "challenge-bank": true, "challenge-cloudflare": true},
	}
	for key, value := range page.Controls {
		if allowedControls[key] == nil || !allowedControls[key][value] {
			return checkout.PageFacts{}, bridgeError("CHECKOUT_PAGE_CONTRACT_INVALID", "checkout control is invalid")
		}
	}
	page.StructuralHash = pageFingerprint(page)
	return page, nil
}

func validateHandoffFacts(page checkout.PageFacts, request checkout.Request) (checkout.PageFacts, error) {
	if page.StateID != "PAYMENT_ACTION_REQUIRED" ||
		(page.Origin != "https://chatgpt.com" && page.Origin != "https://pay.openai.com") {
		return checkout.PageFacts{}, bridgeError("CHECKOUT_CONTEXT_INVALID", "challenge page context is invalid")
	}
	allowedRoute := map[string]bool{"/checkout": true, "/checkout/{id}": true, "/pay/{id}": true, "/settings/subscription": true, "/settings/billing": true, "/account/billing/overview": true}
	challenge := page.Controls["challenge"]
	if !allowedRoute[page.RouteTemplate] || !map[string]bool{
		"challenge-3ds": true, "challenge-captcha": true, "challenge-sms": true,
		"challenge-bank": true, "challenge-cloudflare": true,
	}[challenge] {
		return checkout.PageFacts{}, bridgeError("CHECKOUT_PAGE_CONTRACT_INVALID", "challenge page contract is invalid")
	}
	completeContract := page.Plan != "" || page.Country != "" || page.Currency != "" || page.DisplayedAmount != nil
	if completeContract {
		return validatePageFacts(page, request)
	}
	for key, value := range page.Controls {
		if key != "challenge" && value != "" {
			return checkout.PageFacts{}, bridgeError("CHECKOUT_PAGE_CONTRACT_INVALID", "challenge page controls are invalid")
		}
	}
	page.StructuralHash = pageFingerprint(page)
	return page, nil
}

func pageFingerprint(page checkout.PageFacts) string {
	copy := page
	copy.StructuralHash = ""
	raw, _ := json.Marshal(copy)
	digest := fmt.Sprintf("%x", sha256Sum(raw))
	return digest
}

func sha256Sum(value []byte) [32]byte { return sha256.Sum256(value) }

func (b *Bridge) requestLease(response http.ResponseWriter, request *http.Request) (Lease, bool) {
	executorID := request.Header.Get("X-Executor-ID")
	epoch, err := strconv.ParseInt(request.Header.Get("X-Lease-Epoch"), 10, 64)
	if err != nil || !safeIdentity(executorID) {
		writeJSON(response, http.StatusUnauthorized, map[string]any{"code": "EXECUTOR_LEASE_INVALID"})
		return Lease{}, false
	}
	lease, err := b.queue.ValidateLease(request.Context(), request.PathValue("id"), executorID, epoch, request.Header.Get("X-Lease-Token"), b.now().UTC())
	if err != nil {
		writeBridgeError(response, err)
		return Lease{}, false
	}
	return lease, true
}

func (b *Bridge) requestPending(response http.ResponseWriter, request *http.Request) (Lease, *pendingExecution, bool) {
	lease, ok := b.requestLease(response, request)
	if !ok {
		return Lease{}, nil, false
	}
	b.mu.Lock()
	pending := b.pending[lease.Command.ID]
	lost := pending == nil || pending.finished
	b.mu.Unlock()
	if lost {
		writeJSON(response, http.StatusConflict, map[string]any{"code": "EXECUTOR_CONTEXT_LOST"})
		return Lease{}, nil, false
	}
	return lease, pending, true
}

func (b *Bridge) pendingAction(response http.ResponseWriter, pending *pendingExecution, id string) (*pendingAction, bool) {
	b.mu.Lock()
	action := pending.actions[id]
	b.mu.Unlock()
	if action == nil {
		writeJSON(response, http.StatusConflict, map[string]any{"code": "ACTION_PERMIT_BINDING_MISMATCH"})
		return nil, false
	}
	return action, true
}

func (b *Bridge) finishUncertain(lease Lease, pending *pendingExecution, action *pendingAction, code string, cause error) {
	_ = b.queue.Report(context.Background(), lease, code, `{"phase":"action","status":"unknown"}`, b.now().UTC())
	b.finish(pending, executionResult{err: &checkout.Error{ErrorCode: code, Message: "checkout action outcome is uncertain", Cause: cause, Action: &action.action, Permit: &action.permit, Uncertain: true}})
}

func (b *Bridge) finish(pending *pendingExecution, result executionResult) {
	b.mu.Lock()
	if pending.finished {
		b.mu.Unlock()
		return
	}
	pending.finished = true
	pending.outcome = result
	b.mu.Unlock()
}

func (b *Bridge) progressionCount(pending *pendingExecution) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return pending.progressionCount
}

func (b *Bridge) expireCommand(id, code string) {
	now := store.ISO(b.now().UTC())
	_, _ = b.db.Exec(`UPDATE membership_checkout_commands SET state='expired',outcome_code=?,ended_at=COALESCE(ended_at,?),updated_at=?
    WHERE id=? AND state IN ('queued','leased','action_required')`, code, now, now, id)
}

func decodeJSON(response http.ResponseWriter, request *http.Request, target any) bool {
	request.Body = http.MaxBytesReader(response, request.Body, 64<<10)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeJSON(response, http.StatusBadRequest, map[string]any{"code": "EXECUTOR_REQUEST_INVALID"})
		return false
	}
	return true
}

func sanitizeDiagnostic(values map[string]string) string {
	allowed := map[string]bool{"phase": true, "stateId": true, "errorCode": true, "adapter": true, "status": true}
	clean := map[string]string{}
	for key, value := range values {
		value = strings.TrimSpace(value)
		if allowed[key] && len(value) <= 80 && !strings.ContainsAny(value, "\r\n\x00") {
			clean[key] = value
		}
	}
	raw, _ := json.Marshal(clean)
	return string(raw)
}

func safeIdentity(value string) bool {
	if len(value) < 1 || len(value) > 100 {
		return false
	}
	for _, char := range value {
		if !(char >= 'a' && char <= 'z') && !(char >= 'A' && char <= 'Z') && !(char >= '0' && char <= '9') && char != '_' && char != '-' {
			return false
		}
	}
	return true
}

func safeHandoff(value string) bool {
	return value == "cloudflare" || value == "challenge-cloudflare" || value == "captcha" || value == "3ds" || value == "sms" || value == "bank"
}

func bridgeError(code, message string) error {
	return &checkout.Error{ErrorCode: code, Message: message}
}

func writeBridgeError(response http.ResponseWriter, err error) {
	status, code := http.StatusConflict, "EXECUTOR_CONFLICT"
	if errors.Is(err, ErrNoCommand) {
		status, code = http.StatusNoContent, ""
	}
	if errors.Is(err, ErrLeaseExpired) {
		status, code = http.StatusConflict, "EXECUTOR_LEASE_EXPIRED"
	}
	if errors.Is(err, ErrCommandConflict) {
		status, code = http.StatusConflict, "EXECUTOR_COMMAND_CONFLICT"
	}
	var coded interface{ Code() string }
	if errors.As(err, &coded) && safeCode.MatchString(coded.Code()) {
		code = coded.Code()
	}
	if status == http.StatusNoContent {
		response.WriteHeader(status)
		return
	}
	writeJSON(response, status, map[string]any{"code": code})
}

func writeJSON(response http.ResponseWriter, status int, value any) {
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(value)
}

var _ checkout.Executor = (*Bridge)(nil)

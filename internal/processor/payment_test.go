package processor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kwmembership/internal/config"
	"kwmembership/internal/domain"
	"kwmembership/internal/provider"
	"kwmembership/internal/secure"
	"kwmembership/internal/store"
)

type paymentRoundTripper func(*http.Request) (*http.Response, error)

func (roundTrip paymentRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

func TestPaymentFrozenFullCardIsPriceEvidenceButNotReservable(t *testing.T) {
	ctx := context.Background()
	repository, err := store.Open(filepath.Join(t.TempDir(), "frozen-price-evidence.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	if _, err := repository.DB().Exec(`CREATE TABLE managed_cards (
    id TEXT PRIMARY KEY,product_code TEXT,upstream_status TEXT,reconciliation_state TEXT,capacity_state TEXT
  ); CREATE TABLE card_price_signals (
    card_id TEXT,tier TEXT,found INTEGER,amount REAL,min_usd REAL,max_usd REAL,provider_time TEXT
  ); INSERT INTO managed_cards VALUES
    ('card-frozen','product-1','FROZEN','READY','CAPACITY_FULL');
  INSERT INTO card_price_signals VALUES
    ('card-frozen','plus',1,20,15,25,'2026-07-20T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	processor := &Processor{store: repository}
	now := time.Date(2026, 7, 20, 1, 0, 0, 0, time.UTC)
	snapshot, err := processor.paymentLoadProvenProductSnapshot(ctx, "product-1", domain.TierPlus, now)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.TotalCents != 2020 {
		t.Fatalf("snapshot total=%d", snapshot.TotalCents)
	}
	if err := paymentAssertCardReservable(paymentCard{
		UpstreamStatus: "FROZEN", ReconciliationState: "READY", CapacityState: "CAPACITY_FULL",
	}, domain.TierPlus); errorCode(err) != "MEMBERSHIP_CARD_NOT_READY" {
		t.Fatalf("frozen full evidence became reservable: %v", err)
	}
}

func TestPaymentFundingPlanUsesIntegerCents(t *testing.T) {
	snapshot := paymentPriceSnapshot{TargetTier: domain.TierPlus, TotalCents: 2020}
	card := paymentCard{ID: "mc_1", UpstreamCardID: 7, ProductCode: "product_1"}
	live := provider.Card{UpstreamCardID: 7, ProductCode: "product_1", AvailableAmount: 10, Status: "ACTIVE"}
	product := paymentProduct{
		Code: "product_1", RechargeFeeRate: 0.035,
		MinimumAmountCents: 100, MaximumAmountCents: 5000,
	}

	plan, err := paymentPlanExistingCard(snapshot, card, live, product, 5000)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Operation != "recharge" || plan.FundingAmountCents != 1020 || plan.FeeCents != 36 || plan.PlatformDebitCents != 1056 {
		t.Fatalf("unexpected exact-cent funding plan: %+v", plan)
	}
}

func TestPaymentCanonicalFundingBodyMatchesNodeFingerprint(t *testing.T) {
	canonical, err := paymentCanonicalJSON(map[string]any{"card_id": int64(123), "amount": 20.2})
	if err != nil {
		t.Fatal(err)
	}
	if canonical != `{"amount":20.2,"card_id":123}` {
		t.Fatalf("unexpected canonical JSON: %s", canonical)
	}
	if got := paymentFingerprint(canonical); got != "b9976a8127e5a1c059e493c909af5353de1590b764036d05ed90cdd865b0f4d4" {
		t.Fatalf("unexpected cross-runtime fingerprint: %s", got)
	}
	_, replayed, err := paymentDecodeFundingBody(`{"amount":20.2,"card_id":12345678}`, "recharge")
	if err != nil {
		t.Fatal(err)
	}
	if replayed != `{"amount":20.2,"card_id":12345678}` {
		t.Fatalf("large provider card id changed during canonical replay: %s", replayed)
	}
}

func TestPaymentBusinessDateUsesAsiaShanghai(t *testing.T) {
	now := time.Date(2026, 7, 20, 16, 30, 0, 0, time.UTC)
	if got := paymentBusinessDate(now); got != "2026-07-21" {
		t.Fatalf("unexpected Shanghai business date: %s", got)
	}
}

func TestPaymentAcquirePersistsMoneyBoundaryAndBlocksOrdinaryReplay(t *testing.T) {
	ctx := context.Background()
	repository, err := store.Open(filepath.Join(t.TempDir(), "payment.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repository.Close() })
	for _, statement := range []string{
		`CREATE TABLE membership_fulfillments (id TEXT PRIMARY KEY, money_boundary_at TEXT, updated_at TEXT NOT NULL)`,
		`CREATE TABLE managed_cards (
      id TEXT PRIMARY KEY, upstream_card_id INTEGER NOT NULL, vm_card_id TEXT NOT NULL,
      product_code TEXT NOT NULL, upstream_status TEXT NOT NULL, cached_available_amount REAL NOT NULL,
      lane TEXT, consumed_slots INTEGER NOT NULL, capacity_state TEXT NOT NULL, reconciliation_state TEXT NOT NULL
    )`,
		`CREATE TABLE funding_intents (
      id TEXT PRIMARY KEY, fulfillment_id TEXT NOT NULL UNIQUE, operation TEXT NOT NULL,
      target_card_id TEXT, product_code TEXT, amount REAL NOT NULL, fee REAL NOT NULL,
      idempotency_key TEXT NOT NULL, request_fingerprint TEXT NOT NULL,
      request_body_encrypted TEXT NOT NULL, state TEXT NOT NULL, provider_resource_id TEXT,
      created_at TEXT NOT NULL, submitted_at TEXT, resolved_at TEXT
    )`,
	} {
		if _, err := repository.DB().ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	crypt, err := secure.NewDecrypter("payment-test-secret")
	if err != nil {
		t.Fatal(err)
	}
	canonical := `{"amount":20.2,"card_id":12345678}`
	encrypted, err := crypt.Encrypt(canonical)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.DB().ExecContext(ctx, `INSERT INTO membership_fulfillments VALUES ('mf_1',NULL,'2026-07-20T00:00:00.000Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.DB().ExecContext(ctx, `
    INSERT INTO managed_cards VALUES ('mc_1',12345678,'vm_1','product_1','ACTIVE',0,NULL,0,'AVAILABLE','READY')`); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.DB().ExecContext(ctx, `
    INSERT INTO funding_intents VALUES (
      'mfi_1','mf_1','recharge','mc_1',NULL,20.2,0,'kwr:order_1:recharge:v1',?,?,
      'prepared',NULL,'2026-07-20T00:00:00.000Z',NULL,NULL
    )`, paymentFingerprint(canonical), encrypted); err != nil {
		t.Fatal(err)
	}

	processor := &Processor{store: repository, decrypter: crypt}
	now := time.Date(2026, 7, 20, 1, 2, 3, 0, time.UTC)
	acquired, err := processor.paymentAcquireFundingCall(ctx, "mf_1", now)
	if err != nil {
		t.Fatal(err)
	}
	if acquired.Intent.State != "submitted" || acquired.Body.CardID != 12345678 || acquired.Body.AmountCents != 2020 {
		t.Fatalf("unexpected acquired funding call: %+v", acquired)
	}
	var intentState string
	var submittedAt, moneyBoundaryAt string
	if err := repository.DB().QueryRowContext(ctx, `
    SELECT intent.state,intent.submitted_at,fulfillment.money_boundary_at
    FROM funding_intents intent JOIN membership_fulfillments fulfillment ON fulfillment.id=intent.fulfillment_id
    WHERE intent.id='mfi_1'`).Scan(&intentState, &submittedAt, &moneyBoundaryAt); err != nil {
		t.Fatal(err)
	}
	if intentState != "submitted" || submittedAt != store.ISO(now) || moneyBoundaryAt != store.ISO(now) {
		t.Fatalf("money boundary was not atomically persisted: state=%s submitted=%s boundary=%s", intentState, submittedAt, moneyBoundaryAt)
	}
	if _, err := processor.paymentAcquireFundingCall(ctx, "mf_1", now); errorCode(err) != "FUNDING_SUBMISSION_IN_PROGRESS" {
		t.Fatalf("ordinary replay was not blocked: %v", err)
	}
	if _, err := repository.DB().ExecContext(ctx, `UPDATE funding_intents SET state='outcome_unknown' WHERE id='mfi_1'`); err != nil {
		t.Fatal(err)
	}
	if _, err := processor.paymentAcquireFundingCall(ctx, "mf_1", now); errorCode(err) != "FUNDING_RECOVERY_REQUIRED" {
		t.Fatalf("outcome_unknown used ordinary replay: %v", err)
	}
}

func TestPaymentLeaseLossAfterProviderResponseLeavesIntentSubmitted(t *testing.T) {
	ctx := context.Background()
	temporary := t.TempDir()
	repository, err := store.Open(filepath.Join(temporary, "lease-loss.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repository.Close() })
	for _, statement := range []string{
		`CREATE TABLE membership_fulfillments (
      id TEXT PRIMARY KEY, run_mode TEXT, money_boundary_at TEXT, updated_at TEXT NOT NULL
    )`,
		`CREATE TABLE managed_cards (
      id TEXT PRIMARY KEY, upstream_card_id INTEGER NOT NULL, vm_card_id TEXT NOT NULL,
      product_code TEXT NOT NULL, upstream_status TEXT NOT NULL, cached_available_amount REAL NOT NULL,
      lane TEXT, consumed_slots INTEGER NOT NULL, capacity_state TEXT NOT NULL, reconciliation_state TEXT NOT NULL
    )`,
		`CREATE TABLE funding_intents (
      id TEXT PRIMARY KEY, fulfillment_id TEXT NOT NULL UNIQUE, operation TEXT NOT NULL,
      target_card_id TEXT, product_code TEXT, amount REAL NOT NULL, fee REAL NOT NULL,
      idempotency_key TEXT NOT NULL, request_fingerprint TEXT NOT NULL,
      request_body_encrypted TEXT NOT NULL, state TEXT NOT NULL, provider_resource_id TEXT,
      created_at TEXT NOT NULL, submitted_at TEXT, resolved_at TEXT
    )`,
	} {
		if _, err := repository.DB().ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Date(2026, 7, 20, 1, 2, 3, 0, time.UTC)
	if err := repository.EnsureLeaseTable(ctx, now); err != nil {
		t.Fatal(err)
	}
	lease, err := repository.AcquireLease(ctx, "go", "payment-test-token", "test", now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	crypt, err := secure.NewDecrypter("payment-test-secret")
	if err != nil {
		t.Fatal(err)
	}
	canonical := `{"amount":20.2,"card_id":12345678}`
	encrypted, err := crypt.Encrypt(canonical)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.DB().ExecContext(ctx, `INSERT INTO membership_fulfillments VALUES ('mf_1','canary',NULL,?)`, store.ISO(now)); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.DB().ExecContext(ctx, `
    INSERT INTO managed_cards VALUES ('mc_1',12345678,'vm_1','product_1','ACTIVE',0,NULL,0,'AVAILABLE','READY')`); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.DB().ExecContext(ctx, `
    INSERT INTO funding_intents VALUES (
      'mfi_1','mf_1','recharge','mc_1',NULL,20.2,0,'kwr:order_1:recharge:v1',?,?,
      'prepared',NULL,?,NULL,NULL
    )`, paymentFingerprint(canonical), encrypted, store.ISO(now)); err != nil {
		t.Fatal(err)
	}

	transport := paymentRoundTripper(func(request *http.Request) (*http.Response, error) {
		body, readErr := io.ReadAll(request.Body)
		if readErr != nil {
			t.Errorf("read provider request: %v", readErr)
		}
		if string(body) != canonical || request.Header.Get("Idempotency-Key") != "kwr:order_1:recharge:v1" {
			t.Errorf("provider did not receive the immutable request: body=%s key=%s", body, request.Header.Get("Idempotency-Key"))
		}
		if _, updateErr := repository.DB().ExecContext(ctx, `UPDATE membership_processor_lease SET expires_at=? WHERE id='default'`, store.ISO(now)); updateErr != nil {
			t.Errorf("expire processor lease: %v", updateErr)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"code":0,"data":null}`)),
			Request:    request,
		}, nil
	})
	client, err := provider.NewSpaceXClient(&http.Client{Transport: transport}, "app", "secret")
	if err != nil {
		t.Fatal(err)
	}
	processor := &Processor{
		config: config.Config{MaintenancePath: filepath.Join(temporary, "maintenance.json")},
		store:  repository, lease: lease, decrypter: crypt, now: func() time.Time { return now },
	}
	err = processor.paymentSubmitFundingIntent(ctx, client, "mf_1", paymentGate{enabled: true, mode: "canary"}, now)
	if !errors.Is(err, store.ErrLeaseLost) {
		t.Fatalf("expected lease loss after provider response, got %v", err)
	}
	var state string
	var resolvedAt any
	if err := repository.DB().QueryRowContext(ctx, `SELECT state,resolved_at FROM funding_intents WHERE id='mfi_1'`).Scan(&state, &resolvedAt); err != nil {
		t.Fatal(err)
	}
	if state != "submitted" || resolvedAt != nil {
		t.Fatalf("lease loss classified provider outcome: state=%s resolved=%v", state, resolvedAt)
	}
}

func TestRecoverFundingOutcomeReplaysImmutableRechargeAndFinalizes(t *testing.T) {
	var body, key string
	fixture := newPaymentRecoveryFixture(t, "recharge", "outcome_unknown", func(_ *store.Store, _ time.Time, request *http.Request) (*http.Response, error) {
		raw, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		body = string(raw)
		key = request.Header.Get("Idempotency-Key")
		return paymentProviderResponse(http.StatusOK, `{"code":0,"data":null}`), nil
	})

	if err := fixture.processor.RecoverFundingOutcome(fixture.ctx, "mf-recovery", FundingRecoveryOptions{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if body != fixture.canonical || key != fixture.idempotencyKey {
		t.Fatalf("recovery changed immutable request: body=%s key=%s", body, key)
	}
	var intentState, fulfillmentState, stageState, stageCard string
	if err := fixture.repository.DB().QueryRow(`SELECT state FROM funding_intents WHERE id='intent-recovery'`).Scan(&intentState); err != nil {
		t.Fatal(err)
	}
	if err := fixture.repository.DB().QueryRow(`SELECT state FROM membership_fulfillments WHERE id='mf-recovery'`).Scan(&fulfillmentState); err != nil {
		t.Fatal(err)
	}
	if err := fixture.repository.DB().QueryRow(`SELECT state,card_id FROM membership_payment_stages WHERE id='stage-recovery'`).
		Scan(&stageState, &stageCard); err != nil {
		t.Fatal(err)
	}
	var availableEvents int
	if err := fixture.repository.DB().QueryRow(`SELECT COUNT(*) FROM membership_outbox WHERE event_type='membership.available'`).Scan(&availableEvents); err != nil {
		t.Fatal(err)
	}
	if intentState != "succeeded" || fulfillmentState != "CHECKOUT_EXECUTION_WAIT" ||
		stageState != "checkout_pending" || stageCard != "card-recovery" || availableEvents != 0 {
		t.Fatalf("recovery projection: intent=%s fulfillment=%s stage=%s card=%s events=%d",
			intentState, fulfillmentState, stageState, stageCard, availableEvents)
	}
}

func TestRecoverFundingOutcomeGateAndStateAreFailClosed(t *testing.T) {
	tests := []struct {
		name             string
		state            string
		fulfillmentState string
		options          FundingRecoveryOptions
		want             string
	}{
		{name: "gate disabled", state: "outcome_unknown", want: "MEMBERSHIP_PAYMENT_GATE_LOCKED"},
		{name: "prepared is not recovery", state: "prepared", options: FundingRecoveryOptions{Enabled: true}, want: "FUNDING_RECOVERY_NOT_ALLOWED"},
		{name: "submitted needs orphan confirmation", state: "submitted", options: FundingRecoveryOptions{Enabled: true}, want: "FUNDING_SUBMISSION_IN_PROGRESS"},
		{name: "terminal fulfillment", state: "outcome_unknown", fulfillmentState: "CANCELLED", options: FundingRecoveryOptions{Enabled: true}, want: "FUNDING_RECOVERY_NOT_ALLOWED"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			fixture := newPaymentRecoveryFixture(t, "recharge", test.state, func(_ *store.Store, _ time.Time, _ *http.Request) (*http.Response, error) {
				calls++
				return paymentProviderResponse(http.StatusOK, `{"code":0,"data":null}`), nil
			})
			if test.fulfillmentState != "" {
				if _, err := fixture.repository.DB().Exec(`UPDATE membership_fulfillments SET state=? WHERE id='mf-recovery'`, test.fulfillmentState); err != nil {
					t.Fatal(err)
				}
			}
			err := fixture.processor.RecoverFundingOutcome(fixture.ctx, "mf-recovery", test.options)
			if got := errorCode(err); got != test.want {
				t.Fatalf("error code=%s want=%s err=%v", got, test.want, err)
			}
			if calls != 0 {
				t.Fatalf("blocked recovery made %d provider calls", calls)
			}
			var state string
			if err := fixture.repository.DB().QueryRow(`SELECT state FROM funding_intents WHERE id='intent-recovery'`).Scan(&state); err != nil {
				t.Fatal(err)
			}
			if state != test.state {
				t.Fatalf("blocked recovery changed intent from %s to %s", test.state, state)
			}
		})
	}
}

func TestRecoverFundingOutcomeAllowsExplicitOrphanedSubmitted(t *testing.T) {
	calls := 0
	fixture := newPaymentRecoveryFixture(t, "recharge", "submitted", func(_ *store.Store, _ time.Time, _ *http.Request) (*http.Response, error) {
		calls++
		return paymentProviderResponse(http.StatusOK, `{"code":0,"data":null}`), nil
	})
	err := fixture.processor.RecoverFundingOutcome(fixture.ctx, "mf-recovery", FundingRecoveryOptions{
		Enabled: true, AllowOrphanedSubmitted: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("orphan recovery calls=%d", calls)
	}
}

func TestRecoverFundingOutcomeCircuitOpenLeavesUnknownIntentUntouched(t *testing.T) {
	calls := 0
	fixture := newPaymentRecoveryFixture(t, "recharge", "outcome_unknown", func(_ *store.Store, _ time.Time, _ *http.Request) (*http.Response, error) {
		calls++
		return paymentProviderResponse(http.StatusOK, `{"code":0,"data":null}`), nil
	})
	if _, err := fixture.repository.DB().Exec(`INSERT INTO fulfillment_dependency_circuits
    (id,dependency,scope_key,state,failure_count,opened_at,retry_at,recovery_revision,reason_code,updated_at)
    VALUES ('circuit-recovery','spacexcard_openapi','default','open',3,?,?,0,'SPACEXCARD_UNAVAILABLE',?)`,
		store.ISO(fixture.now), store.ISO(fixture.now.Add(time.Hour)), store.ISO(fixture.now)); err != nil {
		t.Fatal(err)
	}
	err := fixture.processor.RecoverFundingOutcome(fixture.ctx, "mf-recovery", FundingRecoveryOptions{Enabled: true})
	if got := errorCode(err); got != "SPACEXCARD_CIRCUIT_OPEN" {
		t.Fatalf("error code=%s err=%v", got, err)
	}
	var state string
	if err := fixture.repository.DB().QueryRow(`SELECT state FROM funding_intents WHERE id='intent-recovery'`).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if calls != 0 || state != "outcome_unknown" {
		t.Fatalf("open circuit calls=%d intent=%s", calls, state)
	}
}

func TestRecoverFundingOutcomeRejectsTamperedIntentOrReservation(t *testing.T) {
	tests := []struct {
		name   string
		mutate string
	}{
		{name: "amount", mutate: `UPDATE funding_intents SET amount=20.21 WHERE id='intent-recovery'`},
		{name: "idempotency key", mutate: `UPDATE funding_intents SET idempotency_key='kwr:tampered' WHERE id='intent-recovery'`},
		{name: "reservation", mutate: `UPDATE card_capacity_reservations SET state='consumed' WHERE id='reservation-recovery'`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			fixture := newPaymentRecoveryFixture(t, "recharge", "outcome_unknown", func(_ *store.Store, _ time.Time, _ *http.Request) (*http.Response, error) {
				calls++
				return paymentProviderResponse(http.StatusOK, `{"code":0,"data":null}`), nil
			})
			if _, err := fixture.repository.DB().Exec(test.mutate); err != nil {
				t.Fatal(err)
			}
			err := fixture.processor.RecoverFundingOutcome(fixture.ctx, "mf-recovery", FundingRecoveryOptions{Enabled: true})
			if got := errorCode(err); got != "FUNDING_INTENT_STORAGE_INVALID" {
				t.Fatalf("error code=%s err=%v", got, err)
			}
			if calls != 0 {
				t.Fatalf("tampered recovery made %d provider calls", calls)
			}
		})
	}
}

func TestRecoverFundingOutcomeLeaseLossAfterResponseMakesNoSubsequentWrites(t *testing.T) {
	fixture := newPaymentRecoveryFixture(t, "recharge", "outcome_unknown", func(repository *store.Store, now time.Time, _ *http.Request) (*http.Response, error) {
		if _, err := repository.DB().Exec(`UPDATE membership_processor_lease SET expires_at=? WHERE id='default'`, store.ISO(now)); err != nil {
			t.Fatal(err)
		}
		return paymentProviderResponse(http.StatusOK, `{"code":0,"data":null}`), nil
	})
	oldCircuitTime := store.ISO(fixture.now.Add(-time.Minute))
	if _, err := fixture.repository.DB().Exec(`INSERT INTO fulfillment_dependency_circuits
    (id,dependency,scope_key,state,failure_count,recovery_revision,reason_code,updated_at)
    VALUES ('circuit-recovery','spacexcard_openapi','default','closed',2,0,'SPACEXCARD_UNAVAILABLE',?)`, oldCircuitTime); err != nil {
		t.Fatal(err)
	}

	err := fixture.processor.RecoverFundingOutcome(fixture.ctx, "mf-recovery", FundingRecoveryOptions{Enabled: true})
	if !errors.Is(err, store.ErrLeaseLost) {
		t.Fatalf("expected lease loss, got %v", err)
	}
	var intentState, fulfillmentState, stageState, circuitTime string
	var resolvedAt any
	if err := fixture.repository.DB().QueryRow(`SELECT state,resolved_at FROM funding_intents WHERE id='intent-recovery'`).Scan(&intentState, &resolvedAt); err != nil {
		t.Fatal(err)
	}
	if err := fixture.repository.DB().QueryRow(`SELECT state FROM membership_fulfillments WHERE id='mf-recovery'`).Scan(&fulfillmentState); err != nil {
		t.Fatal(err)
	}
	if err := fixture.repository.DB().QueryRow(`SELECT state FROM membership_payment_stages WHERE id='stage-recovery'`).Scan(&stageState); err != nil {
		t.Fatal(err)
	}
	if err := fixture.repository.DB().QueryRow(`SELECT updated_at FROM fulfillment_dependency_circuits WHERE id='circuit-recovery'`).Scan(&circuitTime); err != nil {
		t.Fatal(err)
	}
	var outbox int
	if err := fixture.repository.DB().QueryRow(`SELECT COUNT(*) FROM membership_outbox`).Scan(&outbox); err != nil {
		t.Fatal(err)
	}
	if intentState != "submitted" || resolvedAt != nil || fulfillmentState != "FUNDING_OUTCOME_UNKNOWN" ||
		stageState != "funding_unknown" || circuitTime != oldCircuitTime || outbox != 0 {
		t.Fatalf("post-response lease loss wrote state: intent=%s resolved=%v fulfillment=%s stage=%s circuit=%s outbox=%d",
			intentState, resolvedAt, fulfillmentState, stageState, circuitTime, outbox)
	}
}

func TestRecoverFundingOutcomeClassifiesUnknownAndKnownNoWrite(t *testing.T) {
	tests := []struct {
		name            string
		status          int
		wantError       string
		wantIntent      string
		wantFulfillment string
		wantStage       string
	}{
		{name: "unknown", status: http.StatusServiceUnavailable, wantError: "FUNDING_OUTCOME_UNKNOWN", wantIntent: "outcome_unknown", wantFulfillment: "FUNDING_OUTCOME_UNKNOWN", wantStage: "funding_unknown"},
		{name: "known no write", status: http.StatusUnauthorized, wantError: "FUNDING_PROVIDER_REJECTED", wantIntent: "failed", wantFulfillment: "CHECKOUT_PRE_SUBMIT_FAILED", wantStage: "funding_failed"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newPaymentRecoveryFixture(t, "recharge", "outcome_unknown", func(_ *store.Store, _ time.Time, _ *http.Request) (*http.Response, error) {
				return paymentProviderResponse(test.status, ``), nil
			})
			err := fixture.processor.RecoverFundingOutcome(fixture.ctx, "mf-recovery", FundingRecoveryOptions{Enabled: true})
			if got := errorCode(err); got != test.wantError {
				t.Fatalf("error code=%s want=%s err=%v", got, test.wantError, err)
			}
			var intentState, fulfillmentState, stageState string
			if err := fixture.repository.DB().QueryRow(`SELECT state FROM funding_intents WHERE id='intent-recovery'`).Scan(&intentState); err != nil {
				t.Fatal(err)
			}
			if err := fixture.repository.DB().QueryRow(`SELECT state FROM membership_fulfillments WHERE id='mf-recovery'`).Scan(&fulfillmentState); err != nil {
				t.Fatal(err)
			}
			if err := fixture.repository.DB().QueryRow(`SELECT state FROM membership_payment_stages WHERE id='stage-recovery'`).Scan(&stageState); err != nil {
				t.Fatal(err)
			}
			if intentState != test.wantIntent || fulfillmentState != test.wantFulfillment || stageState != test.wantStage {
				t.Fatalf("classification: intent=%s fulfillment=%s stage=%s", intentState, fulfillmentState, stageState)
			}
		})
	}
}

func TestRecoverFundingOutcomeAttachesOpenedCardBeforeFinalize(t *testing.T) {
	fixture := newPaymentRecoveryFixture(t, "open", "outcome_unknown", func(_ *store.Store, _ time.Time, request *http.Request) (*http.Response, error) {
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["product_code"] != "product_1" || body["first_name"] != "Ada" || body["last_name"] != "Lovelace" || body["init_amount"] != 20.2 {
			t.Fatalf("open recovery changed request: %#v", body)
		}
		return paymentProviderResponse(http.StatusOK, `{"code":0,"data":{
      "id":16001,"vm_card_id":"vm-opened","product_code":"product_1",
      "available_amount":20.2,"status":"ACTIVE","open_fee":1
    }}`), nil
	})

	if err := fixture.processor.RecoverFundingOutcome(fixture.ctx, "mf-recovery", FundingRecoveryOptions{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	var cardID, lane, fulfillmentState, resourceID string
	if err := fixture.repository.DB().QueryRow(`SELECT id,lane FROM managed_cards WHERE upstream_card_id=16001`).Scan(&cardID, &lane); err != nil {
		t.Fatal(err)
	}
	var reservationCard string
	if err := fixture.repository.DB().QueryRow(`SELECT card_id FROM card_capacity_reservations WHERE id='reservation-recovery'`).Scan(&reservationCard); err != nil {
		t.Fatal(err)
	}
	if err := fixture.repository.DB().QueryRow(`SELECT provider_resource_id FROM funding_intents WHERE id='intent-recovery'`).Scan(&resourceID); err != nil {
		t.Fatal(err)
	}
	if err := fixture.repository.DB().QueryRow(`SELECT state FROM membership_fulfillments WHERE id='mf-recovery'`).Scan(&fulfillmentState); err != nil {
		t.Fatal(err)
	}
	if cardID == "" || reservationCard != cardID || lane != "plus" || resourceID != "16001" || fulfillmentState != "CHECKOUT_EXECUTION_WAIT" {
		t.Fatalf("open recovery: card=%s reservation=%s lane=%s resource=%s fulfillment=%s",
			cardID, reservationCard, lane, resourceID, fulfillmentState)
	}
}

func TestPaymentLoadLiveCardsRejectsPaginationContractDrift(t *testing.T) {
	tests := []struct {
		name    string
		page    func(int) (int, []int64)
		wantErr bool
	}{
		{name: "exact short final page", page: func(int) (int, []int64) { return 1, []int64{1} }},
		{name: "empty page before total", wantErr: true, page: func(int) (int, []int64) { return 1, nil }},
		{name: "short page before total", wantErr: true, page: func(int) (int, []int64) { return 2, []int64{1} }},
		{name: "collected exceeds total", wantErr: true, page: func(int) (int, []int64) { return 1, []int64{1, 2} }},
		{name: "total changes", wantErr: true, page: func(page int) (int, []int64) {
			if page == 1 {
				ids := make([]int64, paymentCardPageSize)
				for index := range ids {
					ids[index] = int64(index + 1)
				}
				return paymentCardPageSize + 1, ids
			}
			return paymentCardPageSize + 2, []int64{paymentCardPageSize + 1}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newPaymentReadFixture(t, func(_ *store.Store, _ time.Time, request *http.Request) (*http.Response, error) {
				page := 1
				if request.URL.Query().Get("page") == "2" {
					page = 2
				}
				total, ids := test.page(page)
				return paymentCardPageResponse(t, total, ids), nil
			})
			cards, err := fixture.processor.paymentLoadLiveCards(fixture.ctx, fixture.client)
			if test.wantErr {
				if got := errorCode(err); got != "PAYMENT_PROVIDER_CARD_LIST_INVALID" {
					t.Fatalf("error code=%s err=%v", got, err)
				}
				return
			}
			if err != nil || len(cards) != 1 {
				t.Fatalf("exact final page cards=%d err=%v", len(cards), err)
			}
		})
	}
}

func TestPaymentProviderReadsFenceLeaseAfterResponses(t *testing.T) {
	t.Run("balance", func(t *testing.T) {
		calls := 0
		fixture := newPaymentReadFixture(t, func(repository *store.Store, now time.Time, request *http.Request) (*http.Response, error) {
			calls++
			if request.URL.Path != "/openapi/v1/balance" {
				t.Fatalf("unexpected request after lost lease: %s", request.URL.Path)
			}
			expirePaymentTestLease(t, repository, now)
			return paymentProviderResponse(http.StatusOK, `{"code":0,"data":{"balance":100,"currency":"USD"}}`), nil
		})
		_, err := fixture.processor.paymentLoadFundingFacts(fixture.ctx, fixture.client)
		if !errors.Is(err, store.ErrLeaseLost) || calls != 1 {
			t.Fatalf("balance fence calls=%d err=%v", calls, err)
		}
	})

	t.Run("products", func(t *testing.T) {
		calls := 0
		fixture := newPaymentReadFixture(t, func(repository *store.Store, now time.Time, request *http.Request) (*http.Response, error) {
			calls++
			switch request.URL.Path {
			case "/openapi/v1/balance":
				return paymentProviderResponse(http.StatusOK, `{"code":0,"data":{"balance":100,"currency":"USD"}}`), nil
			case "/openapi/v1/products":
				expirePaymentTestLease(t, repository, now)
				return paymentProviderResponse(http.StatusOK, `{"code":0,"data":[]}`), nil
			default:
				t.Fatalf("unexpected request: %s", request.URL.Path)
				return nil, nil
			}
		})
		_, err := fixture.processor.paymentLoadFundingFacts(fixture.ctx, fixture.client)
		if !errors.Is(err, store.ErrLeaseLost) || calls != 2 {
			t.Fatalf("product fence calls=%d err=%v", calls, err)
		}
	})

	t.Run("cards", func(t *testing.T) {
		fixture := newPaymentReadFixture(t, func(repository *store.Store, now time.Time, _ *http.Request) (*http.Response, error) {
			expirePaymentTestLease(t, repository, now)
			return paymentCardPageResponse(t, 1, []int64{1}), nil
		})
		_, err := fixture.processor.paymentLoadLiveCards(fixture.ctx, fixture.client)
		if !errors.Is(err, store.ErrLeaseLost) {
			t.Fatalf("card fence err=%v", err)
		}
	})
}

type paymentRecoveryFixture struct {
	ctx            context.Context
	repository     *store.Store
	processor      *Processor
	now            time.Time
	canonical      string
	idempotencyKey string
}

type paymentReadFixture struct {
	ctx       context.Context
	processor *Processor
	client    *provider.SpaceXClient
}

func newPaymentReadFixture(
	t *testing.T,
	roundTrip func(*store.Store, time.Time, *http.Request) (*http.Response, error),
) paymentReadFixture {
	t.Helper()
	ctx := context.Background()
	temporary := t.TempDir()
	repository, err := store.Open(filepath.Join(temporary, "provider-read.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repository.Close() })
	now := time.Date(2026, 7, 20, 1, 2, 3, 0, time.UTC)
	if err := repository.EnsureLeaseTable(ctx, now); err != nil {
		t.Fatal(err)
	}
	lease, err := repository.AcquireLease(ctx, "go", "provider-read-test", "test", now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	httpClient := &http.Client{Transport: paymentRoundTripper(func(request *http.Request) (*http.Response, error) {
		return roundTrip(repository, now, request)
	}), Timeout: time.Second}
	client, err := provider.NewSpaceXClient(httpClient, "app", "secret")
	if err != nil {
		t.Fatal(err)
	}
	return paymentReadFixture{
		ctx: ctx,
		processor: &Processor{
			config: config.Config{MaintenancePath: filepath.Join(temporary, "maintenance.json")},
			store:  repository, lease: lease, now: func() time.Time { return now },
		},
		client: client,
	}
}

func paymentCardPageResponse(t *testing.T, total int, ids []int64) *http.Response {
	t.Helper()
	list := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		list = append(list, map[string]any{
			"id": id, "vm_card_id": fmt.Sprintf("vm-%d", id), "product_code": "product_1",
			"available_amount": 0, "status": "ACTIVE",
		})
	}
	body, err := json.Marshal(map[string]any{"code": 0, "data": map[string]any{"total": total, "list": list}})
	if err != nil {
		t.Fatal(err)
	}
	return paymentProviderResponse(http.StatusOK, string(body))
}

func expirePaymentTestLease(t *testing.T, repository *store.Store, now time.Time) {
	t.Helper()
	if _, err := repository.DB().Exec(`UPDATE membership_processor_lease SET expires_at=? WHERE id='default'`, store.ISO(now)); err != nil {
		t.Fatal(err)
	}
}

func newPaymentRecoveryFixture(
	t *testing.T,
	operation string,
	intentState string,
	roundTrip func(*store.Store, time.Time, *http.Request) (*http.Response, error),
) paymentRecoveryFixture {
	t.Helper()
	ctx := context.Background()
	temporary := t.TempDir()
	repository, err := store.Open(filepath.Join(temporary, "recovery.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repository.Close() })
	statements := []string{
		`CREATE TABLE membership_fulfillments (
      id TEXT PRIMARY KEY,order_id TEXT NOT NULL,order_no TEXT NOT NULL,target_tier TEXT NOT NULL,
      state TEXT NOT NULL,current_stage TEXT,run_mode TEXT,account_lock_key TEXT,state_revision INTEGER NOT NULL DEFAULT 0,
      retry_at TEXT,money_boundary_at TEXT,card_reservation_id TEXT,failure_code TEXT,created_at TEXT NOT NULL,
      updated_at TEXT NOT NULL,completed_at TEXT
    )`,
		`CREATE TABLE managed_cards (
      id TEXT PRIMARY KEY,upstream_card_id INTEGER NOT NULL UNIQUE,vm_card_id TEXT NOT NULL,product_code TEXT NOT NULL,
      bin TEXT,last4 TEXT,upstream_status TEXT NOT NULL,cached_available_amount REAL NOT NULL,lane TEXT,
      consumed_slots INTEGER NOT NULL,capacity_state TEXT NOT NULL,reconciliation_state TEXT NOT NULL,
      last_balance_sync_at TEXT,created_at TEXT NOT NULL,updated_at TEXT NOT NULL
    )`,
		`CREATE TABLE card_capacity_reservations (
      id TEXT PRIMARY KEY,fulfillment_id TEXT NOT NULL UNIQUE,card_id TEXT,planned_product_code TEXT,
      target_lane TEXT NOT NULL,slot_index INTEGER,state TEXT NOT NULL,reserved_at TEXT NOT NULL
    )`,
		`CREATE TABLE membership_payment_stages (
      id TEXT PRIMARY KEY,fulfillment_id TEXT NOT NULL,stage_key TEXT NOT NULL,expected_tier TEXT NOT NULL,state TEXT NOT NULL,
      card_id TEXT,price_signal_amount REAL,price_signal_min REAL,price_signal_max REAL,price_signal_time TEXT,
      adapter_version TEXT,price_contract_id TEXT,updated_at TEXT NOT NULL
    )`,
		`CREATE TABLE funding_intents (
      id TEXT PRIMARY KEY,fulfillment_id TEXT NOT NULL UNIQUE,operation TEXT NOT NULL,target_card_id TEXT,product_code TEXT,
      amount REAL NOT NULL,fee REAL NOT NULL,idempotency_key TEXT NOT NULL,request_fingerprint TEXT NOT NULL,
      request_body_encrypted TEXT NOT NULL,state TEXT NOT NULL,provider_resource_id TEXT,created_at TEXT NOT NULL,
      submitted_at TEXT,resolved_at TEXT
    )`,
		`CREATE TABLE membership_fulfillment_settings (
      id TEXT PRIMARY KEY,spacexcard_app_id TEXT,spacexcard_app_secret_encrypted TEXT
    )`,
		`CREATE TABLE fulfillment_dependency_circuits (
      id TEXT PRIMARY KEY,dependency TEXT NOT NULL,scope_key TEXT NOT NULL,state TEXT NOT NULL,
      failure_count INTEGER NOT NULL DEFAULT 0,opened_at TEXT,retry_at TEXT,recovery_revision INTEGER NOT NULL DEFAULT 0,
      reason_code TEXT,updated_at TEXT NOT NULL,UNIQUE(dependency,scope_key)
    )`,
		`CREATE TABLE membership_outbox (
      id TEXT PRIMARY KEY,event_type TEXT NOT NULL,fulfillment_id TEXT,state_revision INTEGER,payload TEXT NOT NULL,created_at TEXT NOT NULL
    )`,
	}
	for _, statement := range statements {
		if _, err := repository.DB().ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Date(2026, 7, 20, 1, 2, 3, 0, time.UTC)
	if err := repository.EnsureLeaseTable(ctx, now); err != nil {
		t.Fatal(err)
	}
	lease, err := repository.AcquireLease(ctx, "go", "recovery-test", "test", now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	crypt, err := secure.NewDecrypter("recovery-test-secret")
	if err != nil {
		t.Fatal(err)
	}
	credential, err := crypt.Encrypt("openapi-secret")
	if err != nil {
		t.Fatal(err)
	}
	canonical := `{"amount":20.2,"card_id":15001}`
	targetCard, productCode, reservationCard, plannedProduct, slot, fee := any("card-recovery"), any(nil), any("card-recovery"), any(nil), any(1), 0.25
	if operation == "open" {
		canonical = `{"first_name":"Ada","init_amount":20.2,"last_name":"Lovelace","product_code":"product_1"}`
		targetCard, productCode, reservationCard, plannedProduct, slot, fee = nil, "product_1", nil, "product_1", nil, 1.0
	}
	encrypted, err := crypt.Encrypt(canonical)
	if err != nil {
		t.Fatal(err)
	}
	at := store.ISO(now)
	orderNo := "ORDER-RECOVERY"
	key, err := paymentIdempotencyKey(orderNo, operation)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.DB().ExecContext(ctx, `INSERT INTO membership_fulfillments
    (id,order_id,order_no,target_tier,state,current_stage,run_mode,state_revision,money_boundary_at,card_reservation_id,created_at,updated_at)
    VALUES ('mf-recovery','order-recovery',?,'plus','FUNDING_OUTCOME_UNKNOWN','plus','canary',1,?,'reservation-recovery',?,?)`,
		orderNo, at, at, at); err != nil {
		t.Fatal(err)
	}
	if operation == "recharge" {
		if _, err := repository.DB().ExecContext(ctx, `INSERT INTO managed_cards
      (id,upstream_card_id,vm_card_id,product_code,upstream_status,cached_available_amount,lane,consumed_slots,
       capacity_state,reconciliation_state,created_at,updated_at)
      VALUES ('card-recovery',15001,'vm-recovery','product_1','ACTIVE',0,'plus',0,'AVAILABLE','READY',?,?)`, at, at); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := repository.DB().ExecContext(ctx, `INSERT INTO card_capacity_reservations
    (id,fulfillment_id,card_id,planned_product_code,target_lane,slot_index,state,reserved_at)
    VALUES ('reservation-recovery','mf-recovery',?,?, 'plus',?,'reserved',?)`,
		reservationCard, plannedProduct, slot, at); err != nil {
		t.Fatal(err)
	}
	stageCard := reservationCard
	if _, err := repository.DB().ExecContext(ctx, `INSERT INTO membership_payment_stages
    (id,fulfillment_id,stage_key,expected_tier,state,card_id,price_signal_amount,price_signal_min,price_signal_max,
     price_signal_time,adapter_version,price_contract_id,updated_at)
    VALUES ('stage-recovery','mf-recovery','plus','plus','funding_unknown',?,20,15,25,?,'checkout-v1','contract-plus',?)`,
		stageCard, at, at); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.DB().ExecContext(ctx, `INSERT INTO funding_intents
    (id,fulfillment_id,operation,target_card_id,product_code,amount,fee,idempotency_key,request_fingerprint,
     request_body_encrypted,state,created_at,submitted_at,resolved_at)
		VALUES ('intent-recovery','mf-recovery',?,?,?,20.2,?,?,?,?,?,?,?,?)`,
		operation, targetCard, productCode, fee, key, paymentFingerprint(canonical), encrypted, intentState, at, at, at); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.DB().ExecContext(ctx, `INSERT INTO membership_fulfillment_settings
    (id,spacexcard_app_id,spacexcard_app_secret_encrypted) VALUES ('default','app',?)`, credential); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: paymentRoundTripper(func(request *http.Request) (*http.Response, error) {
		return roundTrip(repository, now, request)
	}), Timeout: time.Second}
	processor := &Processor{
		config: config.Config{MaintenancePath: filepath.Join(temporary, "maintenance.json")},
		store:  repository, lease: lease, decrypter: crypt, httpClient: client,
		now: func() time.Time { return now },
	}
	return paymentRecoveryFixture{
		ctx: ctx, repository: repository, processor: processor, now: now,
		canonical: canonical, idempotencyKey: key,
	}
}

func paymentProviderResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

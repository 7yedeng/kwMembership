package processor

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kwmembership/internal/checkout"
	"kwmembership/internal/config"
	"kwmembership/internal/secure"
	"kwmembership/internal/store"
)

type checkoutExecutorFunc func(context.Context, checkout.Request) (checkout.Result, error)

func (fn checkoutExecutorFunc) Execute(ctx context.Context, request checkout.Request) (checkout.Result, error) {
	return fn(ctx, request)
}

func TestGoSessionPreflightNeedsNoBrokerOrExtensionDelivery(t *testing.T) {
	ctx := context.Background()
	repository, err := store.Open(filepath.Join(t.TempDir(), "checkout-flow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	if _, err := repository.DB().Exec(`CREATE TABLE membership_fulfillments (
    id TEXT PRIMARY KEY,order_id TEXT NOT NULL,order_no TEXT NOT NULL,target_tier TEXT NOT NULL,state TEXT NOT NULL,
    current_stage TEXT,run_mode TEXT,account_lock_key TEXT,state_revision INTEGER NOT NULL DEFAULT 0,retry_at TEXT,
    money_boundary_at TEXT,card_reservation_id TEXT,failure_code TEXT,created_at TEXT NOT NULL,updated_at TEXT NOT NULL,
    completed_at TEXT
	); CREATE TABLE redeem_orders (id TEXT PRIMARY KEY,session_payload TEXT NOT NULL,site_id TEXT,product_id TEXT);
	  CREATE TABLE checkout_price_contracts (
    id TEXT PRIMARY KEY,version INTEGER,tier TEXT,currency TEXT,min_amount REAL,max_amount REAL,status TEXT
	); CREATE TABLE checkout_validation_runs (
	  id TEXT PRIMARY KEY,order_id TEXT,site_id TEXT,product_id TEXT,tier TEXT,adapter_version TEXT,
	  price_contract_id TEXT,status TEXT,sanitized_result TEXT,started_at TEXT,completed_at TEXT,created_by TEXT
	); CREATE TABLE fulfillment_interventions (
	  id TEXT PRIMARY KEY,fulfillment_id TEXT,state TEXT,state_revision INTEGER,reason_code TEXT,created_at TEXT,
	  UNIQUE(fulfillment_id,state,state_revision,reason_code)
  ); CREATE TABLE membership_outbox (
    id TEXT PRIMARY KEY,event_type TEXT,fulfillment_id TEXT,state_revision INTEGER,payload TEXT,created_at TEXT
  )`); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 21, 1, 2, 3, 0, time.UTC)
	if err := repository.EnsureLeaseTable(ctx, now); err != nil {
		t.Fatal(err)
	}
	lease, err := repository.AcquireLease(ctx, "go", "checkout-flow", "test", now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	crypt, err := secure.NewDecrypter("checkout-flow-secret")
	if err != nil {
		t.Fatal(err)
	}
	encryptedSession, _ := crypt.Encrypt(`{"user":{"email":"buyer@example.com"},"sessionToken":"` + strings.Repeat("a", 4400) + `","expires":"2026-10-24T09:43:50.305Z"}`)
	at := store.ISO(now)
	if _, err := repository.DB().Exec(`INSERT INTO membership_fulfillments
    (id,order_id,order_no,target_tier,state,current_stage,run_mode,state_revision,created_at,updated_at)
	VALUES ('mf-flow','order-flow','ORDER-FLOW','plus','CHECKOUT_PREFLIGHT_READY','plus','automatic',0,?,?)`, at, at); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.DB().Exec(`INSERT INTO redeem_orders VALUES ('order-flow',?,'site-1','product-1')`, encryptedSession); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.DB().Exec(`INSERT INTO checkout_price_contracts
	VALUES ('price-plus',1,'plus','PHP',1000,1200,'active')`); err != nil {
		t.Fatal(err)
	}
	called := false
	runner := &Processor{
		config: config.Config{MaintenancePath: filepath.Join(t.TempDir(), "maintenance"), VisibleBrowser: true},
		store:  repository, lease: lease, decrypter: crypt,
		executor: checkoutExecutorFunc(func(ctx context.Context, request checkout.Request) (checkout.Result, error) {
			called = true
			if request.Mode != checkout.ModeSessionPreflight || request.CheckoutURL != "" || len(request.Cookies) != 2 ||
				request.ExpectedEmail != "buyer@example.com" || request.Material != nil {
				t.Fatalf("unexpected Session preflight request: mode=%s url=%s cookies=%d identity=%q material=%t",
					request.Mode, request.CheckoutURL, len(request.Cookies), request.ExpectedEmail, request.Material != nil)
			}
			if request.OnHandoff == nil {
				t.Fatal("visible preflight did not supply a challenge handoff")
			}
			if err := request.OnHandoff(ctx, checkout.Handoff{Type: "cloudflare", Page: checkout.PageFacts{
				StateID: "PAYMENT_ACTION_REQUIRED", Origin: "https://chatgpt.com",
			}}); err != nil {
				t.Fatal(err)
			}
			var state string
			var moneyBoundary, reservation any
			if err := repository.DB().QueryRow(`SELECT state,money_boundary_at,card_reservation_id
			  FROM membership_fulfillments WHERE id='mf-flow'`).Scan(&state, &moneyBoundary, &reservation); err != nil {
				t.Fatal(err)
			}
			if state != "CHECKOUT_CHALLENGE_WAIT" || moneyBoundary != nil || reservation != nil {
				t.Fatalf("unsafe handoff state=%s money=%v reservation=%v", state, moneyBoundary, reservation)
			}
			return checkout.Result{Page: checkout.PageFacts{StateID: "PAYMENT_FINAL_READY"}}, nil
		}),
		now: func() time.Time { return now }, requireLeaseFence: true,
	}
	fulfillment, err := loadFulfillment(ctx, repository.DB(), "mf-flow")
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.checkoutPreflight(ctx, fulfillment, now); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := repository.DB().QueryRow(`SELECT state FROM membership_fulfillments WHERE id='mf-flow'`).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if !called || state != "FUNDING_READY" {
		t.Fatalf("preflight called=%t state=%s", called, state)
	}
	var interventions int
	if err := repository.DB().QueryRow(`SELECT COUNT(*) FROM fulfillment_interventions
	  WHERE fulfillment_id='mf-flow' AND state='CHECKOUT_CHALLENGE_WAIT'
	    AND reason_code='CLOUDFLARE_CHALLENGE_REQUIRED'`).Scan(&interventions); err != nil || interventions != 1 {
		t.Fatalf("challenge interventions=%d err=%v", interventions, err)
	}
	var validations int
	if err := repository.DB().QueryRow(`SELECT COUNT(*) FROM checkout_validation_runs
	  WHERE order_id='order-flow' AND adapter_version='python-session-card-checkout-v1' AND status='passed'`).Scan(&validations); err != nil || validations != 1 {
		t.Fatalf("preflight validations=%d err=%v", validations, err)
	}

	if _, err := repository.DB().Exec(`INSERT INTO membership_fulfillments
    (id,order_id,order_no,target_tier,state,current_stage,run_mode,state_revision,created_at,updated_at)
	VALUES ('mf-subscribed','order-subscribed','ORDER-SUBSCRIBED','plus','CHECKOUT_PREFLIGHT_READY','plus',NULL,0,?,?)`, at, at); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.DB().Exec(`INSERT INTO redeem_orders VALUES ('order-subscribed',?,'site-1','product-1')`, encryptedSession); err != nil {
		t.Fatal(err)
	}
	runner.executor = checkoutExecutorFunc(func(context.Context, checkout.Request) (checkout.Result, error) {
		return checkout.Result{}, &checkout.Error{
			ErrorCode: "CHATGPT_ACCOUNT_ALREADY_SUBSCRIBED",
			Message:   "ChatGPT account already has an active paid subscription",
		}
	})
	fulfillment, err = loadFulfillment(ctx, repository.DB(), "mf-subscribed")
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.checkoutPreflight(ctx, fulfillment, now); err == nil {
		t.Fatal("active subscription unexpectedly reached funding readiness")
	}
	var subscribedState, subscribedFailure string
	var subscribedBoundary any
	if err := repository.DB().QueryRow(`SELECT state,failure_code,money_boundary_at
	  FROM membership_fulfillments WHERE id='mf-subscribed'`).Scan(
		&subscribedState, &subscribedFailure, &subscribedBoundary,
	); err != nil {
		t.Fatal(err)
	}
	if subscribedState != "ACCOUNT_ALREADY_SUBSCRIBED" ||
		subscribedFailure != "CHATGPT_ACCOUNT_ALREADY_SUBSCRIBED" || subscribedBoundary != nil {
		t.Fatalf("active subscription state=%s failure=%s money=%v",
			subscribedState, subscribedFailure, subscribedBoundary)
	}

	if _, err := repository.DB().Exec(`INSERT INTO membership_fulfillments
    (id,order_id,order_no,target_tier,state,current_stage,run_mode,state_revision,created_at,updated_at)
	VALUES ('mf-refresh','order-refresh','ORDER-REFRESH','plus','CHECKOUT_PREFLIGHT_READY','plus',NULL,0,?,?)`, at, at); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.DB().Exec(`INSERT INTO redeem_orders VALUES ('order-refresh',?,'site-1','product-1')`, encryptedSession); err != nil {
		t.Fatal(err)
	}
	runner.executor = checkoutExecutorFunc(func(context.Context, checkout.Request) (checkout.Result, error) {
		return checkout.Result{}, &checkout.Error{
			ErrorCode: "CHATGPT_SESSION_REFRESH_FAILED",
			Message:   "ChatGPT Session could not be refreshed",
		}
	})
	fulfillment, err = loadFulfillment(ctx, repository.DB(), "mf-refresh")
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.checkoutPreflight(ctx, fulfillment, now); err == nil {
		t.Fatal("Session refresh failure unexpectedly succeeded")
	}
	var refreshState, refreshFailure string
	var refreshRetry sql.NullString
	var refreshBoundary any
	if err := repository.DB().QueryRow(`SELECT state,failure_code,retry_at,money_boundary_at
	  FROM membership_fulfillments WHERE id='mf-refresh'`).Scan(
		&refreshState, &refreshFailure, &refreshRetry, &refreshBoundary,
	); err != nil {
		t.Fatal(err)
	}
	if refreshState != "CHECKOUT_PREFLIGHT_READY" || refreshFailure != "CHATGPT_SESSION_REFRESH_FAILED" ||
		!refreshRetry.Valid || refreshRetry.String != store.ISO(now.Add(checkoutRetryDelay)) || refreshBoundary != nil {
		t.Fatalf("refresh failure state=%s failure=%s retry=%s money=%v",
			refreshState, refreshFailure, refreshRetry.String, refreshBoundary)
	}

	if _, err := repository.DB().Exec(`INSERT INTO membership_fulfillments
    (id,order_id,order_no,target_tier,state,current_stage,run_mode,state_revision,money_boundary_at,created_at,updated_at)
	VALUES ('mf-executor-loss','order-executor-loss','ORDER-EXECUTOR-LOSS','plus','CHECKOUT_EXECUTION_WAIT','plus','automatic',0,?,?,?)`, at, at, at); err != nil {
		t.Fatal(err)
	}
	execution := checkoutExecution{
		Fulfillment: Fulfillment{ID: "mf-executor-loss"},
		Stage:       checkoutStage{StageKey: "plus"},
	}
	cause := coded("EXECUTOR_CONTEXT_LOST", "executor process lost its in-memory payment context")
	if err := runner.checkoutRecordExecutionFailure(ctx, execution, cause, now); errorCode(err) != "EXECUTOR_CONTEXT_LOST" {
		t.Fatalf("post-boundary failure = %v", err)
	}
	var uncertainState, uncertainFailure string
	var uncertainRetry sql.NullString
	if err := repository.DB().QueryRow(`SELECT state,failure_code,retry_at FROM membership_fulfillments
	  WHERE id='mf-executor-loss'`).Scan(&uncertainState, &uncertainFailure, &uncertainRetry); err != nil {
		t.Fatal(err)
	}
	if uncertainState != "EXECUTOR_OUTCOME_UNCERTAIN" || uncertainFailure != "EXECUTOR_CONTEXT_LOST" || uncertainRetry.Valid {
		t.Fatalf("executor loss state=%s failure=%s retry=%v", uncertainState, uncertainFailure, uncertainRetry)
	}
	var executorInterventions int
	if err := repository.DB().QueryRow(`SELECT COUNT(*) FROM fulfillment_interventions
	  WHERE fulfillment_id='mf-executor-loss' AND state='EXECUTOR_OUTCOME_UNCERTAIN'
	    AND reason_code='EXECUTOR_CONTEXT_LOST'`).Scan(&executorInterventions); err != nil || executorInterventions != 1 {
		t.Fatalf("executor interventions=%d err=%v", executorInterventions, err)
	}
}

func TestInteractiveLoginPreflightDoesNotLoadGPTOrReachFunding(t *testing.T) {
	ctx := context.Background()
	repository, err := store.Open(filepath.Join(t.TempDir(), "interactive-checkout-flow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	if _, err := repository.DB().Exec(`CREATE TABLE membership_fulfillments (
    id TEXT PRIMARY KEY,order_id TEXT NOT NULL,order_no TEXT NOT NULL,target_tier TEXT NOT NULL,state TEXT NOT NULL,
    current_stage TEXT,run_mode TEXT,account_lock_key TEXT,state_revision INTEGER NOT NULL DEFAULT 0,retry_at TEXT,
    money_boundary_at TEXT,card_reservation_id TEXT,failure_code TEXT,created_at TEXT NOT NULL,updated_at TEXT NOT NULL,
    completed_at TEXT
	); CREATE TABLE redeem_orders (id TEXT PRIMARY KEY,session_payload TEXT NOT NULL,site_id TEXT,product_id TEXT);
  CREATE TABLE checkout_price_contracts (
    id TEXT PRIMARY KEY,version INTEGER,tier TEXT,currency TEXT,min_amount REAL,max_amount REAL,status TEXT
	); CREATE TABLE checkout_validation_runs (
	  id TEXT PRIMARY KEY,order_id TEXT,site_id TEXT,product_id TEXT,tier TEXT,adapter_version TEXT,
	  price_contract_id TEXT,status TEXT,sanitized_result TEXT,started_at TEXT,completed_at TEXT,created_by TEXT
	); CREATE TABLE fulfillment_interventions (
	  id TEXT PRIMARY KEY,fulfillment_id TEXT,state TEXT,state_revision INTEGER,reason_code TEXT,created_at TEXT,
	  UNIQUE(fulfillment_id,state,state_revision,reason_code)
  ); CREATE TABLE membership_outbox (
    id TEXT PRIMARY KEY,event_type TEXT,fulfillment_id TEXT,state_revision INTEGER,payload TEXT,created_at TEXT
  )`); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 28, 1, 2, 3, 0, time.UTC)
	if err := repository.EnsureLeaseTable(ctx, now); err != nil {
		t.Fatal(err)
	}
	lease, err := repository.AcquireLease(ctx, "go", "interactive-checkout-flow", "test", now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	crypt, err := secure.NewDecrypter("interactive-checkout-flow-secret")
	if err != nil {
		t.Fatal(err)
	}
	encryptedSession, _ := crypt.Encrypt(`{"user":{"email":"buyer@example.com"}}`)
	at := store.ISO(now)
	if _, err := repository.DB().Exec(`INSERT INTO membership_fulfillments
    (id,order_id,order_no,target_tier,state,current_stage,run_mode,state_revision,created_at,updated_at)
	VALUES ('mf-interactive','order-interactive','ORDER-INTERACTIVE','plus','CHECKOUT_LOGIN_READY','plus',NULL,0,?,?)`, at, at); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.DB().Exec(`INSERT INTO redeem_orders VALUES ('order-interactive',?,'site-1','product-1')`, encryptedSession); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.DB().Exec(`INSERT INTO checkout_price_contracts
	VALUES ('price-plus',1,'plus','PHP',1000,1200,'active')`); err != nil {
		t.Fatal(err)
	}
	runner := &Processor{
		config: config.Config{MaintenancePath: filepath.Join(t.TempDir(), "maintenance"), VisibleBrowser: true},
		store:  repository, lease: lease, decrypter: crypt,
		executor: checkoutExecutorFunc(func(ctx context.Context, request checkout.Request) (checkout.Result, error) {
			if request.Mode != checkout.ModeInteractivePreflight || request.CheckoutURL != "" || len(request.Cookies) != 0 ||
				request.ExpectedEmail != "buyer@example.com" || request.Material != nil {
				t.Fatalf("unsafe interactive request: mode=%s url=%t cookies=%d identity=%q material=%t",
					request.Mode, request.CheckoutURL != "", len(request.Cookies), request.ExpectedEmail, request.Material != nil)
			}
			if request.OnHandoff == nil {
				t.Fatal("interactive preflight did not supply a login handoff")
			}
			if err := request.OnHandoff(ctx, checkout.Handoff{Type: "interactive-login"}); err != nil {
				t.Fatal(err)
			}
			var state string
			var moneyBoundary, reservation any
			if err := repository.DB().QueryRow(`SELECT state,money_boundary_at,card_reservation_id
			  FROM membership_fulfillments WHERE id='mf-interactive'`).Scan(&state, &moneyBoundary, &reservation); err != nil {
				t.Fatal(err)
			}
			if state != "CHECKOUT_LOGIN_WAIT" || moneyBoundary != nil || reservation != nil {
				t.Fatalf("unsafe login handoff state=%s money=%v reservation=%v", state, moneyBoundary, reservation)
			}
			return checkout.Result{Page: checkout.PageFacts{StateID: "PAYMENT_FINAL_READY"}}, nil
		}),
		now: func() time.Time { return now }, requireLeaseFence: true,
	}
	fulfillment, err := loadFulfillment(ctx, repository.DB(), "mf-interactive")
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.checkoutPreflight(ctx, fulfillment, now); err != nil {
		t.Fatal(err)
	}
	var state string
	var moneyBoundary, reservation any
	if err := repository.DB().QueryRow(`SELECT state,money_boundary_at,card_reservation_id
	  FROM membership_fulfillments WHERE id='mf-interactive'`).Scan(&state, &moneyBoundary, &reservation); err != nil {
		t.Fatal(err)
	}
	if state != "CHECKOUT_LOGIN_PREFLIGHT_PASSED" || moneyBoundary != nil || reservation != nil {
		t.Fatalf("unsafe completed state=%s money=%v reservation=%v", state, moneyBoundary, reservation)
	}
	var adapter string
	if err := repository.DB().QueryRow(`SELECT adapter_version FROM checkout_validation_runs
	  WHERE order_id='order-interactive'`).Scan(&adapter); err != nil {
		t.Fatal(err)
	}
	if adapter != "go-interactive-login-v0" {
		t.Fatalf("adapter = %q", adapter)
	}

	finish := now.Add(20 * time.Minute)
	if _, err := repository.DB().Exec(`UPDATE membership_fulfillments
	  SET state='CHECKOUT_LOGIN_READY',failure_code=NULL,retry_at=NULL,updated_at=?
	  WHERE id='mf-interactive'`, at); err != nil {
		t.Fatal(err)
	}
	runner.now = func() time.Time { return finish }
	runner.executor = checkoutExecutorFunc(func(ctx context.Context, request checkout.Request) (checkout.Result, error) {
		if err := request.OnHandoff(ctx, checkout.Handoff{Type: "interactive-login"}); err != nil {
			t.Fatal(err)
		}
		return checkout.Result{}, &checkout.Error{ErrorCode: "INTERACTIVE_LOGIN_TIMEOUT", Message: "interactive login timed out"}
	})
	fulfillment, err = loadFulfillment(ctx, repository.DB(), "mf-interactive")
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.checkoutPreflight(ctx, fulfillment, now); err == nil {
		t.Fatal("interactive timeout unexpectedly succeeded")
	}
	var failureCode, updatedAt string
	if err := repository.DB().QueryRow(`SELECT failure_code,updated_at FROM membership_fulfillments
	  WHERE id='mf-interactive'`).Scan(&failureCode, &updatedAt); err != nil {
		t.Fatal(err)
	}
	if failureCode != "INTERACTIVE_LOGIN_TIMEOUT" || updatedAt != store.ISO(finish) {
		t.Fatalf("failure timestamp = %s/%s, want INTERACTIVE_LOGIN_TIMEOUT/%s", failureCode, updatedAt, store.ISO(finish))
	}
}

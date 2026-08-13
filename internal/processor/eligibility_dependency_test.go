package processor

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"kwmembership/internal/config"
	"kwmembership/internal/provider"
	"kwmembership/internal/secure"
	"kwmembership/internal/store"
)

func TestEligibilityCircuitBlocksProviderAndPersistsRetry(t *testing.T) {
	var calls atomic.Int32
	processor, repository, fulfillment, now := newEligibilityDependencyProcessor(t, func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return eligibilityHTTPResponse(http.StatusOK, `{}`), nil
	})
	if _, err := repository.DB().Exec(`INSERT INTO fulfillment_dependency_circuits
    (id,dependency,scope_key,state,failure_count,opened_at,retry_at,recovery_revision,reason_code,updated_at)
    VALUES ('circuit-1','membership_provider','default','open',3,?,?,0,'MEMBERSHIP_PROVIDER_UNAVAILABLE',?)`,
		store.ISO(now), store.ISO(now.Add(time.Hour)), store.ISO(now)); err != nil {
		t.Fatal(err)
	}

	err := processor.processEligibility(context.Background(), fulfillment, now)
	if got := errorCode(err); got != "MEMBERSHIP_PROVIDER_CIRCUIT_OPEN" {
		t.Fatalf("error code = %s, err=%v", got, err)
	}
	if calls.Load() != 0 {
		t.Fatalf("open circuit allowed %d provider calls", calls.Load())
	}
	assertEligibilityFailure(t, repository, "ACCOUNT_CHECKING", "MEMBERSHIP_PROVIDER_CIRCUIT_OPEN")
}

func TestEligibilityProviderFailureIsPersistedRecordedAndReturned(t *testing.T) {
	processor, repository, fulfillment, now := newEligibilityDependencyProcessor(t, func(*http.Request) (*http.Response, error) {
		return eligibilityHTTPResponse(http.StatusServiceUnavailable, ``), nil
	})

	err := processor.processEligibility(context.Background(), fulfillment, now)
	if got := errorCode(err); got != "MEMBERSHIP_PROVIDER_UNAVAILABLE" {
		t.Fatalf("error code = %s, err=%v", got, err)
	}
	assertEligibilityFailure(t, repository, "ACCOUNT_CHECKING", "MEMBERSHIP_PROVIDER_UNAVAILABLE")
	var state, reason string
	var failures int
	if err := repository.DB().QueryRow(`SELECT state,failure_count,reason_code FROM fulfillment_dependency_circuits
    WHERE dependency='membership_provider' AND scope_key='default'`).Scan(&state, &failures, &reason); err != nil {
		t.Fatal(err)
	}
	if state != "closed" || failures != 1 || reason != "MEMBERSHIP_PROVIDER_UNAVAILABLE" {
		t.Fatalf("unexpected circuit: state=%s failures=%d reason=%s", state, failures, reason)
	}
}

func TestEligibilityContractFailureOpensCircuitAndReturnsCode(t *testing.T) {
	processor, repository, fulfillment, now := newEligibilityDependencyProcessor(t, func(*http.Request) (*http.Response, error) {
		return eligibilityHTTPResponse(http.StatusOK, `{"code":200,"data":{"account_type":"surprise"}}`), nil
	})

	err := processor.processEligibility(context.Background(), fulfillment, now)
	if got := errorCode(err); got != "MEMBERSHIP_CONTRACT_UNKNOWN" {
		t.Fatalf("error code = %s, err=%v", got, err)
	}
	assertEligibilityFailure(t, repository, "MEMBERSHIP_CONTRACT_UNKNOWN", "MEMBERSHIP_CONTRACT_UNKNOWN")
	var state, reason string
	if err := repository.DB().QueryRow(`SELECT state,reason_code FROM fulfillment_dependency_circuits
    WHERE dependency='membership_provider' AND scope_key='default'`).Scan(&state, &reason); err != nil {
		t.Fatal(err)
	}
	if state != "open" || reason != "MEMBERSHIP_CONTRACT_UNKNOWN" {
		t.Fatalf("unexpected contract circuit: state=%s reason=%s", state, reason)
	}
}

func TestEligibilityExpiredSessionDoesNotOpenProviderCircuit(t *testing.T) {
	processor, repository, fulfillment, now := newEligibilityDependencyProcessor(t, func(*http.Request) (*http.Response, error) {
		return eligibilityHTTPResponse(http.StatusOK, `{"code":401,"message":"token error","data":null}`), nil
	})

	err := processor.processEligibility(context.Background(), fulfillment, now)
	if got := errorCode(err); got != "SESSION_INVALID" {
		t.Fatalf("error code = %s, err=%v", got, err)
	}
	assertEligibilityTerminal(t, repository, "CANCELLED", "SESSION_INVALID_PRE_BOUNDARY")
	var circuits int
	if err := repository.DB().QueryRow(`SELECT COUNT(*) FROM fulfillment_dependency_circuits
    WHERE dependency='membership_provider' AND scope_key='default'`).Scan(&circuits); err != nil {
		t.Fatal(err)
	}
	if circuits != 0 {
		t.Fatalf("expired order session opened %d provider circuits", circuits)
	}
}

func TestEligibilityExpiredSessionReleasesHalfOpenProviderProbe(t *testing.T) {
	processor, repository, fulfillment, now := newEligibilityDependencyProcessor(t, func(*http.Request) (*http.Response, error) {
		return eligibilityHTTPResponse(http.StatusOK, `{"code":401,"message":"token error","data":null}`), nil
	})
	if _, err := repository.DB().Exec(`INSERT INTO fulfillment_dependency_circuits
    (id,dependency,scope_key,state,failure_count,opened_at,retry_at,recovery_revision,reason_code,updated_at)
    VALUES ('circuit-1','membership_provider','default','half_open',5,?,NULL,3,'MEMBERSHIP_CONTRACT_UNKNOWN',?)`,
		store.ISO(now.Add(-time.Hour)), store.ISO(now.Add(-6*time.Minute))); err != nil {
		t.Fatal(err)
	}

	err := processor.processEligibility(context.Background(), fulfillment, now)
	if got := errorCode(err); got != "SESSION_INVALID" {
		t.Fatalf("error code = %s, err=%v", got, err)
	}
	assertEligibilityTerminal(t, repository, "CANCELLED", "SESSION_INVALID_PRE_BOUNDARY")
	var state string
	var failures int
	var reason sql.NullString
	if err := repository.DB().QueryRow(`SELECT state,failure_count,reason_code
    FROM fulfillment_dependency_circuits WHERE dependency='membership_provider' AND scope_key='default'`).
		Scan(&state, &failures, &reason); err != nil {
		t.Fatal(err)
	}
	if state != "closed" || failures != 0 || reason.Valid {
		t.Fatalf("expired-session probe leaked circuit: state=%s failures=%d reason=%v", state, failures, reason)
	}
}

func TestEligibilitySuccessfulProbeClosesCircuit(t *testing.T) {
	processor, repository, fulfillment, now := newEligibilityDependencyProcessor(t, func(*http.Request) (*http.Response, error) {
		return eligibilityHTTPResponse(http.StatusOK, `{
      "code":200,
      "data":{
        "account_type":"plus","expire_time":"2026-08-20T00:00:00Z","currency":"PHP",
        "auto_renew":false,"is_overdue":false,"is_delinquent":false
      }
    }`), nil
	})
	if _, err := repository.DB().Exec(`INSERT INTO fulfillment_dependency_circuits
    (id,dependency,scope_key,state,failure_count,opened_at,retry_at,recovery_revision,reason_code,updated_at)
    VALUES ('circuit-1','membership_provider','default','open',3,?,?,1,'MEMBERSHIP_PROVIDER_UNAVAILABLE',?)`,
		store.ISO(now.Add(-time.Hour)), store.ISO(now.Add(-time.Millisecond)), store.ISO(now.Add(-time.Hour))); err != nil {
		t.Fatal(err)
	}

	if err := processor.processEligibility(context.Background(), fulfillment, now); err != nil {
		t.Fatal(err)
	}
	var fulfillmentState, circuitState string
	var failures, observations int
	var reason sql.NullString
	if err := repository.DB().QueryRow(`SELECT state FROM membership_fulfillments WHERE id='mf-1'`).Scan(&fulfillmentState); err != nil {
		t.Fatal(err)
	}
	if err := repository.DB().QueryRow(`SELECT state,failure_count,reason_code FROM fulfillment_dependency_circuits
    WHERE dependency='membership_provider' AND scope_key='default'`).Scan(&circuitState, &failures, &reason); err != nil {
		t.Fatal(err)
	}
	if err := repository.DB().QueryRow(`SELECT COUNT(*) FROM membership_observations WHERE fulfillment_id='mf-1'`).Scan(&observations); err != nil {
		t.Fatal(err)
	}
	if fulfillmentState != "ACCOUNT_ALREADY_SUBSCRIBED" || circuitState != "closed" || failures != 0 || reason.Valid || observations != 1 {
		t.Fatalf("probe result: fulfillment=%s circuit=%s failures=%d reason=%v observations=%d",
			fulfillmentState, circuitState, failures, reason, observations)
	}
}

func TestEligibilityCancelsRenewalAndRequiresFreshFreeObservation(t *testing.T) {
	var membershipCalls, cancelCalls atomic.Int32
	processor, repository, fulfillment, now := newEligibilityDependencyProcessor(t, func(request *http.Request) (*http.Response, error) {
		switch request.URL.String() {
		case provider.MembershipStateURL:
			call := membershipCalls.Add(1)
			autoRenew := call == 1
			return eligibilityHTTPResponse(http.StatusOK, `{"code":200,"data":{"account_type":"free",`+
				`"currency":null,"auto_renew":`+strconv.FormatBool(autoRenew)+`,"is_overdue":false,`+
				`"is_delinquent":false,"expire_time":null,"expires_at":""}}`), nil
		case provider.RenewalCancelURL:
			cancelCalls.Add(1)
			return eligibilityHTTPResponse(http.StatusOK, `{"code":0,"data":{"cancelled":true}}`), nil
		default:
			t.Fatalf("unexpected provider URL: %s", request.URL)
			return nil, nil
		}
	})
	processor.renewal = provider.NewRenewalClient(processor.httpClient)
	if _, err := repository.DB().Exec(`CREATE TABLE membership_fulfillment_settings (
	  id TEXT PRIMARY KEY,inventory_status TEXT NOT NULL
	); CREATE TABLE managed_cards (
	  id TEXT PRIMARY KEY,product_code TEXT,lane TEXT,consumed_slots INTEGER,capacity_state TEXT,
	  upstream_status TEXT,reconciliation_state TEXT
	); CREATE TABLE card_price_signals (
	  card_id TEXT,tier TEXT,found INTEGER,amount REAL,provider_time TEXT
	); CREATE TABLE card_product_policies (product_code TEXT,enabled INTEGER);
	CREATE TABLE checkout_price_contracts (
	  id TEXT PRIMARY KEY,tier TEXT,currency TEXT,status TEXT
	);
	INSERT INTO membership_fulfillment_settings VALUES ('default','completed');
	INSERT INTO managed_cards VALUES ('card-1','p1',NULL,0,'AVAILABLE','ACTIVE','READY');
	INSERT INTO card_price_signals VALUES ('card-1','plus',1,16.2,'2026-07-20T00:00:00Z');
	INSERT INTO checkout_price_contracts VALUES ('contract-1','plus','PHP','active')`); err != nil {
		t.Fatal(err)
	}
	if err := processor.processEligibility(context.Background(), fulfillment, now); err != nil {
		t.Fatal(err)
	}
	if membershipCalls.Load() != 2 || cancelCalls.Load() != 1 {
		t.Fatalf("provider calls: membership=%d cancel=%d", membershipCalls.Load(), cancelCalls.Load())
	}
	var state string
	var observations int
	if err := repository.DB().QueryRow(`SELECT state FROM membership_fulfillments WHERE id='mf-1'`).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if err := repository.DB().QueryRow(`SELECT COUNT(*) FROM membership_observations WHERE fulfillment_id='mf-1'`).Scan(&observations); err != nil {
		t.Fatal(err)
	}
	if state != "CHECKOUT_PREFLIGHT_READY" || observations != 2 {
		t.Fatalf("post-cancel state=%s observations=%d", state, observations)
	}
}

func TestSessionFailureAfterMoneyBoundaryKeepsOrderAndCDKLocked(t *testing.T) {
	processor, repository, fulfillment, now := newEligibilityDependencyProcessor(t, func(*http.Request) (*http.Response, error) {
		return nil, errors.New("provider should not be called")
	})
	if _, err := repository.DB().Exec(`INSERT INTO cdkeys
	  (id,status,locked_at,locked_by_order_id,updated_at) VALUES ('cdk-1','locked',?,'order-1',?);
	  UPDATE membership_fulfillments SET money_boundary_at=? WHERE id='mf-1'`,
		store.ISO(now), store.ISO(now), store.ISO(now)); err != nil {
		t.Fatal(err)
	}
	if err := processor.handleSessionFailure(context.Background(), fulfillment.ID, "SESSION_INVALID", "plus", now); err != nil {
		t.Fatal(err)
	}
	var state, cdkState, lockedBy string
	if err := repository.DB().QueryRow(`SELECT state FROM membership_fulfillments WHERE id='mf-1'`).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if err := repository.DB().QueryRow(`SELECT status,locked_by_order_id FROM cdkeys WHERE id='cdk-1'`).Scan(&cdkState, &lockedBy); err != nil {
		t.Fatal(err)
	}
	if state != "SESSION_RECOVERY_REQUIRED" || cdkState != "locked" || lockedBy != "order-1" {
		t.Fatalf("recovery state=%s cdk=%s lockedBy=%s", state, cdkState, lockedBy)
	}
}

type eligibilityRoundTripper func(*http.Request) (*http.Response, error)

func (roundTrip eligibilityRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

func eligibilityHTTPResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func newEligibilityDependencyProcessor(t *testing.T, roundTrip eligibilityRoundTripper) (*Processor, *store.Store, Fulfillment, time.Time) {
	t.Helper()
	ctx := context.Background()
	repository, err := store.Open(filepath.Join(t.TempDir(), "eligibility-dependency.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { repository.Close() })
	if _, err := repository.DB().Exec(`CREATE TABLE membership_fulfillments (
    id TEXT PRIMARY KEY,order_id TEXT NOT NULL,order_no TEXT NOT NULL,target_tier TEXT NOT NULL,
    state TEXT NOT NULL,current_stage TEXT,run_mode TEXT,account_lock_key TEXT,state_revision INTEGER NOT NULL DEFAULT 0,
    retry_at TEXT,money_boundary_at TEXT,card_reservation_id TEXT,failure_code TEXT,created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,completed_at TEXT
	  ); CREATE TABLE redeem_orders (
	    id TEXT PRIMARY KEY,session_payload TEXT NOT NULL,status TEXT,error_message TEXT,completed_at TEXT,updated_at TEXT
	  );
  CREATE TABLE membership_observations (
    id TEXT PRIMARY KEY,fulfillment_id TEXT NOT NULL,stage_key TEXT,purpose TEXT NOT NULL,provider_code INTEGER NOT NULL,
    account_type TEXT NOT NULL,currency TEXT,auto_renew INTEGER,is_overdue INTEGER NOT NULL,is_delinquent INTEGER NOT NULL,
    expire_time TEXT,observed_at TEXT NOT NULL
  ); CREATE TABLE fulfillment_dependency_circuits (
    id TEXT PRIMARY KEY,dependency TEXT NOT NULL,scope_key TEXT NOT NULL,state TEXT NOT NULL,
    failure_count INTEGER NOT NULL DEFAULT 0,opened_at TEXT,retry_at TEXT,recovery_revision INTEGER NOT NULL DEFAULT 0,
    reason_code TEXT,updated_at TEXT NOT NULL,UNIQUE(dependency,scope_key)
  ); CREATE TABLE membership_outbox (
    id TEXT PRIMARY KEY,event_type TEXT NOT NULL,fulfillment_id TEXT,state_revision INTEGER,payload TEXT NOT NULL,created_at TEXT NOT NULL
	  ); CREATE TABLE membership_checkout_commands (
	    id TEXT PRIMARY KEY,fulfillment_id TEXT NOT NULL,command_kind TEXT,state TEXT NOT NULL,material_claimed_at TEXT,
	    outcome_code TEXT,ended_at TEXT,updated_at TEXT NOT NULL
	  ); CREATE TABLE membership_action_permits (
	    id TEXT PRIMARY KEY,fulfillment_id TEXT NOT NULL,action_type TEXT,state TEXT NOT NULL,activated_at TEXT,
	    reported_at TEXT,outcome_code TEXT
	  ); CREATE TABLE card_capacity_reservations (
	    id TEXT PRIMARY KEY,fulfillment_id TEXT NOT NULL,card_id TEXT,target_lane TEXT,state TEXT NOT NULL,
	    released_at TEXT,release_evidence_revision INTEGER
  ); CREATE TABLE automatic_checkout_quota_reservations (
    id TEXT PRIMARY KEY,scope_id TEXT NOT NULL,fulfillment_id TEXT NOT NULL,business_date TEXT NOT NULL,
    order_units INTEGER NOT NULL,risk_reserved_usd REAL NOT NULL,state TEXT NOT NULL,released_at TEXT
	  ); CREATE TABLE automatic_checkout_daily_usage (
	    scope_id TEXT,business_date TEXT,order_units INTEGER,risk_reserved_usd REAL,updated_at TEXT
	  ); CREATE TABLE funding_intents (
	    id TEXT PRIMARY KEY,fulfillment_id TEXT NOT NULL,submitted_at TEXT
  ); CREATE TABLE activation_jobs (
    id TEXT PRIMARY KEY,order_id TEXT NOT NULL,status TEXT NOT NULL,last_error TEXT,locked_at TEXT,locked_by TEXT,updated_at TEXT
	  ); CREATE TABLE cdkeys (
	    id TEXT PRIMARY KEY,status TEXT NOT NULL,locked_at TEXT,locked_by_order_id TEXT,updated_at TEXT
	  ); CREATE TABLE fulfillment_interventions (
	    id TEXT PRIMARY KEY,fulfillment_id TEXT NOT NULL,state TEXT NOT NULL,state_revision INTEGER NOT NULL,
	    reason_code TEXT NOT NULL,created_at TEXT NOT NULL,UNIQUE(fulfillment_id,state,state_revision)
	  ); CREATE TABLE extension_delivery_settings (
	    id TEXT PRIMARY KEY,spacexcard_api_token_encrypted TEXT
	  )`); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 20, 1, 0, 0, 0, time.UTC)
	if err := repository.EnsureLeaseTable(ctx, now); err != nil {
		t.Fatal(err)
	}
	lease, err := repository.AcquireLease(ctx, "go", "eligibility-test", "test", now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	decrypter, err := secure.NewDecrypter("eligibility-test-secret")
	if err != nil {
		t.Fatal(err)
	}
	session, err := decrypter.Encrypt(`{"access_token":"session"}`)
	if err != nil {
		t.Fatal(err)
	}
	renewalToken, err := decrypter.Encrypt("renewal-token")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.DB().Exec(`INSERT INTO extension_delivery_settings
	  (id,spacexcard_api_token_encrypted) VALUES ('default',?)`, renewalToken); err != nil {
		t.Fatal(err)
	}
	at := store.ISO(now)
	if _, err := repository.DB().Exec(`INSERT INTO membership_fulfillments
    (id,order_id,order_no,target_tier,state,current_stage,state_revision,created_at,updated_at)
		VALUES ('mf-1','order-1','ORDER-1','plus','ACCOUNT_CHECKING','eligibility',0,?,?)`, at, at); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.DB().Exec(`INSERT INTO redeem_orders (id,session_payload) VALUES ('order-1',?)`, session); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: roundTrip, Timeout: time.Second}
	processor := &Processor{
		config:     config.Config{MaintenancePath: filepath.Join(t.TempDir(), "standby")},
		store:      repository,
		lease:      lease,
		decrypter:  decrypter,
		httpClient: client,
		membership: provider.NewMembershipClient(client),
		now:        func() time.Time { return now },
	}
	fulfillment, err := loadFulfillment(ctx, repository.DB(), "mf-1")
	if err != nil {
		t.Fatal(err)
	}
	return processor, repository, fulfillment, now
}

func assertEligibilityFailure(t *testing.T, repository *store.Store, wantState, wantCode string) {
	t.Helper()
	var state, code string
	var retryAt sql.NullString
	if err := repository.DB().QueryRow(`SELECT state,failure_code,retry_at FROM membership_fulfillments WHERE id='mf-1'`).
		Scan(&state, &code, &retryAt); err != nil {
		t.Fatal(err)
	}
	if state != wantState || code != wantCode || !retryAt.Valid {
		t.Fatalf("failure projection: state=%s code=%s retry=%v", state, code, retryAt)
	}
}

func assertEligibilityTerminal(t *testing.T, repository *store.Store, wantState, wantCode string) {
	t.Helper()
	var state, code string
	var retryAt sql.NullString
	if err := repository.DB().QueryRow(`SELECT state,failure_code,retry_at FROM membership_fulfillments WHERE id='mf-1'`).
		Scan(&state, &code, &retryAt); err != nil {
		t.Fatal(err)
	}
	if state != wantState || code != wantCode || retryAt.Valid {
		t.Fatalf("terminal projection: state=%s code=%s retry=%v", state, code, retryAt)
	}
}

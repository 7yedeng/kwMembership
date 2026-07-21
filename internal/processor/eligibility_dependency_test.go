package processor

import (
	"context"
	"database/sql"
	"io"
	"net/http"
	"path/filepath"
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
	assertEligibilityFailure(t, repository, "MEMBERSHIP_CONTRACT_UNKNOWN", "SESSION_INVALID")
	var circuits int
	if err := repository.DB().QueryRow(`SELECT COUNT(*) FROM fulfillment_dependency_circuits
    WHERE dependency='membership_provider' AND scope_key='default'`).Scan(&circuits); err != nil {
		t.Fatal(err)
	}
	if circuits != 0 {
		t.Fatalf("expired order session opened %d provider circuits", circuits)
	}
}

func TestEligibilitySuccessfulProbeClosesCircuit(t *testing.T) {
	processor, repository, fulfillment, now := newEligibilityDependencyProcessor(t, func(*http.Request) (*http.Response, error) {
		return eligibilityHTTPResponse(http.StatusOK, `{
      "code":200,
      "data":{
        "account_type":"plus","expire_time":"2026-08-20T00:00:00Z","currency":"PHP",
        "auto_renew":true,"is_overdue":false,"is_delinquent":false
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
  ); CREATE TABLE redeem_orders (id TEXT PRIMARY KEY,session_payload TEXT NOT NULL);
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

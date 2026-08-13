package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"kwmembership/internal/checkout"
	"kwmembership/internal/store"
)

const bridgeTestSecret = "bridge-test-secret-0000000000000001"

func TestBridgeLeasesPreflightAndClaimsMaterialOnce(t *testing.T) {
	ctx := context.Background()
	repository, err := store.Open(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	createBridgeSchema(t, repository)
	now := time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC)
	at := store.ISO(now)
	if _, err := repository.DB().Exec(`INSERT INTO membership_fulfillments
    (id,state,run_mode,state_revision,card_reservation_id,money_boundary_at,updated_at)
    VALUES ('mf-bridge','CHECKOUT_PREFLIGHT_READY','',3,NULL,NULL,?)`, at); err != nil {
		t.Fatal(err)
	}
	bridge, err := NewBridge(repository.DB(), bridgeTestSecret)
	if err != nil {
		t.Fatal(err)
	}
	bridge.now = func() time.Time { return now }
	server := httptest.NewServer(bridge.Handler())
	defer server.Close()

	checkoutRequest := checkout.Request{
		Mode: checkout.ModeSessionPreflight, Stage: "plus", TargetTier: "plus",
		Session:       json.RawMessage(`{"sessionToken":"protected","user":{"email":"buyer@example.com"}}`),
		ExpectedEmail: "buyer@example.com",
		PriceContract: checkout.PriceContract{ID: "price-plus", Version: 1, Tier: "plus", Currency: "PHP", MinAmount: 1000, MaxAmount: 1200},
		Binding:       checkout.ExecutionBinding{FulfillmentID: "mf-bridge", FulfillmentRevision: 3, AttemptNo: 0, PriorityClass: "normal", AdapterVersion: AdapterVersion},
	}
	if _, err := bridge.Execute(ctx, checkoutRequest); errorCode(err) != "EXECUTOR_PENDING" {
		t.Fatalf("first Execute error = %v", err)
	}
	waitForCommand(t, repository, "mf-bridge")

	unauthorized := postBridge(t, server.URL+"/internal/v1/executions/lease", "", nil, map[string]any{"executorId": "python-test"})
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized lease status = %d", unauthorized.Code)
	}
	leased := postBridge(t, server.URL+"/internal/v1/executions/lease", bridgeTestSecret, nil, map[string]any{"executorId": "python-test"})
	if leased.Code != http.StatusOK || leased.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("lease response = %d cache=%q body=%s", leased.Code, leased.Header().Get("Cache-Control"), leased.Body.String())
	}
	var command map[string]any
	if err := json.Unmarshal(leased.Body.Bytes(), &command); err != nil {
		t.Fatal(err)
	}
	if command["commandKind"] != "preflight" || command["adapterVersion"] != AdapterVersion {
		t.Fatalf("unexpected command: %#v", command)
	}
	headers := map[string]string{
		"X-Executor-ID": "python-test",
		"X-Lease-Epoch": jsonNumber(command["leaseEpoch"]),
		"X-Lease-Token": command["leaseToken"].(string),
	}
	executionID := command["executionId"].(string)
	materialURL := server.URL + "/internal/v1/executions/" + executionID + "/material"
	material := postBridge(t, materialURL, bridgeTestSecret, headers, map[string]any{})
	if material.Code != http.StatusOK || strings.Contains(material.Body.String(), "cookie") || strings.Contains(material.Body.String(), "checkoutURL") {
		t.Fatalf("unsafe material response = %d %s", material.Code, material.Body.String())
	}
	secondMaterial := postBridge(t, materialURL, bridgeTestSecret, headers, map[string]any{})
	if secondMaterial.Code != http.StatusConflict {
		t.Fatalf("second material claim status = %d", secondMaterial.Code)
	}
	page := map[string]any{
		"stateId": "PAYMENT_FINAL_READY", "origin": "https://pay.openai.com", "routeTemplate": "/pay/{id}",
		"plan": "plus", "country": "PH", "currency": "PHP", "displayedAmount": 1100,
		"stateMarker": "review", "fields": map[string]bool{}, "controls": map[string]string{}, "structuralHash": "untrusted",
	}
	pageResponse := postBridge(t, server.URL+"/internal/v1/executions/"+executionID+"/page-facts", bridgeTestSecret, headers, map[string]any{"page": page})
	if pageResponse.Code != http.StatusOK || strings.Contains(pageResponse.Body.String(), "untrusted") {
		t.Fatalf("page validation = %d %s", pageResponse.Code, pageResponse.Body.String())
	}
	var validated struct {
		Page checkout.PageFacts `json:"page"`
	}
	if err := json.Unmarshal(pageResponse.Body.Bytes(), &validated); err != nil {
		t.Fatal(err)
	}
	reported := postBridge(t, server.URL+"/internal/v1/executions/"+executionID+"/result", bridgeTestSecret, headers, map[string]any{
		"status": "success", "errorCode": "", "page": validated.Page,
		"diagnostic": map[string]string{"phase": "fixture", "status": "passed", "secret": "must-drop"},
	})
	if reported.Code != http.StatusAccepted {
		t.Fatalf("reported status = %d body=%s", reported.Code, reported.Body.String())
	}
	executed, executeErr := bridge.Execute(ctx, checkoutRequest)
	if executeErr != nil || executed.Page.StructuralHash == "" {
		t.Fatalf("second Execute result = %+v err=%v", executed, executeErr)
	}
	var state, diagnostic string
	if err := repository.DB().QueryRow(`SELECT state,sanitized_diagnostic FROM membership_checkout_commands WHERE id=?`, executionID).Scan(&state, &diagnostic); err != nil {
		t.Fatal(err)
	}
	if state != "reported" || strings.Contains(diagnostic, "secret") {
		t.Fatalf("command result = %s/%s", state, diagnostic)
	}
}

func TestBridgeRebuildsNeverLeasedQueuedContextAfterRestart(t *testing.T) {
	ctx := context.Background()
	repository, err := store.Open(filepath.Join(t.TempDir(), "bridge-restart.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	createBridgeSchema(t, repository)
	now := time.Date(2026, 8, 13, 1, 0, 0, 0, time.UTC)
	at := store.ISO(now)
	if _, err := repository.DB().Exec(`INSERT INTO membership_fulfillments
	  (id,state,run_mode,state_revision,card_reservation_id,money_boundary_at,updated_at)
	  VALUES ('mf-restart','CHECKOUT_PREFLIGHT_READY','',4,NULL,NULL,?)`, at); err != nil {
		t.Fatal(err)
	}
	request := checkout.Request{
		Mode: checkout.ModeSessionPreflight, Stage: "plus", TargetTier: "plus",
		Session:       json.RawMessage(`{"sessionToken":"protected","user":{"email":"buyer@example.com"}}`),
		ExpectedEmail: "buyer@example.com",
		PriceContract: checkout.PriceContract{ID: "price-plus", Version: 1, Tier: "plus", Currency: "PHP", MinAmount: 1000, MaxAmount: 1200},
		Binding:       checkout.ExecutionBinding{FulfillmentID: "mf-restart", FulfillmentRevision: 4, AttemptNo: 4, PriorityClass: "normal", AdapterVersion: AdapterVersion},
	}
	first, err := NewBridge(repository.DB(), bridgeTestSecret)
	if err != nil {
		t.Fatal(err)
	}
	first.now = func() time.Time { return now }
	if _, err := first.Execute(ctx, request); errorCode(err) != "EXECUTOR_PENDING" {
		t.Fatalf("first Execute error = %v", err)
	}

	restarted, err := NewBridge(repository.DB(), bridgeTestSecret)
	if err != nil {
		t.Fatal(err)
	}
	restarted.now = func() time.Time { return now }
	if _, err := restarted.Execute(ctx, request); errorCode(err) != "EXECUTOR_PENDING" {
		t.Fatalf("restarted Execute error = %v", err)
	}
	lease, err := restarted.queue.LeaseNext(ctx, "python-restarted", now)
	if err != nil || lease.Command.FulfillmentID != "mf-restart" {
		t.Fatalf("restarted lease = %+v err=%v", lease.Command, err)
	}
}

func TestExpectedActionControlBindsUpgradeToTargetTier(t *testing.T) {
	page := checkout.PageFacts{
		StateID: "UPGRADE_SELECTION_READY",
		Controls: map[string]string{
			"progression": "",
			"upgradeX5":   "upgrade-x5",
			"upgradeX20":  "upgrade-x20",
		},
	}
	request := checkout.Request{TargetTier: "x5"}
	if got := expectedActionControl(page, request, checkout.ActionProgression); got != "upgrade-x5" {
		t.Fatalf("x5 progression control = %q", got)
	}
	request.TargetTier = "x20"
	if got := expectedActionControl(page, request, checkout.ActionProgression); got != "upgrade-x20" {
		t.Fatalf("x20 progression control = %q", got)
	}
	if got := expectedActionControl(page, request, checkout.ActionSubmit); got != "" {
		t.Fatalf("upgrade submit control = %q", got)
	}

	page.StateID = "PAYMENT_PROGRESSION_READY"
	page.Controls["progression"] = "payment-next"
	if got := expectedActionControl(page, request, checkout.ActionProgression); got != "payment-next" {
		t.Fatalf("payment progression control = %q", got)
	}
}

func TestValidateHandoffFactsRejectsUntrustedChallengePage(t *testing.T) {
	request := checkout.Request{
		Stage: "plus", TargetTier: "plus",
		PriceContract: checkout.PriceContract{MinAmount: 1000, MaxAmount: 1200},
	}
	trusted := checkout.PageFacts{
		StateID: "PAYMENT_ACTION_REQUIRED", Origin: "https://chatgpt.com",
		RouteTemplate: "/checkout/{id}", Controls: map[string]string{"challenge": "challenge-cloudflare"},
		Fields: map[string]bool{},
	}
	validated, err := validateHandoffFacts(trusted, request)
	if err != nil || validated.StructuralHash == "" {
		t.Fatalf("trusted challenge = %+v err=%v", validated, err)
	}

	foreign := trusted
	foreign.Origin = "https://evil.example"
	if _, err := validateHandoffFacts(foreign, request); errorCode(err) != "CHECKOUT_CONTEXT_INVALID" {
		t.Fatalf("foreign challenge error = %v", err)
	}
	unknown := trusted
	unknown.Controls = map[string]string{"challenge": "challenge-unknown"}
	if _, err := validateHandoffFacts(unknown, request); errorCode(err) != "CHECKOUT_PAGE_CONTRACT_INVALID" {
		t.Fatalf("unknown challenge error = %v", err)
	}
}

func errorCode(err error) string {
	var coded interface{ Code() string }
	if errors.As(err, &coded) {
		return coded.Code()
	}
	return ""
}

func createBridgeSchema(t *testing.T, repository *store.Store) {
	t.Helper()
	_, err := repository.DB().Exec(`CREATE TABLE membership_checkout_commands (
    id TEXT PRIMARY KEY,fulfillment_id TEXT,stage_key TEXT,attempt_no INTEGER,command_kind TEXT,
    priority_class TEXT,target_tier TEXT,adapter_version TEXT,price_contract_id TEXT,
    price_contract_version INTEGER,fulfillment_revision INTEGER,state TEXT,lease_epoch INTEGER DEFAULT 0,
    lease_token_sha256 TEXT,leased_by TEXT,leased_at TEXT,heartbeat_at TEXT,lease_expires_at TEXT,
    hard_deadline_at TEXT,material_claimed_at TEXT,available_at TEXT,payment_ready_at TEXT,outcome_code TEXT,
    sanitized_diagnostic TEXT,created_at TEXT,updated_at TEXT,ended_at TEXT,
    UNIQUE(fulfillment_id,stage_key,attempt_no,command_kind)
  ); CREATE TABLE membership_fulfillments (
    id TEXT PRIMARY KEY,state TEXT,run_mode TEXT,state_revision INTEGER,card_reservation_id TEXT,
    money_boundary_at TEXT,updated_at TEXT
  ); CREATE TABLE membership_fulfillment_settings (id TEXT PRIMARY KEY,enabled INTEGER,rollout_mode TEXT);
  CREATE TABLE membership_payment_stages (
    fulfillment_id TEXT,stage_key TEXT,attempt_no INTEGER,adapter_version TEXT,price_contract_id TEXT,state TEXT,card_id TEXT
  ); CREATE TABLE card_capacity_reservations (id TEXT PRIMARY KEY,state TEXT,card_id TEXT);
  CREATE TABLE membership_action_permits (
    fulfillment_id TEXT,stage_key TEXT,attempt_no INTEGER,action_type TEXT,state TEXT
  ); INSERT INTO membership_fulfillment_settings VALUES ('default',0,'disabled');`)
	if err != nil {
		t.Fatal(err)
	}
}

func waitForCommand(t *testing.T, repository *store.Store, fulfillmentID string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		var count int
		if err := repository.DB().QueryRow(`SELECT COUNT(*) FROM membership_checkout_commands WHERE fulfillment_id=?`, fulfillmentID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count == 1 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("checkout command was not queued")
}

func postBridge(t *testing.T, url, secret string, headers map[string]string, payload any) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(payload)
	request := httptest.NewRequest(http.MethodPost, url, bytes.NewReader(raw))
	request.Header.Set("Content-Type", "application/json")
	if secret != "" {
		request.Header.Set("Authorization", "Bearer "+secret)
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response := httptest.NewRecorder()
	// Preserve the URL path routing behavior while avoiding a real network call.
	serverURL := request.URL
	request.URL.Scheme, request.URL.Host = "", ""
	_ = serverURL
	// Handler is recovered through the test server's client only for routing;
	// use the globally installed transport would hide response headers.
	response.Code = 0
	realRequest, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(raw))
	realRequest.Header = request.Header.Clone()
	realResponse, err := http.DefaultClient.Do(realRequest)
	if err != nil {
		t.Fatal(err)
	}
	defer realResponse.Body.Close()
	response.Code = realResponse.StatusCode
	response.HeaderMap = realResponse.Header.Clone()
	_, _ = response.Body.ReadFrom(realResponse.Body)
	return response
}

func jsonNumber(value any) string {
	return strings.TrimSuffix(strings.TrimSuffix(strconv.FormatFloat(value.(float64), 'f', 1, 64), "0"), ".")
}

package processor

import (
	"context"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kwmembership/internal/checkout"
	"kwmembership/internal/config"
	"kwmembership/internal/provider"
	"kwmembership/internal/secure"
	"kwmembership/internal/store"
)

type checkoutExecutorFunc func(context.Context, checkout.Request) (checkout.Result, error)

func (fn checkoutExecutorFunc) Execute(ctx context.Context, request checkout.Request) (checkout.Result, error) {
	return fn(ctx, request)
}

func TestGoPreflightNeedsNoExtensionDelivery(t *testing.T) {
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
  CREATE TABLE extension_delivery_settings (id TEXT PRIMARY KEY,spacexcard_api_token_encrypted TEXT);
  CREATE TABLE checkout_price_contracts (
    id TEXT PRIMARY KEY,version INTEGER,tier TEXT,currency TEXT,min_amount REAL,max_amount REAL,status TEXT
	); CREATE TABLE checkout_validation_runs (
	  id TEXT PRIMARY KEY,order_id TEXT,site_id TEXT,product_id TEXT,tier TEXT,adapter_version TEXT,
	  price_contract_id TEXT,status TEXT,sanitized_result TEXT,started_at TEXT,completed_at TEXT,created_by TEXT
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
	encryptedSession, _ := crypt.Encrypt(`{"user":{"email":"buyer@example.com"}}`)
	encryptedToken, _ := crypt.Encrypt("gpt-token")
	at := store.ISO(now)
	if _, err := repository.DB().Exec(`INSERT INTO membership_fulfillments
    (id,order_id,order_no,target_tier,state,current_stage,run_mode,state_revision,created_at,updated_at)
	VALUES ('mf-flow','order-flow','ORDER-FLOW','plus','CHECKOUT_PREFLIGHT_READY','plus','automatic',0,?,?)`, at, at); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.DB().Exec(`INSERT INTO redeem_orders VALUES ('order-flow',?,'site-1','product-1')`, encryptedSession); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.DB().Exec(`INSERT INTO extension_delivery_settings VALUES ('default',?)`, encryptedToken); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.DB().Exec(`INSERT INTO checkout_price_contracts
	VALUES ('price-plus',1,'plus','PHP',1000,1200,'active')`); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: paymentRoundTripper(func(request *http.Request) (*http.Response, error) {
		if request.Header.Get("Authorization") != "Bearer gpt-token" {
			t.Fatalf("missing GPT token for %s", request.URL.Path)
		}
		body := `{"code":0,"data":{"link":"https://chatgpt.com/checkout/safe"}}`
		if strings.HasSuffix(request.URL.Path, "/session-to-cookie") {
			body = `{"code":0,"data":{"count":1,"email":"buyer@example.com","cookies":[{"domain":".chatgpt.com","hostOnly":false,"httpOnly":true,"name":"__Secure-next-auth.session-token","path":"/","sameSite":"lax","secure":true,"session":true,"storeId":null,"value":"cookie-secret"}]}}`
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	}), Timeout: time.Second}
	called := false
	runner := &Processor{
		config: config.Config{MaintenancePath: filepath.Join(t.TempDir(), "maintenance")},
		store:  repository, lease: lease, decrypter: crypt, gpt: provider.NewGPTClient(client),
		executor: checkoutExecutorFunc(func(_ context.Context, request checkout.Request) (checkout.Result, error) {
			called = true
			if request.Mode != checkout.ModePreflight || request.CheckoutURL != "https://chatgpt.com/checkout/safe" || len(request.Cookies) != 1 || request.Material != nil {
				t.Fatalf("unexpected preflight request: mode=%s url=%s cookies=%d material=%t", request.Mode, request.CheckoutURL, len(request.Cookies), request.Material != nil)
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
	var validations int
	if err := repository.DB().QueryRow(`SELECT COUNT(*) FROM checkout_validation_runs
	  WHERE order_id='order-flow' AND adapter_version='go-headless-v1' AND status='passed'`).Scan(&validations); err != nil || validations != 1 {
		t.Fatalf("preflight validations=%d err=%v", validations, err)
	}
}

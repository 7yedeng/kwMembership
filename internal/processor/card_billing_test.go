package processor

import (
	"context"
	"database/sql"
	"errors"
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

type cardBillingStub struct {
	lookups  []provider.CardReference
	unbound  int
	response provider.BillingAddress
	err      error
}

func (stub *cardBillingStub) Cardholder(context.Context) (provider.Cardholder, error) {
	return provider.Cardholder{FirstName: "Fixture", LastName: "Holder"}, nil
}
func (stub *cardBillingStub) Billing(context.Context) (provider.BillingAddress, error) {
	stub.unbound++
	return stub.response, stub.err
}
func (stub *cardBillingStub) BillingForCard(_ context.Context, reference provider.CardReference) (provider.BillingAddress, error) {
	stub.lookups = append(stub.lookups, reference)
	return stub.response, stub.err
}

func cardBillingFixture() provider.BillingAddress {
	return provider.BillingAddress{
		Name: "Fixture Holder", Line1: "1 Test Street", Line2: "Unit 2", City: "Fixture City",
		State: "CA", PostalCode: "00000", Country: "US",
	}
}

func TestCheckoutBillingUsesCompleteCardFactsWithoutAddressService(t *testing.T) {
	stub := &cardBillingStub{err: errors.New("must not call an address service")}
	p := &Processor{address: stub, requireCardBillingBinding: true}
	issuer := cardBillingFixture()
	issuer.State, issuer.PostalCode, issuer.Country = "", "", "SG"
	got, err := p.resolveCardBilling(context.Background(), provider.CardReference{PlatformKey: "test", UpstreamCardID: 1}, provider.CardMaterial{
		HolderName: "Fixture Holder", Billing: &issuer,
	})
	if err != nil || got != issuer || len(stub.lookups) != 0 || stub.unbound != 0 {
		t.Fatal("complete issuer facts did not take precedence")
	}
}

func TestOwnedCheckoutBillingRequiresExactCardBinding(t *testing.T) {
	reference := provider.CardReference{PlatformKey: "test", UpstreamCardID: 101}
	address := cardBillingFixture()
	bound, err := provider.NewCardBoundBillingService(provider.Cardholder{FirstName: "Fixture", LastName: "Holder"}, map[provider.CardReference]provider.BillingAddress{reference: address})
	if err != nil {
		t.Fatal(err)
	}
	p := &Processor{address: bound, requireCardBillingBinding: true}
	got, err := p.resolveCardBilling(context.Background(), reference, provider.CardMaterial{HolderName: address.Name})
	if err != nil || got != address {
		t.Fatal("matching explicitly bound profile was not used")
	}
	for _, other := range []provider.CardReference{
		{PlatformKey: "test", UpstreamCardID: 102}, {PlatformKey: "other", UpstreamCardID: 101},
	} {
		if got, err := p.resolveCardBilling(context.Background(), other, provider.CardMaterial{}); errorCode(err) != "CARD_BILLING_NOT_CONFIGURED" || got != (provider.BillingAddress{}) {
			t.Fatal("profile escaped its card or platform binding")
		}
	}
	unbound, err := provider.NewConfiguredBillingService(provider.BillingProfile{
		Holder: provider.Cardholder{FirstName: "Fixture", LastName: "Holder"}, Address: address,
	})
	if err != nil {
		t.Fatal(err)
	}
	owned, err := NewWithServices(config.Config{}, nil, store.Lease{}, nil, nil, Services{
		Accounts: &accountServiceStub{}, Billing: unbound,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := owned.resolveCardBilling(context.Background(), reference, provider.CardMaterial{}); errorCode(err) != "BILLING_CARD_BINDING_REQUIRED" {
		t.Fatal("owned constructor allowed an unbound fallback profile")
	}
}

func TestCheckoutBillingFillsOnlyMissingIssuerFields(t *testing.T) {
	stub := &cardBillingStub{response: cardBillingFixture()}
	stub.response.Line2 = ""
	p := &Processor{address: stub, requireCardBillingBinding: true}
	reference := provider.CardReference{PlatformKey: "test", UpstreamCardID: 71}
	partial := provider.BillingAddress{Line2: "Issuer Unit", Country: " us "}
	got, err := p.resolveCardBilling(context.Background(), reference, provider.CardMaterial{HolderName: "Fixture Holder", Billing: &partial})
	if err != nil || got.Line2 != "Issuer Unit" || got.Line1 != "1 Test Street" || got.Country != "US" ||
		len(stub.lookups) != 1 || stub.lookups[0] != reference || stub.unbound != 0 {
		t.Fatal("partial issuer facts were overwritten or resolved without the exact reference")
	}
}

func TestCheckoutBillingRejectsConflictingAndMalformedCardFacts(t *testing.T) {
	for name, partial := range map[string]provider.BillingAddress{
		"name": {Name: "Different Holder"}, "street": {Line1: "Different Street"},
		"line2": {Line2: "Different Unit"}, "city": {City: "Different City"},
		"state": {State: "NY"}, "postal": {PostalCode: "99999"}, "country": {Country: "GB"},
	} {
		t.Run(name, func(t *testing.T) {
			stub := &cardBillingStub{response: cardBillingFixture()}
			p := &Processor{address: stub, requireCardBillingBinding: true}
			got, err := p.resolveCardBilling(context.Background(), provider.CardReference{PlatformKey: "test", UpstreamCardID: 1}, provider.CardMaterial{Billing: &partial})
			if errorCode(err) != "CARD_BILLING_CONFLICT" || got != (provider.BillingAddress{}) {
				t.Fatal("conflicting issuer fact was replaced by configuration")
			}
		})
	}
	stub := &cardBillingStub{response: cardBillingFixture()}
	p := &Processor{address: stub, requireCardBillingBinding: true}
	_, err := p.resolveCardBilling(context.Background(), provider.CardReference{PlatformKey: "test", UpstreamCardID: 1}, provider.CardMaterial{Billing: &provider.BillingAddress{Line2: "private\n"}})
	if errorCode(err) != "BILLING_PROFILE_INVALID" || len(stub.lookups) != 0 || strings.Contains(err.Error(), "private") {
		t.Fatal("malformed issuer facts were repaired or disclosed")
	}
}

func TestCheckoutBillingSanitizesLookupFailuresAndHonorsCancellation(t *testing.T) {
	secret := "private-person=https://example.invalid/?secret=fixture"
	upstream := errors.New(secret)
	stub := &cardBillingStub{err: upstream}
	p := &Processor{address: stub, requireCardBillingBinding: true}
	reference := provider.CardReference{PlatformKey: "test", UpstreamCardID: 1}
	_, err := p.resolveCardBilling(context.Background(), reference, provider.CardMaterial{})
	if errorCode(err) != "CARD_BILLING_UNAVAILABLE" || !errors.Is(err, upstream) || strings.Contains(err.Error(), secret) {
		t.Fatal("lookup error was not classified and sanitized")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, err := p.resolveCardBilling(ctx, reference, provider.CardMaterial{})
	if !errors.Is(err, context.Canceled) || got != (provider.BillingAddress{}) || len(stub.lookups) != 1 {
		t.Fatal("cancelled lookup read or returned billing data")
	}
}

func TestCheckoutExecutionCarriesThisCardsIssuerAddressToExecutor(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	repository, err := store.Open(filepath.Join(t.TempDir(), "card-material.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	if _, err := repository.DB().Exec(`CREATE TABLE membership_fulfillments (id TEXT PRIMARY KEY,order_id TEXT);
	  CREATE TABLE redeem_orders (id TEXT PRIMARY KEY,session_payload TEXT);
	  CREATE TABLE membership_card_platforms (key TEXT PRIMARY KEY,kind TEXT,base_url TEXT,credential_encrypted TEXT);
	  INSERT INTO membership_fulfillments VALUES ('fulfillment-fixture','order-fixture');`); err != nil {
		t.Fatal(err)
	}
	if err := repository.EnsureLeaseTable(ctx, now); err != nil {
		t.Fatal(err)
	}
	lease, err := repository.AcquireLease(ctx, "go", "fixture-owner", "test", now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	crypt, err := secure.NewDecrypter("synthetic-billing-test-key")
	if err != nil {
		t.Fatal(err)
	}
	session, err := crypt.Encrypt(`{"sessionToken":"synthetic-session","user":{"email":"buyer@example.invalid"},"expires":"2099-01-01T00:00:00Z"}`)
	if err != nil {
		t.Fatal(err)
	}
	credential, err := crypt.Encrypt(`{"appId":"fixture-app","appSecret":"fixture-key"}`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.DB().Exec(`INSERT INTO redeem_orders VALUES ('order-fixture',?)`, session); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.DB().Exec(`INSERT INTO membership_card_platforms VALUES ('platform-fixture','spacexcard','https://cards.example.invalid',?)`, credential); err != nil {
		t.Fatal(err)
	}
	lookups := &cardBillingStub{err: errors.New("an address generator must not be called")}
	requests, executions := 0, 0
	p := &Processor{
		config: config.Config{MaintenancePath: filepath.Join(t.TempDir(), "maintenance")},
		store:  repository, lease: lease, decrypter: crypt, address: lookups,
		requireLeaseFence: true, requireCardBillingBinding: true, now: func() time.Time { return now },
		httpClient: &http.Client{Transport: eligibilityRoundTripper(func(request *http.Request) (*http.Response, error) {
			requests++
			if request.Method != http.MethodGet || request.URL.Host != "cards.example.invalid" || request.URL.Path != "/openapi/v1/cards/77" {
				t.Fatal("unexpected card lookup or external write")
			}
			return eligibilityHTTPResponse(http.StatusOK, `{"code":0,"data":{
			  "card_number":"4111111111111111","cvv":"123","expire":"12/39","status":"ACTIVE",
			  "user_name":"Fixture Holder","card_address":{"address_line_one":"1 Test Street",
			  "address_line_two":"Issuer Unit 2","city":"Fixture City","state":"CA","country":"US","post_code":"00000"}}}`), nil
		})},
		executor: checkoutExecutorFunc(func(_ context.Context, request checkout.Request) (checkout.Result, error) {
			executions++
			if request.Material == nil || request.Material.Billing.Line2 != "Issuer Unit 2" ||
				request.Material.Billing.State != "CA" || request.Material.Billing.Name != "Fixture Holder" ||
				request.Guard == nil || request.Mode != checkout.ModeSessionCheckout {
				t.Fatal("issuer billing or the durable action guard was lost before execution")
			}
			// Do not exercise a payment control; this test stops at the boundary.
			return checkout.Result{}, nil
		}),
	}
	err = p.checkoutExecute(ctx, checkoutExecution{
		Fulfillment: Fulfillment{ID: "fulfillment-fixture", TargetTier: "plus"},
		Stage:       checkoutStage{StageKey: "plus", ProviderKey: "platform-fixture", UpstreamCardID: sql.NullInt64{Int64: 77, Valid: true}},
	}, now)
	if err != nil || requests != 1 || executions != 1 || len(lookups.lookups) != 0 || lookups.unbound != 0 {
		t.Fatalf("card-bound execution path failed: requests=%d executions=%d error=%v", requests, executions, err)
	}
}

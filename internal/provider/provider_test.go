package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSpaceXListCardsNormalizesWithoutRetainingPAN(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("X-API-Key") != "secret" || request.Header.Get("X-App-Id") != "app" {
			t.Fatal("missing SpaceX credentials")
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(response, `{"code":0,"data":{"total":1,"list":[{"id":7,"vm_card_id":"vm-7","product_code":"p1","network":"VISA","issuing_area":"US","available_amount":20.4,"status":"active","card_number":"4242424242424242","created_at":"2026-07-20T00:00:00Z"}]}}`)
	}))
	defer server.Close()
	client, err := NewSpaceXClient(server.Client(), "app", "secret")
	if err != nil {
		t.Fatal(err)
	}
	client.baseURL = server.URL
	total, cards, err := client.ListCards(context.Background(), 1, 20, true)
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(cards) != 1 || cards[0].BIN != "424242" || cards[0].Last4 != "4242" {
		t.Fatalf("unexpected normalized cards: %+v", cards)
	}
	encoded, _ := json.Marshal(cards)
	if string(encoded) == "" || contains(string(encoded), "4242424242424242") {
		t.Fatal("full PAN escaped normalized model")
	}
}

func TestSpaceXRechargeSendsPersistedIdempotencyKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.Header.Get("Idempotency-Key") != "kwr:ORDER-1:recharge:v1" {
			t.Fatalf("unexpected request %s %q", request.Method, request.Header.Get("Idempotency-Key"))
		}
		body, _ := io.ReadAll(request.Body)
		if string(body) != `{"amount":16.4,"card_id":7}` {
			t.Fatalf("body = %s", body)
		}
		_, _ = io.WriteString(response, `{"code":0,"data":{}}`)
	}))
	defer server.Close()
	client, _ := NewSpaceXClient(server.Client(), "", "secret")
	client.baseURL = server.URL
	if err := client.RechargeCard(context.Background(), 7, 16.4, "kwr:ORDER-1:recharge:v1"); err != nil {
		t.Fatal(err)
	}
}

func TestSpaceXFreezeCardSendsFreezeRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/cards/freeze" {
			t.Fatalf("unexpected request %s %s", request.Method, request.URL.Path)
		}
		body, _ := io.ReadAll(request.Body)
		if string(body) != `{"card_id":7,"freeze":true}` {
			t.Fatalf("body = %s", body)
		}
		_, _ = io.WriteString(response, `{"code":0,"msg":"ok"}`)
	}))
	defer server.Close()
	client, _ := NewSpaceXClient(server.Client(), "", "secret")
	client.baseURL = server.URL
	if err := client.FreezeCard(context.Background(), 7); err != nil {
		t.Fatal(err)
	}
}

func TestSpaceXGetCardMaterialValidatesAndReturnsEphemeralFields(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/cards/7" {
			t.Fatalf("path = %s", request.URL.Path)
		}
		_, _ = io.WriteString(response, `{"code":0,"data":{"card_number":"4242424242424242","cvv":"123","expire":"08/29","status":"ACTIVE"}}`)
	}))
	defer server.Close()
	client, _ := NewSpaceXClient(server.Client(), "", "secret")
	client.baseURL = server.URL
	material, err := client.GetCardMaterial(context.Background(), 7, time.Date(2026, 7, 21, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if material.Number != "4242424242424242" || material.CVV != "123" || material.ExpiryMonth != "08" || material.ExpiryYear != "2029" {
		t.Fatalf("unexpected card material: %+v", material)
	}
}

func TestNormalizeCookieEnvelopeBindsSessionIdentity(t *testing.T) {
	raw := []byte(`{"code":0,"data":{"count":1,"email":"buyer@example.com","cookies":[{"domain":".chatgpt.com","hostOnly":false,"httpOnly":true,"name":"__Secure-next-auth.session-token","path":"/","sameSite":"lax","secure":true,"session":true,"storeId":null,"value":"secret"}]}}`)
	cookies, err := normalizeCookieEnvelope(raw, "buyer@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if len(cookies) != 1 || cookies[0].Name != cookieBaseName || cookies[0].Value != "secret" {
		t.Fatalf("unexpected cookies: %+v", cookies)
	}
	if _, err := normalizeCookieEnvelope(raw, "other@example.com"); err == nil {
		t.Fatal("identity mismatch accepted")
	}
}

func TestNormalizeCookieEnvelopeAcceptsDocumentedHTTPSuccessCode(t *testing.T) {
	raw := []byte(`{"code":200,"data":{"count":1,"email":"buyer@example.com","cookies":[{"domain":".chatgpt.com","hostOnly":false,"httpOnly":true,"name":"__Secure-next-auth.session-token","path":"/","sameSite":"lax","secure":true,"session":true,"storeId":null,"value":"secret"}]}}`)
	cookies, err := normalizeCookieEnvelope(raw, "buyer@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if len(cookies) != 1 || cookies[0].Name != cookieBaseName {
		t.Fatalf("unexpected cookies: %+v", cookies)
	}
}

func TestGPTCheckoutAcceptsDocumentedHTTPSuccessCode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer private-token" {
			t.Fatal("missing GPT bearer token")
		}
		_, _ = io.WriteString(response, `{"code":200,"data":{"link":"https://chatgpt.com/checkout/safe"},"message":"ok"}`)
	}))
	defer server.Close()
	client := NewGPTClient(server.Client())
	client.http.Transport = rewriteTransport{target: server.URL, base: http.DefaultTransport}
	url, err := client.CheckoutURL(context.Background(), json.RawMessage(`{"user":{"email":"buyer@example.com"}}`), "private-token")
	if err != nil {
		t.Fatal(err)
	}
	if url != "https://chatgpt.com/checkout/safe" {
		t.Fatalf("checkout URL = %q", url)
	}
}

func TestGPTCheckoutClassifiesSanitizedContractBoundaries(t *testing.T) {
	tests := map[string]struct {
		payload string
		code    string
	}{
		"invalid json":      {payload: `{`, code: "CHECKOUT_BROKER_RESPONSE_INVALID"},
		"invalid code":      {payload: `{"code":"200","data":{"link":"https://chatgpt.com/checkout/safe"}}`, code: "CHECKOUT_BROKER_CODE_INVALID"},
		"business rejected": {payload: `{"code":400,"message":"not exposed"}`, code: "CHECKOUT_BROKER_BUSINESS_REJECTED"},
		"link missing":      {payload: `{"code":200,"data":{}}`, code: "CHECKOUT_BROKER_LINK_MISSING"},
		"link invalid":      {payload: `{"code":200,"data":{"link":"https://example.com/not-allowed"}}`, code: "CHECKOUT_BROKER_LINK_INVALID"},
	}
	for name, item := range tests {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(response, item.payload)
			}))
			defer server.Close()
			client := NewGPTClient(server.Client())
			client.http.Transport = rewriteTransport{target: server.URL, base: http.DefaultTransport}
			_, err := client.CheckoutURL(context.Background(), json.RawMessage(`{"user":{"email":"buyer@example.com"}}`), "private-token")
			requireProviderErrorCode(t, err, item.code)
		})
	}
}

func TestValidateCheckoutURLAllowsOpenAILLCPath(t *testing.T) {
	for _, prefix := range []string{"oaics_", "cs_"} {
		value := "https://chatgpt.com/checkout/openai_llc/" + prefix + "4e33c980620248fb926f384591ae06f1"
		if got, err := validateCheckoutURL(value); err != nil || got != value {
			t.Fatalf("validateCheckoutURL() = %q, %v", got, err)
		}
	}
	if _, err := validateCheckoutURL("https://chatgpt.com/checkout/other_org/oaics_4e33c980620248fb926f384591ae06f1"); err == nil {
		t.Fatal("unexpected checkout organization accepted")
	}
}

func TestSpaceXListCardsRejectsMissingTotal(t *testing.T) {
	client := newSpaceXResponseClient(t, `{"code":0,"data":{"list":[]}}`)
	_, _, err := client.ListCards(context.Background(), 1, 20, false)
	requireProviderErrorCode(t, err, "SPACEXCARD_CONTRACT_DRIFT")
}

func TestSpaceXListCardsRejectsMissingOrNullList(t *testing.T) {
	for name, payload := range map[string]string{
		"missing": `{"code":0,"data":{"total":0}}`,
		"null":    `{"code":0,"data":{"total":0,"list":null}}`,
	} {
		t.Run(name, func(t *testing.T) {
			client := newSpaceXResponseClient(t, payload)
			_, _, err := client.ListCards(context.Background(), 1, 20, false)
			requireProviderErrorCode(t, err, "SPACEXCARD_CONTRACT_DRIFT")
		})
	}
}

func TestSpaceXListProductsRejectsMissingFees(t *testing.T) {
	tests := map[string]string{
		"open fee":     `{"code":0,"data":[{"product_code":"p1","recharge_fee":0,"min_amount":0,"max_amount":100}]}`,
		"recharge fee": `{"code":0,"data":[{"product_code":"p1","open_fee":0,"min_amount":0,"max_amount":100}]}`,
	}
	for name, response := range tests {
		t.Run(name, func(t *testing.T) {
			client := newSpaceXResponseClient(t, response)
			_, err := client.ListProducts(context.Background())
			requireProviderErrorCode(t, err, "SPACEXCARD_CONTRACT_DRIFT")
		})
	}
}

func TestSpaceXOpenCardRejectsMissingRequiredNumericFields(t *testing.T) {
	tests := map[string]string{
		"id":               `{"code":0,"data":{"vm_card_id":"vm-7","product_code":"p1","available_amount":20,"status":"active","open_fee":1}}`,
		"available amount": `{"code":0,"data":{"id":7,"vm_card_id":"vm-7","product_code":"p1","status":"active","open_fee":1}}`,
		"open fee":         `{"code":0,"data":{"id":7,"vm_card_id":"vm-7","product_code":"p1","available_amount":20,"status":"active"}}`,
	}
	for name, response := range tests {
		t.Run(name, func(t *testing.T) {
			client := newSpaceXResponseClient(t, response)
			_, err := client.OpenCard(context.Background(), OpenCardInput{ProductCode: "p1", FirstName: "A", LastName: "B", InitAmount: 20}, "open-card-key")
			requireProviderErrorCode(t, err, "SPACEXCARD_CONTRACT_DRIFT")
		})
	}
}

func TestSpaceXPriceSignalsRejectMissingFound(t *testing.T) {
	client := newSpaceXResponseClient(t, `{"code":0,"data":[{"tier":"plus","label":"Plus","min_usd":1,"max_usd":2,"amount":0,"time":""},{"tier":"x5","label":"X5","min_usd":1,"max_usd":2,"amount":0,"time":"","found":false},{"tier":"x20","label":"X20","min_usd":1,"max_usd":2,"amount":0,"time":"","found":false}]}`)
	_, err := client.GetOpenAIPayments(context.Background(), 7, nil)
	requireProviderErrorCode(t, err, "SPACEXCARD_CONTRACT_DRIFT")
}

func TestMembershipProviderUsesObjectToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		if string(body) != `{"token":{"access_token":"x","user":{"email":"a@example.com"}}}` {
			t.Fatalf("body = %s", body)
		}
		_, _ = io.WriteString(response, `{"code":200,"data":{"account_type":"free","is_overdue":false,"is_delinquent":false}}`)
	}))
	defer server.Close()
	client := &MembershipClient{http: server.Client()}
	old := MembershipStateURL
	_ = old
	// MembershipStateURL is fixed in production; use a request-rewriting transport in the test.
	client.http.Transport = rewriteTransport{target: server.URL, base: http.DefaultTransport}
	if _, err := client.Fetch(context.Background(), json.RawMessage(`{"access_token":"x","user":{"email":"a@example.com"}}`)); err != nil {
		t.Fatal(err)
	}
}

func TestRenewalCancelRejectsMissingEnvelopeCode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(response, `{"data":{"auto_renew":false}}`)
	}))
	defer server.Close()
	client := NewRenewalClient(server.Client())
	client.http.Transport = rewriteTransport{target: server.URL, base: http.DefaultTransport}
	err := client.Cancel(context.Background(), json.RawMessage(`{"access_token":"x"}`), "token")
	requireProviderErrorCode(t, err, "RENEWAL_CANCEL_RESPONSE_INVALID")
}

type rewriteTransport struct {
	target string
	base   http.RoundTripper
}

func (transport rewriteTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	clone := request.Clone(request.Context())
	parsed, _ := http.NewRequest(request.Method, transport.target, request.Body)
	clone.URL = parsed.URL
	return transport.base.RoundTrip(clone)
}

func contains(value, part string) bool {
	for index := 0; index+len(part) <= len(value); index++ {
		if value[index:index+len(part)] == part {
			return true
		}
	}
	return false
}

func newSpaceXResponseClient(t *testing.T, payload string) *SpaceXClient {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(response, payload)
	}))
	t.Cleanup(server.Close)
	client, err := NewSpaceXClient(server.Client(), "", "secret")
	if err != nil {
		t.Fatal(err)
	}
	client.baseURL = server.URL
	return client
}

func requireProviderErrorCode(t *testing.T, err error, code string) {
	t.Helper()
	var providerError *Error
	if !errors.As(err, &providerError) || providerError.Code() != code {
		t.Fatalf("error = %v, want provider code %s", err, code)
	}
}

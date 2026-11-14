package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

func zovoFixtureClient(t *testing.T, handle func(*http.Request) (*http.Response, error)) *ZovoClient {
	t.Helper()
	client, err := NewZovoClient(&http.Client{Transport: factoryRoundTripper(handle)}, "fixture-app", "fixture-secret")
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func zovoResponse(status int, body string) *http.Response {
	response := factoryResponse(body)
	response.StatusCode = status
	return response
}

func TestZovoFactoryUsesItsOwnKindOriginAndConfigurationKey(t *testing.T) {
	calls := 0
	client := &http.Client{Transport: factoryRoundTripper(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.String() != ZovoCardBaseURL+"/balance" || r.Header.Get("X-API-Key") != "fixture-secret" || r.Header.Get("X-App-Id") != "fixture-app" {
			t.Fatal("Zovo request used a different origin or credentials")
		}
		return zovoResponse(200, `{"code":0,"data":{"balance":50,"spendable_balance":30,"account_reserve_amount":20,"currency":"USD"}}`), nil
	})}
	platform, err := NewCardPlatform(client, CardPlatformConfig{Key: "zovo-primary", Kind: CardPlatformZovo,
		CredentialJSON: json.RawMessage(`{"appId":"fixture-app","appSecret":"fixture-secret"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 0 || platform.Key() != "zovo-primary" || platform.Kind() != CardPlatformZovo {
		t.Fatal("constructor performed I/O or lost configuration identity")
	}
	if _, ok := platform.(CardSnapshotReader); !ok {
		t.Fatal("configuration wrapper hid snapshot capability")
	}
	policy, ok := platform.(FundingReplayPolicy)
	if !ok || policy.AllowsFundingReplay("open") || policy.AllowsFundingReplay("recharge") || platform.Capabilities().RechargeIdempotent {
		t.Fatal("2xx-only caching was incorrectly advertised as safe funding replay")
	}
	balance, err := platform.GetBalance(context.Background())
	if err != nil || balance.Balance != 30 || balance.Currency != "USD" || calls != 1 {
		t.Fatalf("spendable balance: %+v %v", balance, err)
	}
}

func TestZovoReadsUseOnlyBasicEndpointsAndNoMembershipFeed(t *testing.T) {
	seen := map[string]int{}
	client := zovoFixtureClient(t, func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet {
			t.Fatal("read produced a mutation")
		}
		seen[r.URL.Path]++
		switch r.URL.Path {
		case "/openapi/v1/products":
			return zovoResponse(200, `{"code":0,"data":[{"product_code":"fixture-product","open_fee":1,"recharge_fee":0,"min_amount":1,"max_amount":100}]}`), nil
		case "/openapi/v1/cards":
			if r.URL.Query().Get("sync") != "1" || r.URL.Query().Get("page") != "2" || r.URL.Query().Get("page_size") != "10" {
				t.Fatal("requested page/freshness lost")
			}
			return zovoResponse(200, `{"code":0,"data":{"total":0,"list":[]}}`), nil
		case "/openapi/v1/cards/7/transactions":
			return zovoResponse(200, `{"code":0,"data":[]}`), nil
		default:
			t.Fatal("request outside basic card API")
			return nil, errors.New("unexpected")
		}
	})
	if _, err := client.ListProducts(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := client.ListCards(context.Background(), 2, 10, true); err != nil {
		t.Fatal(err)
	}
	if _, err := client.ListTransactions(context.Background(), 7, 1, 50); err != nil {
		t.Fatal(err)
	}
	_, err := client.GetOpenAIPayments(context.Background(), 7, nil)
	requireProviderErrorCode(t, err, "CARD_PLATFORM_CAPABILITY_UNSUPPORTED")
	if len(seen) != 3 {
		t.Fatal("membership price feed made another request")
	}
}

func zovoSnapshotBody(extra string) string {
	return `{"code":0,"data":{"card_id":"fixture-card-7","card_number":"4242424242424242","cvv":"123","expire":"08/29","status":"ACTIVE","user_name":"Registered Holder","available_amount":25` + extra + `}}`
}

func zovoBindingListBody() string {
	return `{"code":0,"data":{"total":1,"list":[{"id":7,"vm_card_id":"fixture-card-7","card_number":"4242424242424242"}]}}`
}

func TestZovoSnapshotKeepsUnknownCurrencyAndBillingAndExplicitRestrictions(t *testing.T) {
	for _, tc := range []struct {
		name, extra string
		known       bool
		currency    string
		blocked     *bool
	}{
		{name: "missing facts"},
		{name: "empty rules", extra: `,"restricted_merchants":[],"card_address":{}`, known: true},
		{name: "null rules", extra: `,"restricted_merchants":null`},
		{name: "explicit USD", extra: `,"currency":"usd","restricted_merchants":["GOOGLE CHATGPT"],"google_chatgpt_blocked":true`, known: true, currency: "USD"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			client := zovoFixtureClient(t, func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.Path == "/openapi/v1/cards" {
					return zovoResponse(200, zovoBindingListBody()), nil
				}
				if r.URL.Path != "/openapi/v1/cards/7" {
					t.Fatal("snapshot read wrong card")
				}
				return zovoResponse(200, zovoSnapshotBody(tc.extra)), nil
			})
			snapshot, err := client.GetCardSnapshot(context.Background(), 7, time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC))
			if err != nil {
				t.Fatal(err)
			}
			if calls != 2 || !snapshot.CardBindingVerified || snapshot.UpstreamCardID != 7 || snapshot.Currency != tc.currency || snapshot.AvailableAmount != 25 || snapshot.MerchantRestrictionsKnown != tc.known || snapshot.Material.Billing != nil || snapshot.Material.HolderName != "Registered Holder" {
				t.Fatal("snapshot invented or lost facts")
			}
			if tc.name == "explicit USD" && (snapshot.GoogleChatGPTBlocked == nil || !*snapshot.GoogleChatGPTBlocked || len(snapshot.RestrictedMerchants) != 1) {
				t.Fatal("merchant restriction lost")
			}
			encoded, err := json.Marshal(snapshot)
			if err != nil {
				t.Fatal(err)
			}
			for _, value := range []string{string(encoded), fmt.Sprintf("%+v", snapshot), fmt.Sprintf("%#v", snapshot)} {
				if strings.Contains(value, "4242424242424242") || strings.Contains(value, "Registered Holder") || strings.Contains(value, `"CVV"`) {
					t.Fatal("snapshot formatting exposed material")
				}
			}
		})
	}
}

func TestZovoSnapshotRejectsMalformedFacts(t *testing.T) {
	for _, extra := range []string{`,"restricted_merchants":{}`, `,"restricted_merchants":[""]`, `,"restricted_merchants":["x\n"]`, `,"google_chatgpt_blocked":"false"`, `,"currency":"US"`, `,"available_amount":null`, `,"available_amount":-1`} {
		t.Run(extra, func(t *testing.T) {
			client := zovoFixtureClient(t, func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/openapi/v1/cards" {
					return zovoResponse(200, zovoBindingListBody()), nil
				}
				return zovoResponse(200, zovoSnapshotBody(extra)), nil
			})
			_, err := client.GetCardSnapshot(context.Background(), 7, time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC))
			if err == nil {
				t.Fatal("malformed snapshot accepted")
			}
		})
	}
}

func TestZovoFundingRequestValidationStopsBeforeNetwork(t *testing.T) {
	calls := 0
	client := zovoFixtureClient(t, func(*http.Request) (*http.Response, error) { calls++; return nil, errors.New("unexpected") })
	input := OpenCardInput{ProductCode: "fixture-product", FirstName: "Registered", LastName: "Holder", InitAmount: 20}
	for _, key := range []string{"", strings.Repeat("x", 81), "has space", "line\nend", "utf8-卡"} {
		if _, err := client.OpenCard(context.Background(), input, key); err == nil {
			t.Fatal("invalid open key accepted")
		}
		if err := client.RechargeCard(context.Background(), 7, 20, key); err == nil {
			t.Fatal("invalid recharge key accepted")
		}
	}
	for _, amount := range []float64{0, -1, math.NaN(), math.Inf(1)} {
		input.InitAmount = amount
		if _, err := client.OpenCard(context.Background(), input, "stable:key"); err == nil {
			t.Fatal("invalid open amount accepted")
		}
		if err := client.RechargeCard(context.Background(), 7, amount, "stable:key"); err == nil {
			t.Fatal("invalid recharge amount accepted")
		}
	}
	if calls != 0 {
		t.Fatal("invalid funding request reached network")
	}
}

func TestZovoFundingUsesOneBasicPOSTAndPreservesIdempotencyKey(t *testing.T) {
	var calls []string
	client := zovoFixtureClient(t, func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPost || r.Header.Get("Idempotency-Key") != "immutable:key-1" || r.Header.Get("X-API-Key") != "fixture-secret" || r.GetBody != nil {
			t.Fatal("write request contract changed")
		}
		calls = append(calls, r.URL.Path)
		var body map[string]any
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			t.Fatal("invalid request body")
		}
		switch r.URL.Path {
		case "/openapi/v1/cards/open":
			if body["product_code"] != "fixture-product" || body["first_name"] != "Registered" || body["last_name"] != "Holder" || body["init_amount"] != 20.0 {
				t.Fatal("open parameters changed")
			}
			return zovoResponse(200, `{"code":0,"data":{"id":7,"vm_card_id":"fixture-card","product_code":"fixture-product","available_amount":20,"status":"ACTIVE","open_fee":1}}`), nil
		case "/openapi/v1/cards/recharge":
			if body["card_id"] != 7.0 || body["amount"] != 5.0 {
				t.Fatal("recharge parameters changed")
			}
			return zovoResponse(200, `{"code":0,"msg":"充值成功"}`), nil
		default:
			t.Fatal("write outside basic card API")
			return nil, errors.New("unexpected")
		}
	})
	card, err := client.OpenCard(context.Background(), OpenCardInput{ProductCode: "fixture-product", FirstName: "Registered", LastName: "Holder", InitAmount: 20}, "immutable:key-1")
	if err != nil || card.UpstreamCardID != 7 {
		t.Fatalf("open failed: %v", err)
	}
	if err := client.RechargeCard(context.Background(), 7, 5, "immutable:key-1"); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 {
		t.Fatal("write retried")
	}
}

func TestZovoUnknownWriteResponsesNeverClaimNoWriteOrRetry(t *testing.T) {
	for _, tc := range []struct {
		name           string
		status         int
		body           string
		transportError bool
	}{
		{"pending HTTP400", 400, `{"code":400,"error_code":"request_failed","msg":"充值结果确认中"}`, false},
		{"pending HTTP202", 202, `{"code":0,"data":{"pending":true}}`, false},
		{"pending HTTP200", 200, `{"code":0,"data":{"pending":true}}`, false},
		{"in progress", 409, `{"code":409}`, false},
		{"server failure", 500, `{"code":500,"error_code":"internal_error"}`, false},
		{"rate limited", 429, `{"code":429}`, false},
		{"malformed JSON", 200, `{"code":0`, false},
		{"contradictory HTTP status", 500, `{"code":0}`, false},
		{"business failure in HTTP200", 200, `{"code":400,"error_code":"request_failed"}`, false},
		{"transport lost", 0, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			client := zovoFixtureClient(t, func(*http.Request) (*http.Response, error) {
				calls++
				if tc.transportError {
					return nil, errors.New("secret-url-marker")
				}
				return zovoResponse(tc.status, tc.body), nil
			})
			err := client.RechargeCard(context.Background(), 7, 5, "immutable:key-1")
			var typed *Error
			if !errors.As(err, &typed) || typed.Code() != "ZOVOCARD_WRITE_OUTCOME_UNCERTAIN" || typed.KnownNoWrite || typed.Retryable || calls != 1 {
				t.Fatalf("unknown outcome classified incorrectly: %v calls=%d", err, calls)
			}
			if strings.Contains(err.Error(), "secret-url-marker") {
				t.Fatal("transport error leaked details")
			}
		})
	}
}

func TestZovoKnownPreWriteRejectionsAreExplicit(t *testing.T) {
	for _, tc := range []struct {
		status     int
		body, code string
	}{
		{401, `{}`, "ZOVOCARD_AUTH_FAILED"}, {403, `{}`, "ZOVOCARD_ACCESS_DENIED"},
		{400, `{"code":400,"error_code":"insufficient_balance"}`, "ZOVOCARD_OPERATION_REJECTED"},
		{400, `{"code":400,"error_code":"invalid_argument"}`, "ZOVOCARD_OPERATION_REJECTED"},
	} {
		client := zovoFixtureClient(t, func(*http.Request) (*http.Response, error) { return zovoResponse(tc.status, tc.body), nil })
		err := client.RechargeCard(context.Background(), 7, 5, "stable:key")
		var typed *Error
		if !errors.As(err, &typed) || typed.Code() != tc.code || !typed.KnownNoWrite || typed.Retryable {
			t.Fatalf("bad pre-write classification: %v", err)
		}
	}
}

func TestZovoRedirectNeverForwardsCredentialsOrClaimsNoWrite(t *testing.T) {
	calls := 0
	client := zovoFixtureClient(t, func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Host != "zovocard.com" {
			t.Fatal("credential-bearing redirect followed")
		}
		response := zovoResponse(http.StatusTemporaryRedirect, `{}`)
		response.Header.Set("Location", "https://foreign.invalid/receive")
		return response, nil
	})
	err := client.RechargeCard(context.Background(), 7, 5, "stable:key")
	var typed *Error
	if !errors.As(err, &typed) || typed.Code() != "ZOVOCARD_REDIRECT_REFUSED" || typed.KnownNoWrite || calls != 1 {
		t.Fatal("unsafe redirect classification")
	}
}

func TestZovoReadErrorsDoNotExposeEndpointOrCredentials(t *testing.T) {
	client := zovoFixtureClient(t, func(*http.Request) (*http.Response, error) { return nil, errors.New("sensitive-upstream-detail") })
	_, err := client.GetBalance(context.Background())
	if err == nil || strings.Contains(err.Error(), "sensitive") || strings.Contains(err.Error(), "https") {
		t.Fatal("read error is not redacted")
	}
	_, err = NewCardPlatform(&http.Client{}, CardPlatformConfig{Key: "zovo", Kind: CardPlatformZovo, BaseURL: "http://invalid", CredentialJSON: json.RawMessage(`{"appSecret":"fixture"}`)})
	requireProviderErrorCode(t, err, "ZOVOCARD_CONFIGURATION_INVALID")
}

func TestZovoSnapshotConfigurationWrapperUsesTheSelectedAccount(t *testing.T) {
	client := &http.Client{Transport: factoryRoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "sandbox.zovocard.com" || r.Header.Get("X-API-Key") != "sandbox-key" {
			t.Fatal("snapshot escaped selected platform account")
		}
		if r.URL.Path == "/openapi/v1/cards" {
			return zovoResponse(200, zovoBindingListBody()), nil
		}
		return zovoResponse(200, zovoSnapshotBody(`,"restricted_merchants":[]`)), nil
	})}
	platform, err := NewCardPlatform(client, CardPlatformConfig{Key: "sandbox-cards", Kind: CardPlatformZovo, BaseURL: ZovoCardSandboxBaseURL, CredentialJSON: json.RawMessage(`{"appSecret":"sandbox-key"}`)})
	if err != nil {
		t.Fatal(err)
	}
	reader := platform.(CardSnapshotReader)
	if _, err := reader.GetCardSnapshot(context.Background(), 7, time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	if platform.Key() != "sandbox-cards" {
		t.Fatal("snapshot lost account binding")
	}
}

func TestZovoOversizedWriteReplyIsUnknown(t *testing.T) {
	client := zovoFixtureClient(t, func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(strings.Repeat("x", maxSpaceXResponse+1)))}, nil
	})
	err := client.RechargeCard(context.Background(), 7, 5, "stable:key")
	requireProviderErrorCode(t, err, "ZOVOCARD_WRITE_OUTCOME_UNCERTAIN")
}

func TestZovoSnapshotRejectsDifferentOrMissingIssuerCardIdentity(t *testing.T) {
	for _, tc := range []struct{ name, extra string }{
		{"different issuer card", `,"card_id":"another-card"`},
		{"missing issuer card", `,"card_id":""`},
		{"different PAN and CVV", `,"card_number":"5555555555554444","cvv":"987"`},
		{"different returned local ID", `,"id":8`},
		{"malformed PAN hidden by digit stripping", `,"card_number":"garbage4242424242424242"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			listCalls, detailCalls := 0, 0
			client := zovoFixtureClient(t, func(r *http.Request) (*http.Response, error) {
				switch r.URL.Path {
				case "/openapi/v1/cards":
					listCalls++
					return zovoResponse(200, zovoBindingListBody()), nil
				case "/openapi/v1/cards/7":
					detailCalls++
					return zovoResponse(200, zovoSnapshotBody(tc.extra)), nil
				default:
					t.Fatal("unexpected read")
					return nil, errors.New("unexpected")
				}
			})
			snapshot, err := client.GetCardSnapshot(context.Background(), 7, time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC))
			requireProviderErrorCode(t, err, "ZOVOCARD_CARD_BINDING_MISMATCH")
			if listCalls != 1 || detailCalls != 1 || snapshot.CardBindingVerified || snapshot.Material.Number != "" || snapshot.Material.CVV != "" {
				t.Fatal("wrong card material escaped binding check")
			}
		})
	}
}

func TestZovoBindingLookupRejectsMissingDuplicateOrIncompleteList(t *testing.T) {
	for _, tc := range []struct{ name, body, code string }{
		{"not found", `{"code":0,"data":{"total":0,"list":[]}}`, "ZOVOCARD_CARD_NOT_FOUND"},
		{"duplicate ID", `{"code":0,"data":{"total":2,"list":[{"id":7,"vm_card_id":"fixture-card-7","card_number":"4242424242424242"},{"id":7,"vm_card_id":"other","card_number":"5555555555554444"}]}}`, "ZOVOCARD_CONTRACT_DRIFT"},
		{"missing issuer ID", `{"code":0,"data":{"total":1,"list":[{"id":7,"card_number":"4242424242424242"}]}}`, "ZOVOCARD_CARD_BINDING_UNCONFIRMED"},
		{"masked card number", `{"code":0,"data":{"total":1,"list":[{"id":7,"vm_card_id":"fixture-card-7","card_number":"424242******4242"}]}}`, "ZOVOCARD_CARD_BINDING_UNCONFIRMED"},
		{"incomplete list", `{"code":0,"data":{"total":3,"list":[{"id":8,"vm_card_id":"other","card_number":"5555555555554444"}]}}`, "ZOVOCARD_CARD_LOOKUP_INCOMPLETE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			client := zovoFixtureClient(t, func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.Path != "/openapi/v1/cards" {
					t.Fatal("unbound card detail was requested")
				}
				return zovoResponse(200, tc.body), nil
			})
			_, err := client.GetCardSnapshot(context.Background(), 7, time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC))
			requireProviderErrorCode(t, err, tc.code)
			if calls != 1 {
				t.Fatal("invalid catalog was retried")
			}
		})
	}
}

func zovoBindingPageBody(t *testing.T, page, total, count int) string {
	t.Helper()
	items := make([]map[string]any, 0, count)
	for i := 0; i < count; i++ {
		id := (page-1)*100 + i + 1
		items = append(items, map[string]any{"id": id, "vm_card_id": "fixture-card-" + strconv.Itoa(id), "card_number": "4242424242424242"})
	}
	raw, err := json.Marshal(map[string]any{"code": 0, "data": map[string]any{"total": total, "list": items}})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestZovoBindingLookupUsesDocumentedPaginationAndStopsAtFoundCard(t *testing.T) {
	listCalls, detailCalls := 0, 0
	client := zovoFixtureClient(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/openapi/v1/cards" {
			listCalls++
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			if r.URL.Query().Get("page_size") != "100" || len(r.URL.Query()) != 2 || page != listCalls {
				t.Fatal("undocumented or repeated lookup parameters")
			}
			count := 100
			if page == 2 {
				count = 1
			}
			return zovoResponse(200, zovoBindingPageBody(t, page, 101, count)), nil
		}
		detailCalls++
		if r.URL.Path != "/openapi/v1/cards/101" {
			t.Fatal("detail used wrong local ID")
		}
		return zovoResponse(200, zovoSnapshotBody(`,"card_id":"fixture-card-101","cvv":"987"`)), nil
	})
	snapshot, err := client.GetCardSnapshot(context.Background(), 101, time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.CardBindingVerified || snapshot.UpstreamCardID != 101 || snapshot.Material.CVV != "987" || listCalls != 2 || detailCalls != 1 {
		t.Fatal("pagination did not preserve exact selected-card material")
	}
}

func TestZovoBindingLookupTerminatesAtPageLimitWithoutReadingUnboundDetail(t *testing.T) {
	calls := 0
	client := zovoFixtureClient(t, func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Path != "/openapi/v1/cards" {
			t.Fatal("unbound detail requested")
		}
		return zovoResponse(200, zovoBindingPageBody(t, calls, zovoCardLookupMaxPages*100+1, 100)), nil
	})
	_, err := client.GetCardSnapshot(context.Background(), zovoCardLookupMaxPages*100+1, time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC))
	requireProviderErrorCode(t, err, "ZOVOCARD_CARD_LOOKUP_INCOMPLETE")
	if calls != zovoCardLookupMaxPages {
		t.Fatalf("lookup requests=%d", calls)
	}
}

func TestZovoBoundCardStillRejectsUnknownStatusExpiryOrBalance(t *testing.T) {
	for _, extra := range []string{`,"status":""`, `,"status":"FROZEN"`, `,"expire":""`, `,"expire":"01/20"`, `,"available_amount":null`} {
		client := zovoFixtureClient(t, func(r *http.Request) (*http.Response, error) {
			if r.URL.Path == "/openapi/v1/cards" {
				return zovoResponse(200, zovoBindingListBody()), nil
			}
			return zovoResponse(200, zovoSnapshotBody(extra)), nil
		})
		snapshot, err := client.GetCardSnapshot(context.Background(), 7, time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC))
		if err == nil || snapshot.CardBindingVerified || snapshot.Material.Number != "" {
			t.Fatal("binding alone made invalid card material usable")
		}
	}
}

package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const efunCatalogFixture = `{"success":true,"message":"ok","data":{"cardTypes":[{"id":1,"cardType":"559666","baseCardFeeRmb":"10.00","feeRate":"0.0200","minAmount":"5.00","maxAmount":"5000.00"}],"exchangeRate":7.3,"discount":{"levelName":"diamond","discountPercent":80},"purchaseEnabled":true,"validityOptions":[]}}`

func TestEfunCatalogAndBalanceNormalizeCNYToUSD(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("X-API-Key") != "efk_test" {
			t.Fatal("missing EfunCard API key")
		}
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/account/balance":
			_, _ = io.WriteString(response, `{"success":true,"data":{"balance":"730.00","currency":"CNY"}}`)
		case "/card-types":
			_, _ = io.WriteString(response, efunCatalogFixture)
		default:
			t.Fatalf("unexpected path %s", request.URL.Path)
		}
	}))
	defer server.Close()
	client, err := NewEfunCardClient(server.Client(), server.URL, "efk_test")
	if err != nil {
		t.Fatal(err)
	}
	balance, err := client.GetBalance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	products, err := client.ListProducts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if balance.Currency != "USD" || balance.Balance != 100 || len(products) != 1 {
		t.Fatalf("unexpected normalized contract: balance=%+v products=%+v", balance, products)
	}
	product := products[0]
	if product.ProductCode != "559666" || !product.OpenEnabled || math.Abs(product.OpenFee-(8.0/7.3)) > 0.0001 || product.OpenFeeRate != 0.016 {
		t.Fatalf("unexpected product: %+v", product)
	}
}

func TestEfunCardsAndTransactionsNormalizeWithoutRetainingPAN(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/cards":
			_, _ = io.WriteString(response, `{"success":true,"data":{"cards":[{"id":101,"cardNumber":"424242******4242","cardType":"559666","status":"active","cardBalance":"100.00","createdAt":"2026-02-01T12:00:00.000Z"}],"total":1,"page":1,"pageSize":20}}`)
		case "/cards/101":
			_, _ = io.WriteString(response, `{"success":true,"data":{"id":101,"cardNumber":"4242424242424242","cardType":"559666","cvv":"123","expiryMonth":"12","expiryYear":"2028","status":"active","cardBalance":"100.00"}}`)
		case "/cards/101/transactions":
			_, _ = io.WriteString(response, `{"success":true,"data":{"transactions":[{"request_id":"charge-1","amount":"-19.99","currency":"USD","merchant":"OPENAI *CHATGPT","transaction_time":"2026-05-18 10:30:00","status":"completed"},{"request_id":"refund-1","amount":"19.99","currency":"USD","merchant":"OPENAI *CHATGPT","transaction_time":"2026-05-19 10:30:00","status":"completed"}],"total":2,"page":1,"pageSize":20}}`)
		default:
			t.Fatalf("unexpected path %s", request.URL.Path)
		}
	}))
	defer server.Close()
	client, _ := NewEfunCardClient(server.Client(), server.URL, "efk_test")
	total, cards, err := client.ListCards(context.Background(), 1, 20, false)
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(cards) != 1 || cards[0].Last4 != "4242" || cards[0].ProductCode != "559666" {
		t.Fatalf("unexpected cards: %+v", cards)
	}
	encoded, _ := json.Marshal(cards)
	if strings.Contains(string(encoded), "4242424242424242") {
		t.Fatal("full PAN escaped normalized card model")
	}
	material, err := client.GetCardMaterial(context.Background(), 101, time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC))
	if err != nil || material.Number != "4242424242424242" || material.CVV != "123" {
		t.Fatalf("material=%+v err=%v", material, err)
	}
	transactions, err := client.ListTransactions(context.Background(), 101, 1, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(transactions) != 2 || transactions[0].Type != "Settlement" || transactions[0].SettleAmount != 19.99 ||
		transactions[1].Type != "Refund" || transactions[1].SettleAmount != 19.99 {
		t.Fatalf("unexpected transactions: %+v", transactions)
	}
	prices, err := client.GetOpenAIPayments(context.Background(), 101, transactions)
	if err != nil || len(prices) != 3 || !prices[0].Found || prices[0].Amount != 19.99 {
		t.Fatalf("unexpected prices: %+v err=%v", prices, err)
	}
}

func TestEfunOpenCardUsesIdempotencyAndValidatesAcceptedResult(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/card-types":
			_, _ = io.WriteString(response, efunCatalogFixture)
		case request.Method == http.MethodPost && request.URL.Path == "/cards/purchase":
			if len(request.Header.Get("X-Idempotency-Key")) < 16 {
				t.Fatal("missing normalized idempotency key")
			}
			body, _ := io.ReadAll(request.Body)
			if !strings.Contains(string(body), `"cardTypeId":1`) || !strings.Contains(string(body), `"openCardAmount":100`) {
				t.Fatalf("unexpected purchase body %s", body)
			}
			_, _ = io.WriteString(response, `{"success":true,"data":{"cards":[{"id":102,"status":"pending"}],"totalCost":"749.68"}}`)
		case request.Method == http.MethodGet && request.URL.Path == "/cards/102":
			_, _ = io.WriteString(response, `{"success":true,"data":{"id":102,"cardNumber":"4242424242424242","cardType":"559666","cvv":"123","expiryMonth":"12","expiryYear":"2028","status":"active","cardBalance":"100.00"}}`)
		default:
			t.Fatalf("unexpected request %s %s", request.Method, request.URL.Path)
		}
	}))
	defer server.Close()
	client, _ := NewEfunCardClient(server.Client(), server.URL, "efk_test")
	result, err := client.OpenCard(context.Background(), OpenCardInput{ProductCode: "559666", InitAmount: 100}, "short-key")
	if err != nil {
		t.Fatal(err)
	}
	if result.UpstreamCardID != 102 || result.Status != "ACTIVE" || result.AvailableAmount != 100 {
		t.Fatalf("unexpected open result: %+v", result)
	}
}

func TestEfunPostWritePollFailureIsOutcomeUnknown(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch {
		case request.URL.Path == "/card-types":
			_, _ = io.WriteString(response, efunCatalogFixture)
		case request.Method == http.MethodPost && request.URL.Path == "/cards/purchase":
			_, _ = io.WriteString(response, `{"success":true,"data":{"cards":[{"id":102,"status":"pending"}],"totalCost":"749.68"}}`)
		case request.URL.Path == "/cards/102":
			response.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(response, `{"success":false,"message":"expired"}`)
		default:
			t.Fatalf("unexpected request %s %s", request.Method, request.URL.Path)
		}
	}))
	defer server.Close()
	client, _ := NewEfunCardClient(server.Client(), server.URL, "efk_test")
	_, err := client.OpenCard(context.Background(), OpenCardInput{ProductCode: "559666", InitAmount: 100}, "short-key")
	requireProviderErrorCode(t, err, "EFUNCARD_FUNDING_OUTCOME_UNKNOWN")
	var providerError *Error
	if !errors.As(err, &providerError) || providerError.KnownNoWrite {
		t.Fatalf("post-write failure must not be classified as known-no-write: %v", err)
	}
}

func TestEfunRateLimiterEnforcesDocumentedWindows(t *testing.T) {
	limiter := &efunRateLimiter{}
	now := time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC)
	for index := 0; index < efunPurchaseRateLimit; index++ {
		if !limiter.allow(http.MethodPost, "/cards/purchase", now) {
			t.Fatalf("purchase request %d was limited early", index+1)
		}
	}
	if limiter.allow(http.MethodPost, "/cards/purchase", now) {
		t.Fatal("eleventh purchase request must be locally limited")
	}
	if !limiter.allow(http.MethodPost, "/cards/purchase", now.Add(time.Minute+time.Nanosecond)) {
		t.Fatal("purchase window did not expire")
	}

	limiter = &efunRateLimiter{}
	for index := 0; index < efunRefreshRateLimit; index++ {
		if !limiter.allow(http.MethodPost, "/cards/101/refresh-balance", now) {
			t.Fatalf("refresh request %d was limited early", index+1)
		}
	}
	if limiter.allow(http.MethodPost, "/cards/101/refresh-balance", now) {
		t.Fatal("thirty-first refresh request must be locally limited")
	}

	limiter = &efunRateLimiter{}
	for index := 0; index < efunGlobalRateLimit; index++ {
		if !limiter.allow(http.MethodGet, "/cards", now) {
			t.Fatalf("global request %d was limited early", index+1)
		}
	}
	if limiter.allow(http.MethodGet, "/cards", now) {
		t.Fatal("sixty-first global request must be locally limited")
	}
}

func TestEfunRechargeValidatesReceiptBeforePolling(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/cards/101":
			_, _ = io.WriteString(response, `{"success":true,"data":{"id":101,"cardType":"559666","status":"active","cardBalance":"100.00"}}`)
		case request.Method == http.MethodGet && request.URL.Path == "/card-types":
			_, _ = io.WriteString(response, efunCatalogFixture)
		case request.Method == http.MethodPost && request.URL.Path == "/cards/101/recharge":
			_, _ = io.WriteString(response, `{"success":true,"data":{"taskId":"2438","rechargeAmountUsd":10,"serviceFeeUsd":0.16,"totalCostCny":74.17}}`)
		case request.Method == http.MethodPost && request.URL.Path == "/cards/101/refresh-balance":
			_, _ = io.WriteString(response, `{"success":true,"data":{"cardBalance":"110.00"}}`)
		default:
			t.Fatalf("unexpected request %s %s", request.Method, request.URL.Path)
		}
	}))
	defer server.Close()
	client, _ := NewEfunCardClient(server.Client(), server.URL, "efk_recharge_valid")
	if err := client.RechargeCard(context.Background(), 101, 10, "ignored"); err != nil {
		t.Fatal(err)
	}
}

func TestEfunRechargeReceiptMismatchIsOutcomeUnknown(t *testing.T) {
	refreshCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/cards/101":
			_, _ = io.WriteString(response, `{"success":true,"data":{"id":101,"cardType":"559666","status":"active","cardBalance":"100.00"}}`)
		case request.Method == http.MethodGet && request.URL.Path == "/card-types":
			_, _ = io.WriteString(response, efunCatalogFixture)
		case request.Method == http.MethodPost && request.URL.Path == "/cards/101/recharge":
			_, _ = io.WriteString(response, `{"success":true,"data":{"taskId":"2438","rechargeAmountUsd":11,"serviceFeeUsd":0.16,"totalCostCny":74.17}}`)
		case request.Method == http.MethodPost && request.URL.Path == "/cards/101/refresh-balance":
			refreshCalls++
			_, _ = io.WriteString(response, `{"success":true,"data":{"cardBalance":"110.00"}}`)
		default:
			t.Fatalf("unexpected request %s %s", request.Method, request.URL.Path)
		}
	}))
	defer server.Close()
	client, _ := NewEfunCardClient(server.Client(), server.URL, "efk_recharge_mismatch")
	err := client.RechargeCard(context.Background(), 101, 10, "ignored")
	requireProviderErrorCode(t, err, "EFUNCARD_FUNDING_OUTCOME_UNKNOWN")
	if refreshCalls != 0 {
		t.Fatal("receipt mismatch must not be treated as a successful recharge")
	}
}

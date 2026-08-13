package provider

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	maxEfunResponse       = 256 << 10
	efunRateWindow        = time.Minute
	efunGlobalRateLimit   = 60
	efunPurchaseRateLimit = 10
	efunRefreshRateLimit  = 30
)

type efunRateLimiter struct {
	mu       sync.Mutex
	global   []time.Time
	purchase []time.Time
	refresh  []time.Time
}

var efunRateLimiters sync.Map

type EfunCardClient struct {
	http    *http.Client
	apiKey  string
	baseURL string
	limiter *efunRateLimiter
}

func NewEfunCardClient(client *http.Client, baseURL, apiKey string) (*EfunCardClient, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return nil, fail("EFUNCARD_CONFIGURATION_INVALID", "EfunCard base URL is invalid", false)
	}
	if strings.TrimSpace(apiKey) == "" {
		return nil, fail("EFUNCARD_OPENAPI_NOT_CONFIGURED", "EfunCard API key is not configured", false)
	}
	apiKey = strings.TrimSpace(apiKey)
	identity := sha256.Sum256([]byte(baseURL + "\x00" + apiKey))
	limiterValue, _ := efunRateLimiters.LoadOrStore(identity, &efunRateLimiter{})
	return &EfunCardClient{http: client, apiKey: apiKey, baseURL: baseURL, limiter: limiterValue.(*efunRateLimiter)}, nil
}

func (c *EfunCardClient) Key() string { return CardPlatformEfun }

func (c *EfunCardClient) Capabilities() CardPlatformCapabilities {
	return CardPlatformCapabilities{
		OpenCard: true, Recharge: true, RechargeIdempotent: false,
		FundingReplayWindow: 5 * time.Minute,
	}
}

func trimEfunRateWindow(values []time.Time, cutoff time.Time) []time.Time {
	index := 0
	for index < len(values) && !values[index].After(cutoff) {
		index++
	}
	return values[index:]
}

func (l *efunRateLimiter) allow(method, path string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := now.Add(-efunRateWindow)
	l.global = trimEfunRateWindow(l.global, cutoff)
	l.purchase = trimEfunRateWindow(l.purchase, cutoff)
	l.refresh = trimEfunRateWindow(l.refresh, cutoff)

	var endpoint *[]time.Time
	limit := 0
	if method == http.MethodPost && path == "/cards/purchase" {
		endpoint, limit = &l.purchase, efunPurchaseRateLimit
	} else if method == http.MethodPost && strings.HasSuffix(path, "/refresh-balance") {
		endpoint, limit = &l.refresh, efunRefreshRateLimit
	}
	if len(l.global) >= efunGlobalRateLimit || (endpoint != nil && len(*endpoint) >= limit) {
		return false
	}
	l.global = append(l.global, now)
	if endpoint != nil {
		*endpoint = append(*endpoint, now)
	}
	return true
}

func (c *EfunCardClient) request(ctx context.Context, method, path string, input any, idempotencyKey string, output any) error {
	if !c.limiter.allow(method, path, time.Now().UTC()) {
		return &Error{ErrorCode: "EFUNCARD_RATE_LIMITED", Message: "EfunCard local rate limit reached", Retryable: true, KnownNoWrite: true}
	}
	var body *bytes.Reader
	if input == nil {
		body = bytes.NewReader(nil)
	} else {
		encoded, err := json.Marshal(input)
		if err != nil {
			return fmt.Errorf("encode EfunCard request: %w", err)
		}
		body = bytes.NewReader(encoded)
	}
	request, err := newRequest(ctx, method, c.baseURL+path, body)
	if err != nil {
		return err
	}
	request.Header.Set("X-API-Key", c.apiKey)
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if idempotencyKey != "" {
		request.Header.Set("X-Idempotency-Key", idempotencyKey)
	}
	response, err := c.http.Do(request)
	if err != nil {
		var networkError net.Error
		if errors.As(err, &networkError) && networkError.Timeout() {
			return wrap("EFUNCARD_TIMEOUT", "EfunCard request timed out", err, true)
		}
		return wrap("EFUNCARD_UNAVAILABLE", "EfunCard network failure", err, true)
	}
	defer response.Body.Close()
	raw, readErr := readLimited(response, maxEfunResponse, "EFUNCARD_RESPONSE_TOO_LARGE")
	if readErr != nil {
		return readErr
	}
	switch response.StatusCode {
	case http.StatusUnauthorized:
		return &Error{ErrorCode: "EFUNCARD_AUTH_FAILED", Message: "EfunCard authentication failed", KnownNoWrite: true}
	case http.StatusForbidden:
		return &Error{ErrorCode: "EFUNCARD_ACCESS_DENIED", Message: "EfunCard access denied", KnownNoWrite: true}
	case http.StatusTooManyRequests:
		return &Error{ErrorCode: "EFUNCARD_RATE_LIMITED", Message: "EfunCard rate limited", Retryable: true, KnownNoWrite: true}
	case http.StatusConflict:
		return fail("EFUNCARD_OPERATION_PENDING", "EfunCard idempotent operation is still processing", true)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		knownNoWrite := response.StatusCode >= 400 && response.StatusCode < 500
		return &Error{ErrorCode: "EFUNCARD_OPERATION_REJECTED", Message: "EfunCard rejected operation", Retryable: response.StatusCode >= 500, KnownNoWrite: knownNoWrite}
	}
	var envelope struct {
		Success *bool           `json:"success"`
		Data    json.RawMessage `json:"data"`
	}
	if json.Unmarshal(raw, &envelope) != nil || envelope.Success == nil {
		return fail("EFUNCARD_RESPONSE_INVALID", "EfunCard response is invalid", true)
	}
	if !*envelope.Success {
		return &Error{ErrorCode: "EFUNCARD_OPERATION_REJECTED", Message: "EfunCard rejected operation", KnownNoWrite: true}
	}
	if output == nil {
		return nil
	}
	if len(envelope.Data) == 0 || string(envelope.Data) == "null" || json.Unmarshal(envelope.Data, output) != nil {
		return fail("EFUNCARD_CONTRACT_DRIFT", "EfunCard response contract drift", true)
	}
	return nil
}

type efunCatalog struct {
	Products        []Product
	IDs             map[string]int64
	ExchangeRate    float64
	PurchaseEnabled bool
}

func parseDecimal(value string, positive bool) (float64, bool) {
	parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) || parsed < 0 || (positive && parsed <= 0) {
		return 0, false
	}
	return parsed, true
}

func (c *EfunCardClient) catalog(ctx context.Context) (efunCatalog, error) {
	var raw struct {
		CardTypes []struct {
			ID             *int64 `json:"id"`
			CardType       string `json:"cardType"`
			BaseCardFeeRMB string `json:"baseCardFeeRmb"`
			FeeRate        string `json:"feeRate"`
			MinAmount      string `json:"minAmount"`
			MaxAmount      string `json:"maxAmount"`
		} `json:"cardTypes"`
		ExchangeRate    *float64 `json:"exchangeRate"`
		PurchaseEnabled *bool    `json:"purchaseEnabled"`
		Discount        *struct {
			Percent *float64 `json:"discountPercent"`
		} `json:"discount"`
	}
	if err := c.request(ctx, http.MethodGet, "/card-types", nil, "", &raw); err != nil {
		return efunCatalog{}, err
	}
	if raw.ExchangeRate == nil || *raw.ExchangeRate <= 0 || raw.PurchaseEnabled == nil {
		return efunCatalog{}, fail("EFUNCARD_CONTRACT_DRIFT", "EfunCard catalog is invalid", true)
	}
	discount := 1.0
	if raw.Discount != nil {
		if raw.Discount.Percent == nil || *raw.Discount.Percent <= 0 || *raw.Discount.Percent > 100 {
			return efunCatalog{}, fail("EFUNCARD_CONTRACT_DRIFT", "EfunCard discount is invalid", true)
		}
		discount = *raw.Discount.Percent / 100
	}
	seen := map[string]bool{}
	ids := map[string]int64{}
	products := make([]Product, 0, len(raw.CardTypes))
	for _, item := range raw.CardTypes {
		code := strings.TrimSpace(item.CardType)
		baseFee, baseOK := parseDecimal(item.BaseCardFeeRMB, false)
		feeRate, rateOK := parseDecimal(item.FeeRate, false)
		minimum, minOK := parseDecimal(item.MinAmount, false)
		maximum, maxOK := parseDecimal(item.MaxAmount, true)
		if item.ID == nil || *item.ID <= 0 || code == "" || seen[code] || !baseOK || !rateOK || !minOK || !maxOK || feeRate > 1 || maximum < minimum {
			return efunCatalog{}, fail("EFUNCARD_CONTRACT_DRIFT", "EfunCard product contract drift", true)
		}
		seen[code] = true
		ids[code] = *item.ID
		products = append(products, Product{
			ProductCode: code, OpenEnabled: *raw.PurchaseEnabled, OpenFee: baseFee * discount / *raw.ExchangeRate,
			OpenFeeRate: feeRate * discount, RechargeFee: feeRate * discount,
			MinAmount: minimum, MaxAmount: maximum,
		})
	}
	return efunCatalog{Products: products, IDs: ids, ExchangeRate: *raw.ExchangeRate, PurchaseEnabled: *raw.PurchaseEnabled}, nil
}

func (c *EfunCardClient) ListProducts(ctx context.Context) ([]Product, error) {
	catalog, err := c.catalog(ctx)
	return catalog.Products, err
}

func (c *EfunCardClient) GetBalance(ctx context.Context) (Balance, error) {
	var raw struct {
		Balance  string `json:"balance"`
		Currency string `json:"currency"`
	}
	if err := c.request(ctx, http.MethodGet, "/account/balance", nil, "", &raw); err != nil {
		return Balance{}, err
	}
	amount, ok := parseDecimal(raw.Balance, false)
	if !ok || strings.ToUpper(strings.TrimSpace(raw.Currency)) != "CNY" {
		return Balance{}, fail("EFUNCARD_CONTRACT_DRIFT", "EfunCard balance contract drift", true)
	}
	catalog, err := c.catalog(ctx)
	if err != nil {
		return Balance{}, err
	}
	return Balance{Balance: amount / catalog.ExchangeRate, Currency: "USD"}, nil
}

func (c *EfunCardClient) ListCards(ctx context.Context, page, pageSize int, _ bool) (int, []Card, error) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 20
	}
	if pageSize > 100 {
		pageSize = 100
	}
	params := url.Values{"page": {strconv.Itoa(page)}, "pageSize": {strconv.Itoa(pageSize)}}
	var raw struct {
		Cards []struct {
			ID          *int64 `json:"id"`
			CardNumber  string `json:"cardNumber"`
			CardType    string `json:"cardType"`
			Status      string `json:"status"`
			CardBalance string `json:"cardBalance"`
			CreatedAt   string `json:"createdAt"`
		} `json:"cards"`
		Total *int `json:"total"`
	}
	if err := c.request(ctx, http.MethodGet, "/cards?"+params.Encode(), nil, "", &raw); err != nil {
		return 0, nil, err
	}
	if raw.Total == nil || *raw.Total < 0 {
		return 0, nil, fail("EFUNCARD_CONTRACT_DRIFT", "EfunCard card total is invalid", true)
	}
	cards := make([]Card, 0, len(raw.Cards))
	for _, item := range raw.Cards {
		balance, ok := parseDecimal(item.CardBalance, false)
		digits := regexp.MustCompile(`\D`).ReplaceAllString(item.CardNumber, "")
		last4 := ""
		if len(digits) >= 4 {
			last4 = digits[len(digits)-4:]
		}
		status := strings.ToUpper(strings.TrimSpace(item.Status))
		if item.ID == nil || *item.ID <= 0 || strings.TrimSpace(item.CardType) == "" || status == "" || !ok || last4 == "" {
			return 0, nil, fail("EFUNCARD_CONTRACT_DRIFT", "EfunCard card list contract drift", true)
		}
		cards = append(cards, Card{UpstreamCardID: *item.ID, VMCardID: strconv.FormatInt(*item.ID, 10), ProductCode: strings.TrimSpace(item.CardType), AvailableAmount: balance, Status: status, Last4: last4, CreatedAt: strings.TrimSpace(item.CreatedAt)})
	}
	return *raw.Total, cards, nil
}

type efunCardDetail struct {
	ID          *int64 `json:"id"`
	CardNumber  string `json:"cardNumber"`
	CardType    string `json:"cardType"`
	CVV         string `json:"cvv"`
	ExpiryMonth string `json:"expiryMonth"`
	ExpiryYear  string `json:"expiryYear"`
	Status      string `json:"status"`
	CardBalance string `json:"cardBalance"`
}

func (c *EfunCardClient) cardDetail(ctx context.Context, cardID int64) (efunCardDetail, error) {
	var detail efunCardDetail
	if cardID <= 0 {
		return detail, fail("CHECKOUT_MATERIAL_INVALID", "card id is invalid", false)
	}
	if err := c.request(ctx, http.MethodGet, fmt.Sprintf("/cards/%d", cardID), nil, "", &detail); err != nil {
		return detail, err
	}
	if detail.ID == nil || *detail.ID != cardID || strings.TrimSpace(detail.Status) == "" {
		return detail, fail("EFUNCARD_CONTRACT_DRIFT", "EfunCard card detail contract drift", true)
	}
	return detail, nil
}

func (c *EfunCardClient) GetCardMaterial(ctx context.Context, cardID int64, now time.Time) (CardMaterial, error) {
	detail, err := c.cardDetail(ctx, cardID)
	if err != nil {
		return CardMaterial{}, err
	}
	number := regexp.MustCompile(`\D`).ReplaceAllString(detail.CardNumber, "")
	cvv := strings.TrimSpace(detail.CVV)
	month, year := strings.TrimSpace(detail.ExpiryMonth), strings.TrimSpace(detail.ExpiryYear)
	status := strings.ToUpper(strings.TrimSpace(detail.Status))
	if !regexp.MustCompile(`^\d{12,19}$`).MatchString(number) || !luhn(number) || !regexp.MustCompile(`^\d{3,4}$`).MatchString(cvv) ||
		!regexp.MustCompile(`^(0[1-9]|1[0-2])$`).MatchString(month) || !regexp.MustCompile(`^\d{4}$`).MatchString(year) || status != "ACTIVE" {
		return CardMaterial{}, fail("CHECKOUT_MATERIAL_CONTRACT_INVALID", "EfunCard material contract drift", true)
	}
	monthNumber, _ := strconv.Atoi(month)
	yearNumber, _ := strconv.Atoi(year)
	if !time.Date(yearNumber, time.Month(monthNumber)+1, 1, 0, 0, 0, 0, time.UTC).After(now.UTC()) {
		return CardMaterial{}, fail("CHECKOUT_CARD_EXPIRED", "EfunCard card is expired", false)
	}
	return CardMaterial{Number: number, CVV: cvv, ExpiryMonth: month, ExpiryYear: year, Status: status}, nil
}

func efunTransactionState(amount float64, status string) (string, string, bool) {
	status = strings.ToLower(strings.TrimSpace(status))
	switch status {
	case "completed", "complete", "success", "succeeded":
		if amount > 0 {
			return "Refund", "COMPLETE", true
		}
		return "Settlement", "COMPLETE", true
	case "pending", "processing":
		return "Authorization", "PENDING", true
	case "declined", "failed", "failure":
		return "Authorization", "DECLINED", true
	default:
		return "", "", false
	}
}

func (c *EfunCardClient) ListTransactions(ctx context.Context, cardID int64, page, pageSize int) ([]Transaction, error) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 20
	}
	if pageSize > 100 {
		pageSize = 100
	}
	now := time.Now().UTC()
	params := url.Values{
		"startDate": {now.AddDate(0, 0, -30).Format("2006-01-02")}, "endDate": {now.Format("2006-01-02")},
		"page": {strconv.Itoa(page)}, "pageSize": {strconv.Itoa(pageSize)},
	}
	var raw struct {
		Transactions []struct {
			RequestID string `json:"request_id"`
			Amount    string `json:"amount"`
			Currency  string `json:"currency"`
			Merchant  string `json:"merchant"`
			Time      string `json:"transaction_time"`
			Status    string `json:"status"`
		} `json:"transactions"`
	}
	if err := c.request(ctx, http.MethodGet, fmt.Sprintf("/cards/%d/transactions?%s", cardID, params.Encode()), nil, "", &raw); err != nil {
		return nil, err
	}
	items := make([]Transaction, 0, len(raw.Transactions))
	for _, item := range raw.Transactions {
		amount, ok := parseDecimal(strings.TrimPrefix(strings.TrimSpace(item.Amount), "-"), false)
		signed, signedErr := strconv.ParseFloat(strings.TrimSpace(item.Amount), 64)
		typeName, status, stateOK := efunTransactionState(signed, item.Status)
		currency := strings.ToUpper(strings.TrimSpace(item.Currency))
		if strings.TrimSpace(item.RequestID) == "" || !ok || signedErr != nil || currency == "" || !stateOK || strings.TrimSpace(item.Time) == "" {
			return nil, fail("EFUNCARD_CONTRACT_DRIFT", "EfunCard transaction contract drift", true)
		}
		merchant := "OTHER"
		lowerMerchant := strings.ToLower(item.Merchant)
		if strings.Contains(lowerMerchant, "openai") || strings.Contains(lowerMerchant, "chatgpt") {
			merchant = "OPENAI"
		}
		transaction := Transaction{AuthID: strings.TrimSpace(item.RequestID), AuthTime: strings.TrimSpace(item.Time), AuthCurrency: currency, Status: status, Type: typeName, MerchantNormalized: merchant, CreatedAt: strings.TrimSpace(item.Time)}
		if typeName == "Settlement" || typeName == "Refund" {
			transaction.SettleAmount, transaction.SettleCurrency = amount, currency
		} else {
			transaction.AuthAmount = amount
		}
		items = append(items, transaction)
	}
	return items, nil
}

func (c *EfunCardClient) GetOpenAIPayments(_ context.Context, _ int64, transactions []Transaction) ([]PriceSignal, error) {
	ranges := []struct {
		tier     string
		min, max float64
	}{{"plus", 15, 20}, {"x5", 90, 100}, {"x20", 120, 160}}
	result := make([]PriceSignal, 0, len(ranges))
	for _, priceRange := range ranges {
		latest := PriceSignal{Tier: priceRange.tier, Label: priceRange.tier, MinUSD: priceRange.min, MaxUSD: priceRange.max}
		for _, transaction := range transactions {
			if transaction.MerchantNormalized != "OPENAI" || transaction.Status != "COMPLETE" || transaction.Type != "Settlement" || transaction.SettleCurrency != "USD" || transaction.SettleAmount < priceRange.min || transaction.SettleAmount > priceRange.max {
				continue
			}
			if !latest.Found || transaction.AuthTime > latest.Time {
				latest.Found, latest.Amount, latest.Time = true, transaction.SettleAmount, transaction.AuthTime
			}
		}
		result = append(result, latest)
	}
	return result, nil
}

func (c *EfunCardClient) OpenCard(ctx context.Context, input OpenCardInput, key string) (OpenCardResult, error) {
	normalizedKey := efunIdempotencyKey(key)
	if normalizedKey == "" || input.InitAmount <= 0 {
		return OpenCardResult{}, fail("FUNDING_IDEMPOTENCY_KEY_INVALID", "invalid EfunCard idempotency key", false)
	}
	catalog, err := c.catalog(ctx)
	if err != nil {
		return OpenCardResult{}, err
	}
	var selected *Product
	for index := range catalog.Products {
		if catalog.Products[index].ProductCode == input.ProductCode {
			selected = &catalog.Products[index]
			break
		}
	}
	if selected == nil {
		return OpenCardResult{}, &Error{ErrorCode: "EFUNCARD_OPERATION_REJECTED", Message: "EfunCard card type is unavailable", KnownNoWrite: true}
	}
	if !catalog.PurchaseEnabled || !selected.OpenEnabled {
		return OpenCardResult{}, &Error{ErrorCode: "EFUNCARD_PURCHASE_DISABLED", Message: "EfunCard purchase is disabled", KnownNoWrite: true}
	}
	cardTypeID := catalog.IDs[input.ProductCode]
	if cardTypeID <= 0 {
		return OpenCardResult{}, fail("EFUNCARD_CONTRACT_DRIFT", "EfunCard card type identity is missing", true)
	}
	var raw struct {
		Cards []struct {
			ID     *int64 `json:"id"`
			Status string `json:"status"`
		} `json:"cards"`
		TotalCost string `json:"totalCost"`
	}
	request := map[string]any{"cardTypeId": cardTypeID, "quantity": 1, "openCardAmount": input.InitAmount, "remark": "kwmembership:" + normalizedKey}
	if err := c.request(ctx, http.MethodPost, "/cards/purchase", request, normalizedKey, &raw); err != nil {
		return OpenCardResult{}, err
	}
	if len(raw.Cards) != 1 || raw.Cards[0].ID == nil || *raw.Cards[0].ID <= 0 {
		return OpenCardResult{}, fail("EFUNCARD_FUNDING_OUTCOME_UNKNOWN", "EfunCard accepted purchase but returned an invalid result", false)
	}
	totalCostCNY, ok := parseDecimal(raw.TotalCost, true)
	if !ok {
		return OpenCardResult{}, fail("EFUNCARD_FUNDING_OUTCOME_UNKNOWN", "EfunCard accepted purchase but returned an invalid cost", false)
	}
	providerFeeUSD := totalCostCNY/catalog.ExchangeRate - input.InitAmount
	expectedFeeUSD := selected.OpenFee + input.InitAmount*selected.OpenFeeRate
	if providerFeeUSD < 0 || math.Abs(providerFeeUSD-expectedFeeUSD) > 0.02 {
		return OpenCardResult{}, fail("EFUNCARD_FUNDING_OUTCOME_UNKNOWN", "EfunCard accepted purchase with an unexpected cost", false)
	}
	cardID := *raw.Cards[0].ID
	deadline := time.Now().Add(5 * time.Minute)
	for {
		detail, detailErr := c.cardDetail(ctx, cardID)
		if detailErr != nil {
			return OpenCardResult{}, wrap("EFUNCARD_FUNDING_OUTCOME_UNKNOWN", "EfunCard card activation could not be reconciled", detailErr, false)
		}
		status := strings.ToUpper(strings.TrimSpace(detail.Status))
		if status == "ACTIVE" {
			balance, valid := parseDecimal(detail.CardBalance, false)
			if !valid || strings.TrimSpace(detail.CardType) != input.ProductCode {
				return OpenCardResult{}, fail("EFUNCARD_FUNDING_OUTCOME_UNKNOWN", "EfunCard activated card does not match the request", false)
			}
			return OpenCardResult{UpstreamCardID: cardID, VMCardID: strconv.FormatInt(cardID, 10), ProductCode: input.ProductCode, AvailableAmount: balance, Status: status, OpenFee: expectedFeeUSD}, nil
		}
		if status != "PENDING" && status != "PROCESSING" {
			return OpenCardResult{}, fail("EFUNCARD_FUNDING_OUTCOME_UNKNOWN", "EfunCard accepted purchase but card activation failed", false)
		}
		if time.Now().After(deadline) {
			return OpenCardResult{}, fail("EFUNCARD_FUNDING_OUTCOME_UNKNOWN", "EfunCard card activation is still pending", false)
		}
		select {
		case <-ctx.Done():
			return OpenCardResult{}, wrap("EFUNCARD_FUNDING_OUTCOME_UNKNOWN", "EfunCard card activation interrupted", ctx.Err(), false)
		case <-time.After(2 * time.Second):
		}
	}
}

func efunIdempotencyKey(key string) string {
	key = strings.TrimSpace(key)
	if regexp.MustCompile(`^[A-Za-z0-9_\-:.]{16,128}$`).MatchString(key) {
		return key
	}
	if key == "" || !regexp.MustCompile(`^[A-Za-z0-9_\-:.]+$`).MatchString(key) {
		return ""
	}
	digest := sha256.Sum256([]byte(key))
	return "kwr:" + fmt.Sprintf("%x", digest[:])
}

func (c *EfunCardClient) RechargeCard(ctx context.Context, cardID int64, amount float64, _ string) error {
	detail, err := c.cardDetail(ctx, cardID)
	if err != nil {
		return err
	}
	before, ok := parseDecimal(detail.CardBalance, false)
	if !ok || strings.ToUpper(strings.TrimSpace(detail.Status)) != "ACTIVE" || amount <= 0 {
		return &Error{ErrorCode: "EFUNCARD_OPERATION_REJECTED", Message: "EfunCard recharge request is invalid", KnownNoWrite: true}
	}
	catalog, err := c.catalog(ctx)
	if err != nil {
		return err
	}
	var product *Product
	for index := range catalog.Products {
		if catalog.Products[index].ProductCode == strings.TrimSpace(detail.CardType) {
			product = &catalog.Products[index]
			break
		}
	}
	if product == nil {
		return &Error{ErrorCode: "EFUNCARD_OPERATION_REJECTED", Message: "EfunCard card type is unavailable", KnownNoWrite: true}
	}
	var receipt struct {
		TaskID            string   `json:"taskId"`
		RechargeAmountUSD *float64 `json:"rechargeAmountUsd"`
		ServiceFeeUSD     *float64 `json:"serviceFeeUsd"`
		TotalCostCNY      *float64 `json:"totalCostCny"`
	}
	if err := c.request(ctx, http.MethodPost, fmt.Sprintf("/cards/%d/recharge", cardID), map[string]any{"amount": amount}, "", &receipt); err != nil {
		return err
	}
	expectedFeeUSD := amount * product.RechargeFee
	expectedCostCNY := (amount + expectedFeeUSD) * catalog.ExchangeRate
	if strings.TrimSpace(receipt.TaskID) == "" || receipt.RechargeAmountUSD == nil || receipt.ServiceFeeUSD == nil || receipt.TotalCostCNY == nil ||
		math.IsNaN(*receipt.RechargeAmountUSD) || math.IsInf(*receipt.RechargeAmountUSD, 0) ||
		math.IsNaN(*receipt.ServiceFeeUSD) || math.IsInf(*receipt.ServiceFeeUSD, 0) ||
		math.IsNaN(*receipt.TotalCostCNY) || math.IsInf(*receipt.TotalCostCNY, 0) ||
		math.Abs(*receipt.RechargeAmountUSD-amount) > 0.005 ||
		math.Abs(*receipt.ServiceFeeUSD-expectedFeeUSD) > 0.02 ||
		math.Abs(*receipt.TotalCostCNY-expectedCostCNY) > 0.05 {
		return fail("EFUNCARD_FUNDING_OUTCOME_UNKNOWN", "EfunCard accepted recharge with an invalid receipt", false)
	}
	deadline := time.Now().Add(5 * time.Minute)
	for {
		var refreshed struct {
			CardBalance string `json:"cardBalance"`
		}
		if err := c.request(ctx, http.MethodPost, fmt.Sprintf("/cards/%d/refresh-balance", cardID), nil, "", &refreshed); err != nil {
			return wrap("EFUNCARD_FUNDING_OUTCOME_UNKNOWN", "EfunCard recharge could not be reconciled", err, false)
		}
		balance, valid := parseDecimal(refreshed.CardBalance, false)
		if valid && balance+0.001 >= before+amount {
			return nil
		}
		if time.Now().After(deadline) {
			return fail("EFUNCARD_FUNDING_OUTCOME_UNKNOWN", "EfunCard recharge outcome is unknown", false)
		}
		select {
		case <-ctx.Done():
			return wrap("EFUNCARD_FUNDING_OUTCOME_UNKNOWN", "EfunCard recharge interrupted", ctx.Err(), false)
		case <-time.After(10 * time.Second):
		}
	}
}

func (c *EfunCardClient) FreezeCard(context.Context, int64) error {
	return UnsupportedCapability(CardPlatformEfun, "freeze")
}

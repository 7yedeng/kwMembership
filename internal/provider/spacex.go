package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const SpaceXCardBaseURL = "https://spacexcard.com/openapi/v1"
const maxSpaceXResponse = 256 << 10

type SpaceXClient struct {
	http      *http.Client
	appID     string
	appSecret string
	baseURL   string
}

type Product struct {
	ProductCode string
	OpenEnabled bool
	OpenFee     float64
	OpenFeeRate float64
	RechargeFee float64
	MinAmount   float64
	MaxAmount   float64
}

type Card struct {
	UpstreamCardID  int64
	VMCardID        string
	ProductCode     string
	Network         string
	IssuingArea     string
	AvailableAmount float64
	Status          string
	BIN             string
	Last4           string
	CreatedAt       string
}

type Transaction struct {
	AuthID             string
	AuthTime           string
	AuthAmount         float64
	AuthCurrency       string
	SettleAmount       float64
	SettleCurrency     string
	Status             string
	Type               string
	MerchantNormalized string
	CreatedAt          string
}

type PriceSignal struct {
	Tier   string
	Label  string
	MinUSD float64
	MaxUSD float64
	Amount float64
	Time   string
	Found  bool
}

type Balance struct {
	Balance  float64
	Currency string
}

type CardMaterial struct {
	Number      string
	CVV         string
	ExpiryMonth string
	ExpiryYear  string
	Status      string
}

type OpenCardInput struct {
	ProductCode string  `json:"product_code"`
	FirstName   string  `json:"first_name"`
	LastName    string  `json:"last_name"`
	InitAmount  float64 `json:"init_amount"`
}

type OpenCardResult struct {
	UpstreamCardID  int64
	VMCardID        string
	ProductCode     string
	AvailableAmount float64
	Status          string
	OpenFee         float64
}

func NewSpaceXClient(client *http.Client, appID, appSecret string) (*SpaceXClient, error) {
	if strings.TrimSpace(appSecret) == "" {
		return nil, fail("SPACEXCARD_OPENAPI_NOT_CONFIGURED", "SpaceX Card OpenAPI app_secret is not configured", false)
	}
	return &SpaceXClient{http: client, appID: strings.TrimSpace(appID), appSecret: strings.TrimSpace(appSecret), baseURL: SpaceXCardBaseURL}, nil
}

func (c *SpaceXClient) Key() string { return CardPlatformSpaceX }

func (c *SpaceXClient) Capabilities() CardPlatformCapabilities {
	return CardPlatformCapabilities{Freeze: true, OpenCard: true, Recharge: true, RechargeIdempotent: true}
}

func (c *SpaceXClient) request(ctx context.Context, method, path string, input any, idempotencyKey string, output any) error {
	var body *bytes.Reader
	if input == nil {
		body = bytes.NewReader(nil)
	} else {
		encoded, err := json.Marshal(input)
		if err != nil {
			return fmt.Errorf("encode SpaceX request: %w", err)
		}
		body = bytes.NewReader(encoded)
	}
	request, err := newRequest(ctx, method, c.baseURL+path, body)
	if err != nil {
		return err
	}
	request.Header.Set("X-API-Key", c.appSecret)
	if c.appID != "" {
		request.Header.Set("X-App-Id", c.appID)
	}
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if idempotencyKey != "" {
		request.Header.Set("Idempotency-Key", idempotencyKey)
	}
	response, err := c.http.Do(request)
	if err != nil {
		var networkError net.Error
		if errors.As(err, &networkError) && networkError.Timeout() {
			return wrap("SPACEXCARD_TIMEOUT", "SpaceX Card request timed out", err, true)
		}
		return wrap("SPACEXCARD_UNAVAILABLE", "SpaceX Card network failure", err, true)
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusUnauthorized:
		return fail("SPACEXCARD_AUTH_FAILED", "SpaceX Card authentication failed", false)
	case http.StatusForbidden:
		return fail("SPACEXCARD_ACCESS_DENIED", "SpaceX Card access denied", false)
	case http.StatusTooManyRequests:
		return fail("SPACEXCARD_RATE_LIMITED", "SpaceX Card rate limited", true)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fail("SPACEXCARD_UNAVAILABLE", "SpaceX Card unavailable", true)
	}
	raw, err := readLimited(response, maxSpaceXResponse, "SPACEXCARD_RESPONSE_TOO_LARGE")
	if err != nil {
		return err
	}
	var envelope struct {
		Code *json.Number    `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&envelope); err != nil {
		return fail("SPACEXCARD_RESPONSE_INVALID", "SpaceX Card response is invalid JSON", true)
	}
	if envelope.Code == nil {
		return fail("SPACEXCARD_CONTRACT_DRIFT", "SpaceX Card response code is invalid", true)
	}
	code, err := strconv.ParseInt(string(*envelope.Code), 10, 64)
	if err != nil {
		return fail("SPACEXCARD_CONTRACT_DRIFT", "SpaceX Card response code is invalid", true)
	}
	if code != 0 {
		return &Error{ErrorCode: "SPACEXCARD_OPERATION_REJECTED", Message: "SpaceX Card rejected operation", Retryable: false, KnownNoWrite: true}
	}
	if output == nil {
		return nil
	}
	if len(envelope.Data) == 0 || string(envelope.Data) == "null" {
		return fail("SPACEXCARD_CONTRACT_DRIFT", "SpaceX Card response data is missing", true)
	}
	if err := json.Unmarshal(envelope.Data, output); err != nil {
		return fail("SPACEXCARD_CONTRACT_DRIFT", "SpaceX Card response contract drift", true)
	}
	return nil
}

func (c *SpaceXClient) ListProducts(ctx context.Context) ([]Product, error) {
	var raw []struct {
		ProductCode string   `json:"product_code"`
		OpenFee     *float64 `json:"open_fee"`
		RechargeFee *float64 `json:"recharge_fee"`
		MinAmount   *float64 `json:"min_amount"`
		MaxAmount   *float64 `json:"max_amount"`
	}
	if err := c.request(ctx, http.MethodGet, "/products", nil, "", &raw); err != nil {
		return nil, err
	}
	products := make([]Product, 0, len(raw))
	seen := map[string]bool{}
	for _, item := range raw {
		code := strings.TrimSpace(item.ProductCode)
		if code == "" || seen[code] || item.OpenFee == nil || item.RechargeFee == nil || item.MinAmount == nil || item.MaxAmount == nil || *item.OpenFee < 0 || *item.RechargeFee < 0 || *item.MinAmount < 0 || *item.MaxAmount < *item.MinAmount {
			return nil, fail("SPACEXCARD_CONTRACT_DRIFT", "SpaceX Card product contract drift", true)
		}
		seen[code] = true
		products = append(products, Product{ProductCode: code, OpenEnabled: true, OpenFee: *item.OpenFee, RechargeFee: *item.RechargeFee, MinAmount: *item.MinAmount, MaxAmount: *item.MaxAmount})
	}
	return products, nil
}

func (c *SpaceXClient) ListCards(ctx context.Context, page, pageSize int, sync bool) (int, []Card, error) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 20
	}
	if pageSize > 100 {
		pageSize = 100
	}
	params := url.Values{"page": {strconv.Itoa(page)}, "page_size": {strconv.Itoa(pageSize)}}
	if sync {
		params.Set("sync", "1")
	}
	var raw struct {
		Total *int `json:"total"`
		List  *[]struct {
			ID              *int64   `json:"id"`
			VMCardID        string   `json:"vm_card_id"`
			ProductCode     string   `json:"product_code"`
			Network         string   `json:"network"`
			IssuingArea     string   `json:"issuing_area"`
			AvailableAmount *float64 `json:"available_amount"`
			Status          string   `json:"status"`
			CardNumber      string   `json:"card_number"`
			CreatedAt       string   `json:"created_at"`
		} `json:"list"`
	}
	if err := c.request(ctx, http.MethodGet, "/cards?"+params.Encode(), nil, "", &raw); err != nil {
		return 0, nil, err
	}
	if raw.Total == nil || *raw.Total < 0 || raw.List == nil {
		return 0, nil, fail("SPACEXCARD_CONTRACT_DRIFT", "SpaceX Card total is invalid", true)
	}
	cards := make([]Card, 0, len(*raw.List))
	for _, item := range *raw.List {
		digits := regexp.MustCompile(`\D`).ReplaceAllString(item.CardNumber, "")
		bin, last4 := "", ""
		if len(digits) >= 12 && len(digits) <= 19 {
			bin, last4 = digits[:6], digits[len(digits)-4:]
		}
		if item.ID == nil || item.AvailableAmount == nil || *item.ID <= 0 || strings.TrimSpace(item.VMCardID) == "" || strings.TrimSpace(item.ProductCode) == "" || *item.AvailableAmount < 0 || strings.TrimSpace(item.Status) == "" {
			return 0, nil, fail("SPACEXCARD_CONTRACT_DRIFT", "SpaceX Card list contract drift", true)
		}
		cards = append(cards, Card{UpstreamCardID: *item.ID, VMCardID: strings.TrimSpace(item.VMCardID), ProductCode: strings.TrimSpace(item.ProductCode), Network: strings.TrimSpace(item.Network), IssuingArea: strings.TrimSpace(item.IssuingArea), AvailableAmount: *item.AvailableAmount, Status: strings.ToUpper(strings.TrimSpace(item.Status)), BIN: bin, Last4: last4, CreatedAt: strings.TrimSpace(item.CreatedAt)})
	}
	return *raw.Total, cards, nil
}

func (c *SpaceXClient) ListTransactions(ctx context.Context, cardID int64, page, pageSize int) ([]Transaction, error) {
	params := url.Values{"page": {strconv.Itoa(page)}, "page_size": {strconv.Itoa(pageSize)}}
	var raw []struct {
		AuthID         string   `json:"auth_id"`
		AuthTime       string   `json:"auth_time"`
		AuthAmount     *float64 `json:"auth_amount"`
		AuthCurrency   string   `json:"auth_currency"`
		SettleAmount   *float64 `json:"settle_amount"`
		SettleCurrency string   `json:"settle_currency"`
		Status         string   `json:"status"`
		Type           string   `json:"type"`
		MerchantName   string   `json:"merchant_name"`
		CreatedAt      string   `json:"create_time"`
	}
	path := fmt.Sprintf("/cards/%d/transactions?%s", cardID, params.Encode())
	if err := c.request(ctx, http.MethodGet, path, nil, "", &raw); err != nil {
		return nil, err
	}
	items := make([]Transaction, 0, len(raw))
	for _, item := range raw {
		if strings.TrimSpace(item.AuthID) == "" || item.AuthAmount == nil || item.SettleAmount == nil || *item.AuthAmount < 0 || *item.SettleAmount < 0 || strings.TrimSpace(item.Status) == "" || strings.TrimSpace(item.Type) == "" {
			return nil, fail("SPACEXCARD_CONTRACT_DRIFT", "SpaceX Card transaction contract drift", true)
		}
		merchant := "OTHER"
		if strings.Contains(strings.ToLower(item.MerchantName), "openai") {
			merchant = "OPENAI"
		}
		items = append(items, Transaction{AuthID: strings.TrimSpace(item.AuthID), AuthTime: strings.TrimSpace(item.AuthTime), AuthAmount: *item.AuthAmount, AuthCurrency: strings.ToUpper(strings.TrimSpace(item.AuthCurrency)), SettleAmount: *item.SettleAmount, SettleCurrency: strings.ToUpper(strings.TrimSpace(item.SettleCurrency)), Status: strings.ToUpper(strings.TrimSpace(item.Status)), Type: strings.TrimSpace(item.Type), MerchantNormalized: merchant, CreatedAt: strings.TrimSpace(item.CreatedAt)})
	}
	return items, nil
}

func (c *SpaceXClient) GetCardMaterial(ctx context.Context, cardID int64, now time.Time) (CardMaterial, error) {
	if cardID <= 0 {
		return CardMaterial{}, fail("CHECKOUT_MATERIAL_INVALID", "card id is invalid", false)
	}
	var raw struct {
		CardNumber string `json:"card_number"`
		CVV        string `json:"cvv"`
		Expire     string `json:"expire"`
		Status     string `json:"status"`
	}
	if err := c.request(ctx, http.MethodGet, fmt.Sprintf("/cards/%d", cardID), nil, "", &raw); err != nil {
		return CardMaterial{}, err
	}
	number := regexp.MustCompile(`\D`).ReplaceAllString(raw.CardNumber, "")
	cvv := strings.TrimSpace(raw.CVV)
	expire := strings.Split(strings.TrimSpace(raw.Expire), "/")
	status := strings.ToUpper(strings.TrimSpace(raw.Status))
	if len(expire) != 2 || !regexp.MustCompile(`^\d{12,19}$`).MatchString(number) || !luhn(number) ||
		!regexp.MustCompile(`^\d{3,4}$`).MatchString(cvv) || !regexp.MustCompile(`^(0[1-9]|1[0-2])$`).MatchString(expire[0]) ||
		!regexp.MustCompile(`^\d{2}$`).MatchString(expire[1]) || status != "ACTIVE" {
		return CardMaterial{}, fail("CHECKOUT_MATERIAL_CONTRACT_INVALID", "SpaceX Card material contract drift", true)
	}
	year := "20" + expire[1]
	month, _ := strconv.Atoi(expire[0])
	yearNumber, _ := strconv.Atoi(year)
	if !time.Date(yearNumber, time.Month(month)+1, 1, 0, 0, 0, 0, time.UTC).After(now.UTC()) {
		return CardMaterial{}, fail("CHECKOUT_CARD_EXPIRED", "SpaceX Card is expired", false)
	}
	return CardMaterial{Number: number, CVV: cvv, ExpiryMonth: expire[0], ExpiryYear: year, Status: status}, nil
}

func luhn(value string) bool {
	sum, doubled := 0, false
	for index := len(value) - 1; index >= 0; index-- {
		digit := int(value[index] - '0')
		if doubled {
			digit *= 2
			if digit > 9 {
				digit -= 9
			}
		}
		sum += digit
		doubled = !doubled
	}
	return sum%10 == 0
}

func (c *SpaceXClient) GetOpenAIPayments(ctx context.Context, cardID int64, _ []Transaction) ([]PriceSignal, error) {
	var raw []struct {
		Tier   string   `json:"tier"`
		Label  string   `json:"label"`
		MinUSD *float64 `json:"min_usd"`
		MaxUSD *float64 `json:"max_usd"`
		Amount *float64 `json:"amount"`
		Time   string   `json:"time"`
		Found  *bool    `json:"found"`
	}
	if err := c.request(ctx, http.MethodGet, fmt.Sprintf("/cards/%d/openai-payments", cardID), nil, "", &raw); err != nil {
		return nil, err
	}
	if len(raw) != 3 {
		return nil, fail("SPACEXCARD_CONTRACT_DRIFT", "SpaceX Card price contract drift", true)
	}
	seen := map[string]bool{}
	items := make([]PriceSignal, 0, 3)
	for _, item := range raw {
		if !map[string]bool{"plus": true, "x5": true, "x20": true}[item.Tier] || seen[item.Tier] || item.MinUSD == nil || item.MaxUSD == nil || item.Amount == nil || item.Found == nil || *item.MinUSD < 0 || *item.MaxUSD < *item.MinUSD || (*item.Found && (*item.Amount <= 0 || strings.TrimSpace(item.Time) == "")) {
			return nil, fail("SPACEXCARD_CONTRACT_DRIFT", "SpaceX Card price contract drift", true)
		}
		seen[item.Tier] = true
		items = append(items, PriceSignal{Tier: item.Tier, Label: item.Label, MinUSD: *item.MinUSD, MaxUSD: *item.MaxUSD, Amount: *item.Amount, Time: strings.TrimSpace(item.Time), Found: *item.Found})
	}
	return items, nil
}

func (c *SpaceXClient) GetBalance(ctx context.Context) (Balance, error) {
	var raw struct {
		Balance  *float64 `json:"balance"`
		Currency string   `json:"currency"`
	}
	if err := c.request(ctx, http.MethodGet, "/balance", nil, "", &raw); err != nil {
		return Balance{}, err
	}
	raw.Currency = strings.ToUpper(strings.TrimSpace(raw.Currency))
	if raw.Balance == nil || *raw.Balance < 0 || raw.Currency != "USD" {
		return Balance{}, fail("SPACEXCARD_CONTRACT_DRIFT", "SpaceX Card balance contract drift", true)
	}
	return Balance{Balance: *raw.Balance, Currency: raw.Currency}, nil
}

func (c *SpaceXClient) OpenCard(ctx context.Context, input OpenCardInput, key string) (OpenCardResult, error) {
	if key == "" || len(key) > 200 {
		return OpenCardResult{}, fail("FUNDING_IDEMPOTENCY_KEY_INVALID", "invalid idempotency key", false)
	}
	var raw struct {
		ID              *int64   `json:"id"`
		VMCardID        string   `json:"vm_card_id"`
		ProductCode     string   `json:"product_code"`
		AvailableAmount *float64 `json:"available_amount"`
		Status          string   `json:"status"`
		OpenFee         *float64 `json:"open_fee"`
	}
	if err := c.request(ctx, http.MethodPost, "/cards/open", input, key, &raw); err != nil {
		return OpenCardResult{}, err
	}
	if raw.ID == nil || raw.AvailableAmount == nil || raw.OpenFee == nil || *raw.ID <= 0 || strings.TrimSpace(raw.VMCardID) == "" || strings.TrimSpace(raw.ProductCode) == "" || *raw.AvailableAmount < 0 || strings.TrimSpace(raw.Status) == "" || *raw.OpenFee < 0 {
		return OpenCardResult{}, fail("SPACEXCARD_CONTRACT_DRIFT", "SpaceX Card open result drift", true)
	}
	return OpenCardResult{UpstreamCardID: *raw.ID, VMCardID: strings.TrimSpace(raw.VMCardID), ProductCode: strings.TrimSpace(raw.ProductCode), AvailableAmount: *raw.AvailableAmount, Status: strings.ToUpper(strings.TrimSpace(raw.Status)), OpenFee: *raw.OpenFee}, nil
}

func (c *SpaceXClient) RechargeCard(ctx context.Context, cardID int64, amount float64, key string) error {
	if key == "" || len(key) > 200 || cardID <= 0 || amount <= 0 {
		return fail("FUNDING_REQUEST_INVALID", "invalid recharge request", false)
	}
	return c.request(ctx, http.MethodPost, "/cards/recharge", map[string]any{"card_id": cardID, "amount": amount}, key, nil)
}

func (c *SpaceXClient) FreezeCard(ctx context.Context, cardID int64) error {
	if cardID <= 0 {
		return fail("CARD_FREEZE_REQUEST_INVALID", "invalid card freeze request", false)
	}
	return c.request(ctx, http.MethodPost, "/cards/freeze", map[string]any{"card_id": cardID, "freeze": true}, "", nil)
}

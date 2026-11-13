package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode"
)

const CardPlatformZovo = "zovocard"
const ZovoCardBaseURL = "https://zovocard.com/openapi/v1"
const ZovoCardSandboxBaseURL = "https://sandbox.zovocard.com/openapi/v1"

// CardSnapshotReader is an optional, read-only adapter capability. The selected
// card detail supplies material and balance; catalog reads may bind its identity.
type CardSnapshotReader interface {
	GetCardSnapshot(context.Context, int64, time.Time) (CardSnapshot, error)
}

// FundingReplayPolicy can restrict an explicitly requested recovery beyond the
// legacy generic capabilities. A false result forbids another funding POST.
type FundingReplayPolicy interface {
	AllowsFundingReplay(operation string) bool
}

type CardSnapshot struct {
	UpstreamCardID            int64
	CardBindingVerified       bool
	Material                  CardMaterial `json:"-"`
	AvailableAmount           float64
	Currency                  string
	RestrictedMerchants       []string
	MerchantRestrictionsKnown bool
	GoogleChatGPTBlocked      *bool
}

func (CardSnapshot) String() string   { return "CardSnapshot{redacted}" }
func (CardSnapshot) GoString() string { return "CardSnapshot{redacted}" }

// ZovoClient exposes only basic card-platform operations. The shared SpaceX
// decoder is reused for explicitly verified wire shapes, not as a brand alias.
// Membership/direct-charge endpoints are intentionally unavailable.
type ZovoClient struct{ cards *SpaceXClient }

func (*ZovoClient) String() string   { return "ZovoClient{redacted}" }
func (*ZovoClient) GoString() string { return "ZovoClient{redacted}" }

func NewZovoClient(client *http.Client, appID, appSecret string) (*ZovoClient, error) {
	return NewZovoClientWithBaseURL(client, "", appID, appSecret)
}

func NewZovoClientWithBaseURL(client *http.Client, baseURL, appID, appSecret string) (*ZovoClient, error) {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = ZovoCardBaseURL
	}
	cards, err := NewSpaceXClientWithBaseURL(client, baseURL, appID, appSecret)
	if err != nil {
		return nil, zovoError(err)
	}
	return &ZovoClient{cards: cards}, nil
}

func (*ZovoClient) Key() string { return CardPlatformZovo }

func (*ZovoClient) Capabilities() CardPlatformCapabilities {
	// The provider documents 2xx-only idempotency caching. A failed or timed-out
	// write is not proven replay-safe, even with the original idempotency key.
	return CardPlatformCapabilities{Freeze: true, OpenCard: true, Recharge: true}
}

func (*ZovoClient) AllowsFundingReplay(string) bool { return false }

func (c *ZovoClient) ListProducts(ctx context.Context) ([]Product, error) {
	products, err := c.cards.ListProducts(ctx)
	return products, zovoError(err)
}

func (c *ZovoClient) ListCards(ctx context.Context, page, pageSize int, sync bool) (int, []Card, error) {
	total, cards, err := c.cards.ListCards(ctx, page, pageSize, sync)
	return total, cards, zovoError(err)
}

func (c *ZovoClient) ListTransactions(ctx context.Context, cardID int64, page, pageSize int) ([]Transaction, error) {
	if cardID <= 0 || page < 1 || pageSize < 1 || pageSize > 100 {
		return nil, zovoFailure("ZOVOCARD_REQUEST_INVALID", false, true)
	}
	transactions, err := c.cards.ListTransactions(ctx, cardID, page, pageSize)
	return transactions, zovoError(err)
}

func (c *ZovoClient) GetBalance(ctx context.Context) (Balance, error) {
	balance, err := c.cards.GetBalance(ctx)
	return balance, zovoError(err)
}

func (c *ZovoClient) GetCardMaterial(ctx context.Context, cardID int64, now time.Time) (CardMaterial, error) {
	snapshot, err := c.GetCardSnapshot(ctx, cardID, now)
	return snapshot.Material, err
}

func (c *ZovoClient) GetCardSnapshot(ctx context.Context, cardID int64, now time.Time) (CardSnapshot, error) {
	if cardID <= 0 {
		return CardSnapshot{}, zovoFailure("ZOVOCARD_REQUEST_INVALID", false, true)
	}
	bound, err := c.lookupCardBinding(ctx, cardID)
	if err != nil {
		return CardSnapshot{}, err
	}
	var raw struct {
		spaceXCardDetails
		CardID               string          `json:"card_id"`
		LocalID              *int64          `json:"id"`
		AvailableAmount      *float64        `json:"available_amount"`
		Currency             string          `json:"currency"`
		RestrictedMerchants  json.RawMessage `json:"restricted_merchants"`
		GoogleChatGPTBlocked *bool           `json:"google_chatgpt_blocked"`
	}
	if err := c.cards.request(ctx, http.MethodGet, fmt.Sprintf("/cards/%d", cardID), nil, "", &raw); err != nil {
		return CardSnapshot{}, zovoError(err)
	}
	number, numberOK := zovoCardNumber(raw.CardNumber)
	if !numberOK || raw.CardID != bound.vmCardID || number != bound.number || raw.LocalID != nil && *raw.LocalID != cardID {
		return CardSnapshot{}, zovoFailure("ZOVOCARD_CARD_BINDING_MISMATCH", false, false)
	}
	material, err := spaceXMaterialFromDetails(raw.spaceXCardDetails, now)
	if err != nil {
		return CardSnapshot{}, zovoError(err)
	}
	if raw.AvailableAmount == nil || !finiteNonnegative(*raw.AvailableAmount) {
		return CardSnapshot{}, zovoFailure("ZOVOCARD_CONTRACT_DRIFT", false, false)
	}
	currency := strings.ToUpper(strings.TrimSpace(raw.Currency))
	if currency != "" && !regexp.MustCompile(`^[A-Z]{3}$`).MatchString(currency) {
		return CardSnapshot{}, zovoFailure("ZOVOCARD_CONTRACT_DRIFT", false, false)
	}
	var restrictions []string
	known := len(raw.RestrictedMerchants) > 0 && string(raw.RestrictedMerchants) != "null"
	if known {
		if json.Unmarshal(raw.RestrictedMerchants, &restrictions) != nil || restrictions == nil || len(restrictions) > 64 {
			return CardSnapshot{}, zovoFailure("ZOVOCARD_CONTRACT_DRIFT", false, false)
		}
		for _, value := range restrictions {
			if value == "" || len(value) > 128 || strings.TrimSpace(value) != value || strings.IndexFunc(value, unicode.IsControl) >= 0 {
				return CardSnapshot{}, zovoFailure("ZOVOCARD_CONTRACT_DRIFT", false, false)
			}
		}
	}
	// A missing currency or rule list stays unknown. In particular a checkout's
	// PHP/EGP number cannot be compared directly with an unspecified card balance.
	return CardSnapshot{UpstreamCardID: cardID, CardBindingVerified: true, Material: material,
		AvailableAmount: *raw.AvailableAmount, Currency: currency,
		RestrictedMerchants: restrictions, MerchantRestrictionsKnown: known,
		GoogleChatGPTBlocked: raw.GoogleChatGPTBlocked}, nil
}

const zovoCardLookupPageSize = 100
const zovoCardLookupMaxPages = 100

type zovoCardBinding struct{ vmCardID, number string }

// /cards/{id} takes a local numeric ID, but its card_id response is the issuer's
// opaque ID. Resolve that mapping from documented list fields before trusting
// the material; never compare the opaque string with strconv.FormatInt(id).
func (c *ZovoClient) lookupCardBinding(ctx context.Context, cardID int64) (zovoCardBinding, error) {
	seen := make(map[int64]bool)
	expectedTotal := -1
	for page := 1; page <= zovoCardLookupMaxPages; page++ {
		if ctx.Err() != nil {
			return zovoCardBinding{}, zovoFailure("ZOVOCARD_REQUEST_CANCELLED", false, true)
		}
		var raw struct {
			Total *int `json:"total"`
			List  *[]struct {
				ID         *int64 `json:"id"`
				VMCardID   string `json:"vm_card_id"`
				CardNumber string `json:"card_number"`
			} `json:"list"`
		}
		path := fmt.Sprintf("/cards?page=%d&page_size=%d", page, zovoCardLookupPageSize)
		if err := c.cards.request(ctx, http.MethodGet, path, nil, "", &raw); err != nil {
			return zovoCardBinding{}, zovoError(err)
		}
		if raw.Total == nil || *raw.Total < 0 || raw.List == nil || len(*raw.List) > zovoCardLookupPageSize ||
			expectedTotal >= 0 && *raw.Total != expectedTotal {
			return zovoCardBinding{}, zovoFailure("ZOVOCARD_CONTRACT_DRIFT", false, false)
		}
		expectedTotal = *raw.Total
		var found *zovoCardBinding
		for _, item := range *raw.List {
			if item.ID == nil || *item.ID <= 0 || seen[*item.ID] {
				return zovoCardBinding{}, zovoFailure("ZOVOCARD_CONTRACT_DRIFT", false, false)
			}
			seen[*item.ID] = true
			if *item.ID == cardID {
				number, valid := zovoCardNumber(item.CardNumber)
				if !valid || item.VMCardID == "" || len(item.VMCardID) > 512 || strings.TrimSpace(item.VMCardID) != item.VMCardID || strings.IndexFunc(item.VMCardID, unicode.IsControl) >= 0 {
					return zovoCardBinding{}, zovoFailure("ZOVOCARD_CARD_BINDING_UNCONFIRMED", false, false)
				}
				found = &zovoCardBinding{vmCardID: item.VMCardID, number: number}
			}
		}
		if len(seen) > expectedTotal {
			return zovoCardBinding{}, zovoFailure("ZOVOCARD_CONTRACT_DRIFT", false, false)
		}
		if found != nil {
			return *found, nil
		}
		if len(seen) == expectedTotal {
			return zovoCardBinding{}, zovoFailure("ZOVOCARD_CARD_NOT_FOUND", false, false)
		}
		if len(*raw.List) < zovoCardLookupPageSize {
			return zovoCardBinding{}, zovoFailure("ZOVOCARD_CARD_LOOKUP_INCOMPLETE", false, false)
		}
	}
	return zovoCardBinding{}, zovoFailure("ZOVOCARD_CARD_LOOKUP_INCOMPLETE", false, false)
}

func zovoCardNumber(value string) (string, bool) {
	value = strings.NewReplacer(" ", "", "-", "").Replace(value)
	if len(value) < 12 || len(value) > 19 {
		return "", false
	}
	for _, digit := range value {
		if digit < '0' || digit > '9' {
			return "", false
		}
	}
	return value, luhn(value)
}

func (*ZovoClient) GetOpenAIPayments(context.Context, int64, []Transaction) ([]PriceSignal, error) {
	return nil, UnsupportedCapability(CardPlatformZovo, "membership price feeds")
}

var zovoIdempotencyKey = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,80}$`)

func finiteNonnegative(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0
}

func (c *ZovoClient) OpenCard(ctx context.Context, input OpenCardInput, key string) (OpenCardResult, error) {
	if !zovoIdempotencyKey.MatchString(key) {
		return OpenCardResult{}, zovoFailure("FUNDING_IDEMPOTENCY_KEY_INVALID", false, true)
	}
	holder, err := normalizeCardholder(Cardholder{FirstName: input.FirstName, LastName: input.LastName}, true)
	if err != nil || input.ProductCode == "" || len(input.ProductCode) > 128 ||
		strings.TrimSpace(input.ProductCode) != input.ProductCode || strings.IndexFunc(input.ProductCode, unicode.IsControl) >= 0 ||
		!finiteNonnegative(input.InitAmount) || input.InitAmount <= 0 {
		return OpenCardResult{}, zovoFailure("FUNDING_REQUEST_INVALID", false, true)
	}
	input.FirstName, input.LastName = holder.FirstName, holder.LastName
	var raw struct {
		ID              *int64   `json:"id"`
		VMCardID        string   `json:"vm_card_id"`
		ProductCode     string   `json:"product_code"`
		AvailableAmount *float64 `json:"available_amount"`
		Status          string   `json:"status"`
		OpenFee         *float64 `json:"open_fee"`
	}
	if err := c.write(ctx, "/cards/open", input, key, &raw); err != nil {
		return OpenCardResult{}, err
	}
	if raw.ID == nil || *raw.ID <= 0 || strings.TrimSpace(raw.VMCardID) == "" || raw.ProductCode != input.ProductCode ||
		raw.AvailableAmount == nil || !finiteNonnegative(*raw.AvailableAmount) ||
		raw.OpenFee == nil || !finiteNonnegative(*raw.OpenFee) || strings.TrimSpace(raw.Status) == "" {
		return OpenCardResult{}, zovoFailure("ZOVOCARD_WRITE_OUTCOME_UNCERTAIN", false, false)
	}
	return OpenCardResult{UpstreamCardID: *raw.ID, VMCardID: strings.TrimSpace(raw.VMCardID), ProductCode: raw.ProductCode,
		AvailableAmount: *raw.AvailableAmount, Status: strings.ToUpper(strings.TrimSpace(raw.Status)), OpenFee: *raw.OpenFee}, nil
}

func (c *ZovoClient) RechargeCard(ctx context.Context, cardID int64, amount float64, key string) error {
	if !zovoIdempotencyKey.MatchString(key) || cardID <= 0 || !finiteNonnegative(amount) || amount <= 0 {
		return zovoFailure("FUNDING_REQUEST_INVALID", false, true)
	}
	return c.write(ctx, "/cards/recharge", map[string]any{"card_id": cardID, "amount": amount}, key, nil)
}

func (c *ZovoClient) FreezeCard(ctx context.Context, cardID int64) error {
	if cardID <= 0 {
		return zovoFailure("CARD_FREEZE_REQUEST_INVALID", false, true)
	}
	return c.write(ctx, "/cards/freeze", map[string]any{"card_id": cardID, "freeze": true}, "", nil)
}

// write sends exactly one request. Unknown outcomes never carry a retry hint;
// the caller must retain its durable funding intent and reconcile separately.
func (c *ZovoClient) write(ctx context.Context, path string, input any, key string, output any) error {
	if ctx.Err() != nil {
		return zovoFailure("ZOVOCARD_REQUEST_CANCELLED", false, true)
	}
	if path != "/cards/open" && path != "/cards/recharge" && path != "/cards/freeze" {
		return zovoFailure("ZOVOCARD_REQUEST_INVALID", false, true)
	}
	body, err := json.Marshal(input)
	if err != nil {
		return zovoFailure("ZOVOCARD_REQUEST_INVALID", false, true)
	}
	request, err := newRequest(ctx, http.MethodPost, c.cards.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return zovoFailure("ZOVOCARD_REQUEST_INVALID", false, true)
	}
	// net/http treats Idempotency-Key as permission to replay a request whose
	// body can be recreated. Zovo caches only 2xx responses, so disable automatic
	// transport replay as well as application-level retry for these writes.
	request.GetBody = nil
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-API-Key", c.cards.appSecret)
	if c.cards.appID != "" {
		request.Header.Set("X-App-Id", c.cards.appID)
	}
	if key != "" {
		request.Header.Set("Idempotency-Key", key)
	}
	response, err := c.cards.http.Do(request)
	if err != nil {
		return zovoFailure("ZOVOCARD_WRITE_OUTCOME_UNCERTAIN", false, false)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusUnauthorized {
		return zovoFailure("ZOVOCARD_AUTH_FAILED", false, true)
	}
	if response.StatusCode == http.StatusForbidden {
		return zovoFailure("ZOVOCARD_ACCESS_DENIED", false, true)
	}
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		return zovoFailure("ZOVOCARD_REDIRECT_REFUSED", false, false)
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxSpaceXResponse+1))
	if err != nil || len(raw) > maxSpaceXResponse {
		return zovoFailure("ZOVOCARD_WRITE_OUTCOME_UNCERTAIN", false, false)
	}
	var envelope struct {
		Code      *int            `json:"code"`
		ErrorCode string          `json:"error_code"`
		Data      json.RawMessage `json:"data"`
	}
	if json.Unmarshal(raw, &envelope) != nil || envelope.Code == nil {
		return zovoFailure("ZOVOCARD_WRITE_OUTCOME_UNCERTAIN", false, false)
	}
	if response.StatusCode != http.StatusOK || *envelope.Code != 0 {
		// Only documented pre-write rejections count as definitely not executed.
		// request_failed and HTTP 400 with a human pending message remain unknown.
		known := (response.StatusCode == http.StatusBadRequest || response.StatusCode == http.StatusOK) &&
			map[string]bool{"invalid_argument": true, "not_found": true, "insufficient_balance": true, "forbidden": true}[envelope.ErrorCode]
		if known {
			return zovoFailure("ZOVOCARD_OPERATION_REJECTED", false, true)
		}
		return zovoFailure("ZOVOCARD_WRITE_OUTCOME_UNCERTAIN", false, false)
	}
	if len(envelope.Data) > 0 && string(envelope.Data) != "null" {
		var pending struct {
			Pending *bool `json:"pending"`
		}
		if json.Unmarshal(envelope.Data, &pending) != nil || pending.Pending != nil && *pending.Pending {
			return zovoFailure("ZOVOCARD_WRITE_OUTCOME_UNCERTAIN", false, false)
		}
	}
	if output != nil && (len(envelope.Data) == 0 || string(envelope.Data) == "null" || json.Unmarshal(envelope.Data, output) != nil) {
		return zovoFailure("ZOVOCARD_WRITE_OUTCOME_UNCERTAIN", false, false)
	}
	return nil
}

func zovoFailure(code string, retryable, knownNoWrite bool) error {
	return &Error{ErrorCode: code, Message: code, Retryable: retryable, KnownNoWrite: knownNoWrite}
}

func zovoError(err error) error {
	if err == nil {
		return nil
	}
	var source *Error
	if errors.As(err, &source) && source != nil {
		code := source.ErrorCode
		if strings.HasPrefix(code, "SPACEXCARD_") {
			code = "ZOVOCARD_" + strings.TrimPrefix(code, "SPACEXCARD_")
		}
		return zovoFailure(code, source.Retryable, source.KnownNoWrite)
	}
	return zovoFailure("ZOVOCARD_UNAVAILABLE", true, false)
}

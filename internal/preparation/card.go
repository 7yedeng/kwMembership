package preparation

import (
	"context"
	"math"
	"math/big"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"kwmembership/internal/checkout"
	"kwmembership/internal/provider"
)

type ReaderFactory func(provider.CardPlatformConfig) (provider.CardSnapshotReader, error)

// NewCardReader constructs a basic Zovo client. The returned interface exposes
// one selected-card snapshot: bounded directory reads followed by one detail
// read, with no funding or membership operation.
func NewCardReader(config provider.CardPlatformConfig) (provider.CardSnapshotReader, error) {
	platform, err := provider.NewCardPlatform(&http.Client{Timeout: 25 * time.Second}, config)
	if err != nil {
		return nil, fail("PREPARATION_CARD_CONFIGURATION_INVALID")
	}
	reader, ok := platform.(provider.CardSnapshotReader)
	if !ok || platform.Key() != config.Key {
		return nil, fail("PREPARATION_CARD_CONFIGURATION_INVALID")
	}
	return reader, nil
}

type cardResolver struct {
	mu       sync.Mutex
	input    Input
	factory  ReaderFactory
	now      func() time.Time
	loaded   bool
	material *checkout.Material
	review   *CardReview
	err      error
}

func (r *cardResolver) load(ctx context.Context) (*checkout.Material, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if r.loaded {
		return r.material, r.err
	}
	r.loaded = true
	r.err = fail("PREPARATION_CARD_UNAVAILABLE")
	if r.factory == nil {
		return nil, r.err
	}
	config := r.input.platform
	config.CredentialJSON = append([]byte(nil), config.CredentialJSON...)
	reader, err := r.factory(config)
	clear(config.CredentialJSON)
	if err != nil || reader == nil {
		return nil, r.err
	}
	snapshot, err := reader.GetCardSnapshot(ctx, r.input.cardID, r.now())
	if ctx.Err() != nil {
		r.err = ctx.Err()
		return nil, r.err
	}
	if err != nil {
		return nil, r.err
	}
	if snapshot.UpstreamCardID != r.input.cardID || !snapshot.CardBindingVerified {
		r.err = fail("PREPARATION_CARD_BINDING_MISMATCH")
		return nil, r.err
	}
	material := snapshot.Material
	month, monthErr := strconv.Atoi(material.ExpiryMonth)
	year, yearErr := strconv.Atoi(material.ExpiryYear)
	if material.Status != "ACTIVE" || monthErr != nil || yearErr != nil ||
		len(material.ExpiryMonth) != 2 || len(material.ExpiryYear) != 4 || month < 1 || month > 12 ||
		!regexp.MustCompile(`^[0-9]{12,19}$`).MatchString(material.Number) ||
		!regexp.MustCompile(`^[0-9]{3,4}$`).MatchString(material.CVV) ||
		math.IsNaN(snapshot.AvailableAmount) || math.IsInf(snapshot.AvailableAmount, 0) || snapshot.AvailableAmount < 0 ||
		snapshot.Currency != "" && !regexp.MustCompile(`^[A-Z]{3}$`).MatchString(snapshot.Currency) {
		r.err = fail("PREPARATION_CARD_INVALID")
		return nil, r.err
	}
	if !time.Date(year, time.Month(month)+1, 1, 0, 0, 0, 0, time.UTC).After(r.now().UTC()) {
		r.err = fail("PREPARATION_CARD_EXPIRED")
		return nil, r.err
	}
	blocked := snapshot.GoogleChatGPTBlocked != nil && *snapshot.GoogleChatGPTBlocked
	for _, merchant := range snapshot.RestrictedMerchants {
		normalized := strings.ToLower(strings.Join(strings.Fields(merchant), ""))
		if strings.Contains(normalized, "chatgpt") || strings.Contains(normalized, "openai") || normalized == "*" || normalized == "all" {
			blocked = true
		}
	}
	if blocked {
		r.err = fail("PREPARATION_CARD_BLOCKED")
		return nil, r.err
	}
	address, err := provider.BillingAddressFromMaterial(material)
	if err != nil {
		r.err = fail("PREPARATION_BILLING_CONFLICT")
		return nil, r.err
	}
	address, source, err := mergeBilling(address, r.input.billing)
	if err != nil {
		r.err = err
		return nil, err
	}
	r.material = &checkout.Material{Card: checkout.CardMaterial{
		Number: material.Number, CVV: material.CVV, ExpiryMonth: material.ExpiryMonth, ExpiryYear: material.ExpiryYear,
	}, Billing: checkout.BillingAddress{Name: address.Name, Line1: address.Line1, Line2: address.Line2,
		City: address.City, State: address.State, PostalCode: address.PostalCode, Country: address.Country}}
	r.review = &CardReview{CardBindingVerified: true, Last4: material.Number[len(material.Number)-4:], AvailableAmount: strconv.FormatFloat(snapshot.AvailableAmount, 'f', -1, 64),
		Currency: snapshot.Currency, BalanceCurrencyKnown: snapshot.Currency != "", BillingSource: source,
		MerchantRestrictionsKnown: snapshot.MerchantRestrictionsKnown}
	r.err = nil
	return r.material, nil
}

func mergeBilling(observed provider.BillingAddress, supplied *provider.BillingAddress) (provider.BillingAddress, string, error) {
	address, err := provider.NormalizeBillingAddress(observed)
	if err != nil {
		return provider.BillingAddress{}, "", fail("PREPARATION_BILLING_INVALID")
	}
	source := "provider"
	if supplied != nil {
		for _, field := range []struct {
			actual   *string
			supplied string
		}{
			{&address.Name, supplied.Name}, {&address.Phone, supplied.Phone}, {&address.Line1, supplied.Line1}, {&address.Line2, supplied.Line2},
			{&address.City, supplied.City}, {&address.State, supplied.State}, {&address.PostalCode, supplied.PostalCode}, {&address.Country, supplied.Country},
		} {
			if *field.actual != "" && field.supplied != "" && !strings.EqualFold(*field.actual, field.supplied) {
				return provider.BillingAddress{}, "", fail("PREPARATION_BILLING_CONFLICT")
			}
			if *field.actual == "" && field.supplied != "" {
				*field.actual = field.supplied
				source = "provider_and_bound_input"
			}
		}
	}
	if provider.ValidateBillingAddress(address) != nil {
		return provider.BillingAddress{}, "", fail("PREPARATION_BILLING_INCOMPLETE")
	}
	return address, source, nil
}

func (r *cardResolver) billing(ctx context.Context) (*checkout.BillingAddress, error) {
	material, err := r.load(ctx)
	if err != nil {
		return nil, err
	}
	copy := material.Billing
	return &copy, nil
}

func (r *cardResolver) resolve(ctx context.Context, quote checkout.ReviewedQuote) (*checkout.Material, error) {
	material, err := r.load(ctx)
	if err != nil {
		return nil, err
	}
	if _, _, err := r.summary(&quote); err != nil {
		r.mu.Lock()
		r.err = err
		r.mu.Unlock()
		return nil, err
	}
	copy := *material
	return &copy, nil
}

func (r *cardResolver) summary(quote *checkout.ReviewedQuote) (*CardReview, []string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return nil, nil, r.err
	}
	if r.review == nil {
		return nil, nil, nil
	}
	review := *r.review
	var unconfirmed []string
	if !review.BalanceCurrencyKnown {
		unconfirmed = append(unconfirmed, "card_balance_currency")
	}
	if quote != nil && review.Currency == quote.Currency && quote.MinorUnitScale == 2 {
		amount, ok := new(big.Rat).SetString(review.AvailableAmount)
		if !ok {
			return nil, nil, fail("PREPARATION_CARD_INVALID")
		}
		review.BalanceComparable = true
		sufficient := amount.Cmp(new(big.Rat).SetFrac(big.NewInt(quote.TotalMinor), big.NewInt(100))) >= 0
		review.BalanceSufficient = &sufficient
		if !sufficient {
			return &review, unconfirmed, fail("PREPARATION_BALANCE_INSUFFICIENT")
		}
	} else {
		unconfirmed = append(unconfirmed, "card_balance_sufficiency")
		if review.Currency != "" {
			unconfirmed = append(unconfirmed, "card_fx_conversion")
		}
	}
	if !review.MerchantRestrictionsKnown {
		unconfirmed = append(unconfirmed, "merchant_restrictions")
	}
	return &review, unconfirmed, nil
}

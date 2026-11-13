package processor

import (
	"context"
	"errors"
	"strings"

	"kwmembership/internal/provider"
)

// CardBillingService resolves an explicitly configured profile for one card.
// Implementations must not select a profile by amount, product, or random choice.
type CardBillingService interface {
	BillingForCard(context.Context, provider.CardReference) (provider.BillingAddress, error)
}

func (p *Processor) resolveCardBilling(ctx context.Context, reference provider.CardReference, material provider.CardMaterial) (provider.BillingAddress, error) {
	if err := ctx.Err(); err != nil {
		return provider.BillingAddress{}, err
	}
	if !reference.Valid() {
		return provider.BillingAddress{}, coded("BILLING_CARD_BINDING_INVALID", "billing card reference is invalid")
	}
	observed, err := provider.BillingAddressFromMaterial(material)
	if err != nil {
		return provider.BillingAddress{}, err
	}
	if provider.ValidateBillingAddress(observed) == nil {
		// Complete issuer facts take precedence. A stale configuration cannot
		// replace the current card's address and no generator is contacted.
		return observed, nil
	}
	var configured provider.BillingAddress
	if bound, ok := p.address.(CardBillingService); ok {
		configured, err = bound.BillingForCard(ctx, reference)
	} else if p.requireCardBillingBinding || p.address == nil {
		return provider.BillingAddress{}, coded("BILLING_CARD_BINDING_REQUIRED", "incomplete issuer billing requires a profile bound to this card")
	} else {
		// Only the explicit compatibility constructors retain this old lookup.
		// Even there, it cannot override a conflicting fact returned by the card.
		configured, err = p.address.Billing(ctx)
	}
	if err != nil {
		return provider.BillingAddress{}, sanitizeBillingError(err)
	}
	if err := ctx.Err(); err != nil {
		return provider.BillingAddress{}, err
	}
	configured, err = provider.NormalizeBillingAddress(configured)
	if err != nil {
		return provider.BillingAddress{}, err
	}
	if err := provider.ValidateBillingAddress(configured); err != nil {
		return provider.BillingAddress{}, err
	}
	// Retain every known issuer field; configuration may only fill a gap. A
	// mismatch is stopped, never repaired by switching to a different address.
	for _, field := range []struct {
		observed string
		target   *string
	}{
		{observed.Name, &configured.Name}, {observed.Phone, &configured.Phone},
		{observed.Line1, &configured.Line1}, {observed.Line2, &configured.Line2},
		{observed.City, &configured.City}, {observed.State, &configured.State},
		{observed.PostalCode, &configured.PostalCode}, {observed.Country, &configured.Country},
	} {
		if field.observed == "" {
			continue
		}
		if *field.target != "" && !strings.EqualFold(field.observed, *field.target) {
			return provider.BillingAddress{}, coded("CARD_BILLING_CONFLICT", "configured billing disagrees with this card's returned facts")
		}
		*field.target = field.observed
	}
	if err := provider.ValidateBillingAddress(configured); err != nil {
		return provider.BillingAddress{}, err
	}
	return configured, nil
}

type billingServiceError struct {
	code  string
	cause error
}

func (err *billingServiceError) Error() string { return "card-bound billing lookup failed" }
func (err *billingServiceError) Code() string  { return err.code }
func (err *billingServiceError) Unwrap() error { return err.cause }

func sanitizeBillingError(err error) error {
	code := "CARD_BILLING_UNAVAILABLE"
	var classified interface{ Code() string }
	if errors.As(err, &classified) {
		switch classified.Code() {
		case "BILLING_CARD_BINDING_INVALID", "BILLING_CARD_BINDING_REQUIRED", "CARD_BILLING_NOT_CONFIGURED", "BILLING_PROFILE_INVALID", "CARD_BILLING_CONFLICT":
			code = classified.Code()
		}
	}
	return &billingServiceError{code: code, cause: err}
}

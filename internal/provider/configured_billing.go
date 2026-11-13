package provider

import (
	"context"
	"strings"
	"unicode"
)

// BillingProfile contains billing details supplied by the card owner or card
// platform. Construction validates structure only; it does not verify these
// details with an issuer or generate substitute names/addresses.
type BillingProfile struct {
	Holder  Cardholder
	Address BillingAddress
}

// ConfiguredBillingService serves one immutable billing profile without network
// requests. It is a compatibility service without a card binding. Owned payment
// processors require CardBoundBillingService when issuer details are incomplete.
type ConfiguredBillingService struct {
	profile BillingProfile
}

func NewConfiguredBillingService(profile BillingProfile) (*ConfiguredBillingService, error) {
	var err error
	profile.Holder, err = normalizeCardholder(profile.Holder, true)
	if err != nil {
		return nil, err
	}
	profile.Address, err = NormalizeBillingAddress(profile.Address)
	if err != nil {
		return nil, err
	}
	if profile.Address.Name == "" {
		profile.Address.Name = profile.Holder.FirstName + " " + profile.Holder.LastName
	}
	if err := ValidateBillingAddress(profile.Address); err != nil {
		return nil, err
	}
	return &ConfiguredBillingService{profile: profile}, nil
}

// NormalizeBillingAddress validates every supplied field and preserves missing
// fields. It does not generate an address, verify it with an issuer, or assert
// that a particular checkout supports it. Controls are rejected before trimming.
func NormalizeBillingAddress(address BillingAddress) (BillingAddress, error) {
	fields := []struct {
		value *string
		limit int
	}{
		{&address.Name, 120}, {&address.Phone, 40},
		{&address.Line1, 160}, {&address.Line2, 160}, {&address.City, 100},
		{&address.State, 64}, {&address.PostalCode, 32}, {&address.Country, 2},
	}
	for _, field := range fields {
		if strings.IndexFunc(*field.value, unicode.IsControl) >= 0 {
			return BillingAddress{}, fail("BILLING_PROFILE_INVALID", "billing profile contains invalid text", false)
		}
		*field.value = strings.TrimSpace(*field.value)
		if !billingText(*field.value, 0, field.limit) {
			return BillingAddress{}, fail("BILLING_PROFILE_INVALID", "billing profile contains malformed fields", false)
		}
	}
	address.Country = strings.ToUpper(address.Country)
	if address.Country != "" && !billingCountry(address.Country) {
		return BillingAddress{}, fail("BILLING_PROFILE_INVALID", "billing profile country is malformed", false)
	}
	return address, nil
}

// ValidateBillingAddress requires a normalized complete address. State, postal
// code and the second address line remain optional here; the actual checkout's
// required-field and validity evidence can impose stricter requirements.
func ValidateBillingAddress(address BillingAddress) error {
	normalized, err := NormalizeBillingAddress(address)
	if err != nil {
		return err
	}
	if normalized != address || address.Name == "" || address.Line1 == "" || address.City == "" || address.Country == "" {
		return fail("BILLING_PROFILE_INVALID", "billing profile is incomplete or not normalized", false)
	}
	return nil
}

func normalizeCardholder(holder Cardholder, required bool) (Cardholder, error) {
	minLength := 0
	if required {
		minLength = 1
	}
	for _, value := range []*string{&holder.FirstName, &holder.LastName} {
		if strings.IndexFunc(*value, unicode.IsControl) >= 0 {
			return Cardholder{}, fail("BILLING_PROFILE_INVALID", "cardholder contains invalid text", false)
		}
		*value = strings.TrimSpace(*value)
		if !billingText(*value, minLength, 100) {
			return Cardholder{}, fail("BILLING_PROFILE_INVALID", "cardholder is incomplete or malformed", false)
		}
	}
	return holder, nil
}

func (service *ConfiguredBillingService) Cardholder(ctx context.Context) (Cardholder, error) {
	if err := ctx.Err(); err != nil {
		return Cardholder{}, err
	}
	return service.profile.Holder, nil
}

func (service *ConfiguredBillingService) Billing(ctx context.Context) (BillingAddress, error) {
	if err := ctx.Err(); err != nil {
		return BillingAddress{}, err
	}
	return service.profile.Address, nil
}

func billingText(value string, min, max int) bool {
	return len(value) >= min && len(value) <= max
}

func billingCountry(value string) bool {
	// This checks the two-letter representation, not issuer support, taxes,
	// checkout eligibility or the existence of an address in this country.
	return len(value) == 2 && value[0] >= 'A' && value[0] <= 'Z' && value[1] >= 'A' && value[1] <= 'Z'
}

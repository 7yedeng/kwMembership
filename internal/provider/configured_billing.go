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
// requests. Use it only when every card assigned to this processor has this
// billing profile. A multi-profile inventory needs a card-bound resolver, not a
// randomly selected or process-wide fallback address.
type ConfiguredBillingService struct {
	profile BillingProfile
}

func NewConfiguredBillingService(profile BillingProfile) (*ConfiguredBillingService, error) {
	// Reject controls before trimming so a malformed value is not silently
	// converted into an apparently valid name or address. Errors contain no PII.
	values := []string{
		profile.Holder.FirstName, profile.Holder.LastName, profile.Address.Name,
		profile.Address.Phone, profile.Address.Line1, profile.Address.City,
		profile.Address.State, profile.Address.PostalCode, profile.Address.Country,
	}
	for _, value := range values {
		if strings.IndexFunc(value, unicode.IsControl) >= 0 {
			return nil, fail("BILLING_PROFILE_INVALID", "billing profile contains invalid text", false)
		}
	}
	profile.Holder.FirstName = strings.TrimSpace(profile.Holder.FirstName)
	profile.Holder.LastName = strings.TrimSpace(profile.Holder.LastName)
	profile.Address.Name = strings.TrimSpace(profile.Address.Name)
	profile.Address.Phone = strings.TrimSpace(profile.Address.Phone)
	profile.Address.Line1 = strings.TrimSpace(profile.Address.Line1)
	profile.Address.City = strings.TrimSpace(profile.Address.City)
	profile.Address.State = strings.TrimSpace(profile.Address.State)
	profile.Address.PostalCode = strings.TrimSpace(profile.Address.PostalCode)
	profile.Address.Country = strings.ToUpper(strings.TrimSpace(profile.Address.Country))
	if profile.Address.Name == "" {
		profile.Address.Name = profile.Holder.FirstName + " " + profile.Holder.LastName
	}
	if !billingText(profile.Holder.FirstName, 1, 100) || !billingText(profile.Holder.LastName, 1, 100) ||
		!billingText(profile.Address.Name, 2, 120) || !billingText(profile.Address.Line1, 2, 160) ||
		!billingText(profile.Address.City, 1, 100) || !billingText(profile.Address.State, 0, 64) ||
		!billingText(profile.Address.PostalCode, 0, 32) || !billingText(profile.Address.Phone, 0, 40) ||
		!billingCountry(profile.Address.Country) {
		return nil, fail("BILLING_PROFILE_INVALID", "billing profile is incomplete or malformed", false)
	}
	return &ConfiguredBillingService{profile: profile}, nil
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

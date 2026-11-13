package provider

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func configuredBillingFixture() BillingProfile {
	return BillingProfile{
		Holder: Cardholder{FirstName: " Fixture ", LastName: " Holder "},
		Address: BillingAddress{
			Line1: " 1 Test Street ", City: " Test City ", State: "CA", PostalCode: "00000", Country: " us ",
		},
	}
}

func TestConfiguredBillingKeepsOneSuppliedProfile(t *testing.T) {
	profile := configuredBillingFixture()
	service, err := NewConfiguredBillingService(profile)
	if err != nil {
		t.Fatal(err)
	}
	// Caller mutation after construction must not change future card opening or
	// checkout material. Returned values must not mutate the stored profile.
	profile.Holder.FirstName = "Different"
	profile.Address.Line1 = "Changed"
	holder, err := service.Cardholder(context.Background())
	if err != nil || holder != (Cardholder{FirstName: "Fixture", LastName: "Holder"}) {
		t.Fatal("cardholder differs from the supplied profile")
	}
	address, err := service.Billing(context.Background())
	if err != nil || address.Name != "Fixture Holder" || address.Line1 != "1 Test Street" || address.Country != "US" || address.State != "CA" {
		t.Fatal("billing data was generated or differs from the supplied profile")
	}
	address.Line1 = "Changed again"
	next, err := service.Billing(context.Background())
	if err != nil || next.Line1 != "1 Test Street" {
		t.Fatal("returned profile was mutable")
	}
}

func TestConfiguredBillingRejectsIncompleteOrMalformedDataWithoutPII(t *testing.T) {
	for name, mutate := range map[string]func(*BillingProfile){
		"missing holder":   func(p *BillingProfile) { p.Holder.FirstName = "" },
		"missing street":   func(p *BillingProfile) { p.Address.Line1 = "" },
		"missing city":     func(p *BillingProfile) { p.Address.City = "" },
		"country name":     func(p *BillingProfile) { p.Address.Country = "United States" },
		"control in name":  func(p *BillingProfile) { p.Address.Name = "secret-name\n" },
		"oversized street": func(p *BillingProfile) { p.Address.Line1 = strings.Repeat("x", 161) },
	} {
		t.Run(name, func(t *testing.T) {
			profile := configuredBillingFixture()
			mutate(&profile)
			service, err := NewConfiguredBillingService(profile)
			if err == nil || service != nil {
				t.Fatal("invalid profile was accepted")
			}
			var coded *Error
			if !errors.As(err, &coded) || coded.ErrorCode != "BILLING_PROFILE_INVALID" || coded.Retryable {
				t.Fatal("invalid profile must fail before checkout without retry")
			}
			if strings.Contains(err.Error(), "secret-name") || strings.Contains(err.Error(), "Test Street") {
				t.Fatal("error disclosed billing data")
			}
		})
	}
}

func TestConfiguredBillingHonorsCancellation(t *testing.T) {
	service, err := NewConfiguredBillingService(configuredBillingFixture())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if holder, err := service.Cardholder(ctx); !errors.Is(err, context.Canceled) || holder != (Cardholder{}) {
		t.Fatal("cancelled cardholder request returned material")
	}
	if address, err := service.Billing(ctx); !errors.Is(err, context.Canceled) || address != (BillingAddress{}) {
		t.Fatal("cancelled billing request returned material")
	}
}

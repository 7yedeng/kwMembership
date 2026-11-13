package provider

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestCardBoundBillingDoesNotCrossCardsOrPlatforms(t *testing.T) {
	one := CardReference{PlatformKey: "platform-a", UpstreamCardID: 17}
	two := CardReference{PlatformKey: "platform-b", UpstreamCardID: 17}
	profile := configuredBillingFixture()
	profile.Address.Name = "Fixture Holder"
	profile.Address.Line2 = " Suite 2 "
	profiles := map[CardReference]BillingAddress{one: profile.Address}
	other := profile.Address
	other.Line1 = "99 Separate Street"
	profiles[two] = other
	service, err := NewCardBoundBillingService(profile.Holder, profiles)
	if err != nil {
		t.Fatal(err)
	}
	profiles[one] = other
	first, err := service.BillingForCard(context.Background(), one)
	if err != nil || first.Line1 != "1 Test Street" || first.Line2 != "Suite 2" {
		t.Fatal("card profile was changed by caller mutation or mixed with another platform")
	}
	second, err := service.BillingForCard(context.Background(), two)
	if err != nil || second.Line1 != "99 Separate Street" {
		t.Fatal("platform identity was ignored")
	}
	first.Line1 = "Mutated return"
	again, _ := service.BillingForCard(context.Background(), one)
	if again.Line1 != "1 Test Street" {
		t.Fatal("returned profile can mutate service state")
	}
	if _, err := service.BillingForCard(context.Background(), CardReference{PlatformKey: "platform-a", UpstreamCardID: 18}); billingErrorCode(err) != "CARD_BILLING_NOT_CONFIGURED" {
		t.Fatal("unconfigured card received a profile")
	}
	if _, err := service.Billing(context.Background()); billingErrorCode(err) != "BILLING_CARD_BINDING_REQUIRED" {
		t.Fatal("unscoped lookup returned card data")
	}
}

func TestCardBoundBillingRejectsBadBindingsAndMalformedSecondLine(t *testing.T) {
	profile := configuredBillingFixture()
	profile.Address.Name = "Fixture Holder"
	for _, reference := range []CardReference{
		{}, {PlatformKey: "platform-a", UpstreamCardID: 0},
		{PlatformKey: " platform-a", UpstreamCardID: 1},
		{PlatformKey: "platform\na", UpstreamCardID: 1},
	} {
		if _, err := NewCardBoundBillingService(profile.Holder, map[CardReference]BillingAddress{reference: profile.Address}); billingErrorCode(err) != "BILLING_CARD_BINDING_INVALID" {
			t.Fatal("invalid card reference was accepted")
		}
	}
	profile.Address.Line2 = "private-suite\n"
	if _, err := NewConfiguredBillingService(profile); billingErrorCode(err) != "BILLING_PROFILE_INVALID" || strings.Contains(err.Error(), "private-suite") {
		t.Fatal("second-line controls were accepted or disclosed")
	}
}

func TestBillingNormalizationPreservesUnknownFieldsAndOptionalRegion(t *testing.T) {
	partial := BillingAddress{Name: " Only Name ", Line2: " Suite 9 ", Country: " sg "}
	got, err := NormalizeBillingAddress(partial)
	if err != nil || got.Name != "Only Name" || got.Line2 != "Suite 9" || got.Country != "SG" || got.Line1 != "" || got.PostalCode != "" {
		t.Fatal("normalization lost or generated address facts")
	}
	if ValidateBillingAddress(got) == nil {
		t.Fatal("partial address was treated as complete")
	}
	got.Line1, got.City = "1 Test Lane", "Fixture City"
	if err := ValidateBillingAddress(got); err != nil {
		t.Fatal("state or postal code was fabricated as a universal requirement")
	}
	if got.State != "" || got.PostalCode != "" {
		t.Fatal("optional regional fields were generated")
	}
}

func TestBillingMaterialExtractionPreservesMissingAndRejectsInvalidFacts(t *testing.T) {
	got, err := BillingAddressFromMaterial(CardMaterial{Holder: Cardholder{FirstName: "Only"}})
	if err != nil || got.Name != "Only" || got.Line1 != "" || got.Country != "" {
		t.Fatal("missing issuer address was replaced")
	}
	_, err = BillingAddressFromMaterial(CardMaterial{
		HolderName: "Visible Holder", Holder: Cardholder{FirstName: "private\n"},
	})
	if billingErrorCode(err) != "BILLING_PROFILE_INVALID" || strings.Contains(err.Error(), "private") {
		t.Fatal("invalid unused holder field was ignored or disclosed")
	}
	_, err = BillingAddressFromMaterial(CardMaterial{
		HolderName: "Card Holder", Billing: &BillingAddress{Name: "Different Holder"},
	})
	if billingErrorCode(err) != "CARD_BILLING_CONFLICT" {
		t.Fatal("contradictory holder identities were accepted")
	}
}

func TestCardBoundBillingHonorsCancellationWithoutReturningData(t *testing.T) {
	profile := configuredBillingFixture()
	profile.Address.Name = "Fixture Holder"
	reference := CardReference{PlatformKey: "platform-a", UpstreamCardID: 1}
	service, err := NewCardBoundBillingService(profile.Holder, map[CardReference]BillingAddress{reference: profile.Address})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if address, err := service.BillingForCard(ctx, reference); !errors.Is(err, context.Canceled) || address != (BillingAddress{}) {
		t.Fatal("cancelled lookup returned billing data")
	}
}

func TestExistingCardInventoryDoesNotNeedAnInventedOpeningHolder(t *testing.T) {
	reference := CardReference{PlatformKey: "platform-a", UpstreamCardID: 1}
	address := BillingAddress{Name: "Existing Holder", Line1: "1 Test Street", City: "Fixture City", Country: "SG"}
	service, err := NewCardBoundBillingService(Cardholder{}, map[CardReference]BillingAddress{reference: address})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := service.BillingForCard(context.Background(), reference); err != nil || got != address {
		t.Fatal("existing-card billing unnecessarily required an opening identity")
	}
	if holder, err := service.Cardholder(context.Background()); billingErrorCode(err) != "NEW_CARD_HOLDER_UNAVAILABLE" || holder != (Cardholder{}) {
		t.Fatal("missing opening holder was generated or accepted for a new card")
	}
	if _, err := NewCardBoundBillingService(Cardholder{FirstName: "Only"}, nil); err == nil {
		t.Fatal("partial opening holder was accepted")
	}
}

func billingErrorCode(err error) string {
	var coded interface{ Code() string }
	if errors.As(err, &coded) {
		return coded.Code()
	}
	return ""
}

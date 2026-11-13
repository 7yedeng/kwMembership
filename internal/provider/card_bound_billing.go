package provider

import (
	"context"
	"strings"
	"unicode"
)

// CardReference identifies a card in one configured platform account. PlatformKey
// must be the durable membership_card_platforms key, not a product or issuer ID.
type CardReference struct {
	PlatformKey    string
	UpstreamCardID int64
}

func (reference CardReference) Valid() bool {
	return reference.UpstreamCardID > 0 && len(reference.PlatformKey) > 0 && len(reference.PlatformKey) <= 128 &&
		strings.TrimSpace(reference.PlatformKey) == reference.PlatformKey &&
		strings.IndexFunc(reference.PlatformKey, unicode.IsControl) < 0
}

// CardBoundBillingService stores explicitly supplied, immutable billing profiles
// per card. It does not infer that a newly opened card uses the opening holder's
// address. New cards need issuer-returned details or their own configured entry.
// An entirely empty opening holder supports existing-card-only inventories;
// attempting to open a new card then fails instead of generating a name.
type CardBoundBillingService struct {
	holder   Cardholder
	profiles map[CardReference]BillingAddress
}

func NewCardBoundBillingService(holder Cardholder, profiles map[CardReference]BillingAddress) (*CardBoundBillingService, error) {
	holder, err := normalizeCardholder(holder, false)
	if err != nil {
		return nil, err
	}
	if (holder.FirstName == "") != (holder.LastName == "") {
		return nil, fail("BILLING_PROFILE_INVALID", "opening cardholder must be complete or absent", false)
	}
	service := &CardBoundBillingService{holder: holder, profiles: make(map[CardReference]BillingAddress, len(profiles))}
	for reference, address := range profiles {
		if !reference.Valid() {
			return nil, fail("BILLING_CARD_BINDING_INVALID", "billing card reference is invalid", false)
		}
		address, err = NormalizeBillingAddress(address)
		if err != nil {
			return nil, err
		}
		if err := ValidateBillingAddress(address); err != nil {
			return nil, err
		}
		service.profiles[reference] = address
	}
	return service, nil
}

func (service *CardBoundBillingService) Cardholder(ctx context.Context) (Cardholder, error) {
	if err := ctx.Err(); err != nil {
		return Cardholder{}, err
	}
	if service.holder.FirstName == "" || service.holder.LastName == "" {
		return Cardholder{}, fail("NEW_CARD_HOLDER_UNAVAILABLE", "opening cardholder is not configured", false)
	}
	return service.holder, nil
}

func (service *CardBoundBillingService) Billing(ctx context.Context) (BillingAddress, error) {
	if err := ctx.Err(); err != nil {
		return BillingAddress{}, err
	}
	return BillingAddress{}, fail("BILLING_CARD_BINDING_REQUIRED", "billing lookup requires a specific card reference", false)
}

func (service *CardBoundBillingService) BillingForCard(ctx context.Context, reference CardReference) (BillingAddress, error) {
	if err := ctx.Err(); err != nil {
		return BillingAddress{}, err
	}
	if !reference.Valid() {
		return BillingAddress{}, fail("BILLING_CARD_BINDING_INVALID", "billing card reference is invalid", false)
	}
	address, ok := service.profiles[reference]
	if !ok {
		return BillingAddress{}, fail("CARD_BILLING_NOT_CONFIGURED", "billing details are not configured for this card", false)
	}
	return address, nil
}

// BillingAddressFromMaterial extracts only billing facts supplied in this card's
// material response. Missing fields remain missing, including a wholly absent
// address. It never guesses how to split a holder name or generates an identity.
func BillingAddressFromMaterial(material CardMaterial) (BillingAddress, error) {
	holder, err := normalizeCardholder(material.Holder, false)
	if err != nil {
		return BillingAddress{}, err
	}
	name, err := NormalizeBillingAddress(BillingAddress{Name: material.HolderName})
	if err != nil {
		return BillingAddress{}, err
	}
	if name.Name == "" {
		name.Name = strings.TrimSpace(holder.FirstName + " " + holder.LastName)
		name, err = NormalizeBillingAddress(name)
		if err != nil {
			return BillingAddress{}, err
		}
	}
	address := BillingAddress{}
	if material.Billing != nil {
		address, err = NormalizeBillingAddress(*material.Billing)
		if err != nil {
			return BillingAddress{}, err
		}
	}
	if address.Name == "" {
		address.Name = name.Name
	} else if name.Name != "" && !strings.EqualFold(address.Name, name.Name) {
		return BillingAddress{}, fail("CARD_BILLING_CONFLICT", "card billing facts disagree", false)
	}
	return address, nil
}

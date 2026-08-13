package provider

import (
	"context"
	"time"
)

const (
	CardPlatformSpaceX = "spacexcard"
	CardPlatformEfun   = "efuncard"
)

// CardPlatform is the seam between membership workflow ownership and a card
// platform adapter. All values crossing it are normalized; credentials and
// provider-specific response shapes remain inside the adapter.
type CardPlatform interface {
	Key() string
	Capabilities() CardPlatformCapabilities
	ListProducts(context.Context) ([]Product, error)
	ListCards(context.Context, int, int, bool) (int, []Card, error)
	ListTransactions(context.Context, int64, int, int) ([]Transaction, error)
	GetOpenAIPayments(context.Context, int64, []Transaction) ([]PriceSignal, error)
	GetBalance(context.Context) (Balance, error)
	GetCardMaterial(context.Context, int64, time.Time) (CardMaterial, error)
	OpenCard(context.Context, OpenCardInput, string) (OpenCardResult, error)
	RechargeCard(context.Context, int64, float64, string) error
	FreezeCard(context.Context, int64) error
}

type CardPlatformCapabilities struct {
	Freeze              bool
	OpenCard            bool
	Recharge            bool
	RechargeIdempotent  bool
	FundingReplayWindow time.Duration
}

func UnsupportedCapability(platform, capability string) error {
	return &Error{
		ErrorCode:    "CARD_PLATFORM_CAPABILITY_UNSUPPORTED",
		Message:      platform + " does not support " + capability,
		Retryable:    false,
		KnownNoWrite: true,
	}
}

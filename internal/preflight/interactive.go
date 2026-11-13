package preflight

import (
	"context"
	"io"

	"kwmembership/internal/checkout"
)

// Interactive metadata deliberately has no Session, token, card or browser
// profile fields. Authentication happens directly inside a fresh browser.
type interactiveInput struct {
	ExpectedEmail string        `json:"expectedEmail"`
	TargetTier    string        `json:"targetTier"`
	PriceContract priceContract `json:"priceContract"`
}

func DecodeInteractive(reader io.Reader) (Prepared, error) {
	raw, err := readBounded(reader, MaxInputBytes)
	if err != nil {
		return Prepared{}, err
	}
	var item interactiveInput
	if err := decodeSingle(raw, &item); err != nil {
		return Prepared{}, err
	}
	request, err := metadataRequest(item.ExpectedEmail, item.TargetTier, item.PriceContract, checkout.ModeInteractivePreflight)
	if err != nil {
		return Prepared{}, err
	}
	return Prepared{validated: true, request: request}, nil
}

// RunInteractive waits for the user to log in and navigate to checkout in the
// new browser. The executor does not collect/export account tokens or create
// checkout sessions programmatically on this path. User navigation may do so.
// notify has no arguments, so raw handoff pages and identities cannot escape.
func RunInteractive(ctx context.Context, prepared Prepared, executor checkout.Executor, notify func() error) (Report, error) {
	if prepared.request.Mode != checkout.ModeInteractivePreflight || notify == nil {
		return Failure("interactive-preflight", "PREFLIGHT_INPUT_INVALID"), fail("PREFLIGHT_INPUT_INVALID")
	}
	prepared.request.OnHandoff = func(ctx context.Context, _ checkout.Handoff) error {
		if err := ctx.Err(); err != nil {
			return fail(SafeCode(err))
		}
		if err := notify(); err != nil {
			return fail("INTERACTIVE_LOGIN_HANDOFF_FAILED")
		}
		return nil
	}
	return execute(ctx, prepared, executor, "interactive-preflight")
}

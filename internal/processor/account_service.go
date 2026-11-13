package processor

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"kwmembership/internal/domain"
	"kwmembership/internal/provider"
)

// These small interfaces retain the legacy wire adapters without coupling the
// processor to their concrete implementations.
type membershipReader interface {
	Fetch(context.Context, json.RawMessage) ([]byte, error)
}

type renewalCanceller interface {
	Cancel(context.Context, json.RawMessage, string) error
}

// BillingService supplies explicitly configured cardholder and billing facts.
// Implementations must return an error for missing data, never generate a
// substitute identity or treat a configured address as bank-verified evidence.
type BillingService interface {
	Cardholder(context.Context) (provider.Cardholder, error)
	Billing(context.Context) (provider.BillingAddress, error)
}

// AccountService is the integration boundary for an owned account service.
// Observe must authenticate the supplied Session, return that account's current
// authoritative state, and leave unavailable facts unknown. In particular, an
// absent subscription field, rejected Session or unsupported response is not
// evidence of a free account. Email is the identity reported by that service;
// the processor also compares it to a Session email when one is present.
//
// CancelRenewal must perform a supported cancellation or return an error (for
// example ErrAccountRenewalUnsupported). Its success is only an acknowledgement:
// the processor still requires a subsequent Observe call before confirmation.
// That observation's timestamp must be no earlier than cancellation completion
// or the preceding observation; adapters must not relabel cached state as fresh.
// Implementations own their authentication/configuration and must not include
// Sessions, provider credentials or raw upstream responses in returned errors.
// Errors may implement Code() string to retain existing retry/session handling.
// Unclassified observation failures are treated as provider unavailability,
// never as evidence that a dependency probe succeeded.
// No network implementation or undocumented cancellation endpoint is implied by
// this contract.
type AccountService interface {
	Observe(context.Context, json.RawMessage) (AccountObservation, error)
	CancelRenewal(context.Context, json.RawMessage) error
}

// AccountObservation preserves unknown booleans as nil. An expired paid
// subscription remains paid until the service explicitly observes a free tier.
type AccountObservation struct {
	Email        string
	Tier         domain.Tier
	Currency     string
	AutoRenew    *bool
	IsOverdue    *bool
	IsDelinquent *bool
	ExpiresAt    *time.Time
	ObservedAt   time.Time
}

// AccountObservationMaxAge bounds cached observations and tolerated clock skew.
// It does not replace the requirement to re-observe after cancellation/payment.
const AccountObservationMaxAge = 5 * time.Minute

var ErrAccountRenewalUnsupported = coded("RENEWAL_CANCEL_UNSUPPORTED", "account service does not support renewal cancellation")

// Keep an unclassified error available to errors.Is/As without printing a raw
// upstream error, which may contain a Session-bearing URL or response body.
type accountServiceError struct {
	code, message string
	cause         error
}

func (e *accountServiceError) Error() string { return e.message }
func (e *accountServiceError) Code() string  { return e.code }
func (e *accountServiceError) Unwrap() error { return e.cause }

func classifyAccountServiceError(err error, fallbackCode, message string) error {
	if err == nil {
		return nil
	}
	var classified interface{ Code() string }
	if errors.As(err, &classified) && classified.Code() != "" {
		return err
	}
	if code := domain.ErrorCode(err); code != "" {
		fallbackCode = code
	}
	return &accountServiceError{code: fallbackCode, message: message, cause: err}
}

func (p *Processor) fetchMembershipObservation(ctx context.Context, session json.RawMessage, now time.Time) (*domain.MembershipObservation, error) {
	if p.accounts != nil {
		var claims map[string]any
		if json.Unmarshal(session, &claims) != nil || claims == nil {
			return nil, coded("SESSION_INVALID", "account Session must be an object")
		}
		observation, err := p.accounts.Observe(ctx, session)
		if err != nil {
			return nil, classifyAccountServiceError(err, "MEMBERSHIP_PROVIDER_UNAVAILABLE", "account service observation failed")
		}
		return normalizeAccountObservation(observation, sessionEmail(claims), now)
	}
	if p.membership == nil {
		return nil, coded("ACCOUNT_SERVICE_NOT_CONFIGURED", "membership reader is not configured")
	}
	raw, err := p.membership.Fetch(ctx, session)
	if err != nil {
		return nil, err
	}
	observation, err := domain.NormalizeMembershipEnvelope(raw, now)
	if err == nil {
		return observation, nil
	}
	code := domain.ErrorCode(err)
	if code == "" {
		code = "MEMBERSHIP_CONTRACT_UNKNOWN"
	}
	return nil, codedWrap(code, "normalize membership observation", err)
}

func normalizeAccountObservation(input AccountObservation, expectedEmail string, now time.Time) (*domain.MembershipObservation, error) {
	unknown := func(message string) (*domain.MembershipObservation, error) {
		return nil, coded("MEMBERSHIP_CONTRACT_UNKNOWN", message)
	}
	email := strings.TrimSpace(input.Email)
	if !validEmail(email) {
		return unknown("account service did not report a verified identity")
	}
	if expectedEmail = strings.TrimSpace(expectedEmail); expectedEmail != "" &&
		(!validEmail(expectedEmail) || !strings.EqualFold(email, expectedEmail)) {
		return nil, coded("CHATGPT_SESSION_IDENTITY_MISMATCH", "account observation does not match the supplied Session identity")
	}
	if input.Tier != domain.TierFree && !domain.IsMembershipTier(input.Tier) {
		return unknown("account service reported an unknown membership tier")
	}
	if input.AutoRenew == nil || input.IsOverdue == nil || input.IsDelinquent == nil {
		return unknown("account service did not establish renewal and debt status")
	}
	if now.IsZero() || input.ObservedAt.IsZero() || now.Sub(input.ObservedAt) > AccountObservationMaxAge ||
		input.ObservedAt.Sub(now) > AccountObservationMaxAge {
		return unknown("account service observation is missing or outside the freshness window")
	}
	currency := strings.ToUpper(strings.TrimSpace(input.Currency))
	if input.Tier != domain.TierFree {
		if len(currency) != 3 || strings.IndexFunc(currency, func(r rune) bool { return r < 'A' || r > 'Z' }) >= 0 ||
			input.ExpiresAt == nil || input.ExpiresAt.IsZero() {
			return unknown("paid account observation lacks currency or expiry evidence")
		}
	} else if *input.AutoRenew || *input.IsOverdue || *input.IsDelinquent ||
		(input.ExpiresAt != nil && input.ExpiresAt.After(now)) {
		return unknown("free account observation contradicts subscription evidence")
	}
	autoRenew := *input.AutoRenew
	result := &domain.MembershipObservation{
		// Zero denotes an in-process observation, not a fabricated HTTP status.
		ProviderCode: 0, ProviderAccountType: string(input.Tier), AccountType: input.Tier,
		Currency: currency, AutoRenew: &autoRenew, IsOverdue: *input.IsOverdue,
		IsDelinquent: *input.IsDelinquent, ObservedAt: input.ObservedAt.UTC(),
	}
	if input.ExpiresAt != nil && !input.ExpiresAt.IsZero() {
		expiresAt := input.ExpiresAt.UTC()
		result.ExpireTime = &expiresAt
		result.ExpireTimeValid = true
		result.ExpireTimeFuture = expiresAt.After(now)
	}
	return result, nil
}

func (p *Processor) fetchPostCancellationObservation(ctx context.Context, session json.RawMessage, previous *domain.MembershipObservation, cancelledAt, now time.Time) (*domain.MembershipObservation, error) {
	observation, err := p.fetchMembershipObservation(ctx, session, now)
	if err != nil || p.accounts == nil {
		return observation, err
	}
	notBefore := cancelledAt
	if previous != nil && previous.ObservedAt.After(notBefore) {
		notBefore = previous.ObservedAt
	}
	if observation.ObservedAt.Before(notBefore) {
		return nil, coded("MEMBERSHIP_CONTRACT_UNKNOWN", "post-cancellation observation predates cancellation or the previous observation")
	}
	return observation, nil
}

func (p *Processor) cancelAutoRenewal(ctx context.Context, session json.RawMessage) error {
	if p.accounts != nil {
		// An owned service supplies its own authorization. Do not read the legacy
		// extension_delivery_settings token even when cancellation is unsupported.
		return classifyAccountServiceError(p.accounts.CancelRenewal(ctx, session), "RENEWAL_CANCEL_FAILED", "account service cancellation failed")
	}
	if p.renewal == nil {
		return coded("RENEWAL_CANCEL_NOT_CONFIGURED", "renewal canceller is not configured")
	}
	token, err := p.renewalToken(ctx)
	if err != nil {
		return err
	}
	return p.renewal.Cancel(ctx, session, token)
}

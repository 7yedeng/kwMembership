package processor

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"kwmembership/internal/config"
	"kwmembership/internal/domain"
	"kwmembership/internal/provider"
	"kwmembership/internal/store"
)

type accountServiceStub struct {
	observe func(context.Context, json.RawMessage) (AccountObservation, error)
	cancel  func(context.Context, json.RawMessage) error
}

func (s *accountServiceStub) Observe(ctx context.Context, session json.RawMessage) (AccountObservation, error) {
	return s.observe(ctx, session)
}

func (s *accountServiceStub) CancelRenewal(ctx context.Context, session json.RawMessage) error {
	return s.cancel(ctx, session)
}

func accountBool(value bool) *bool { return &value }

func accountTestObservation(now time.Time) AccountObservation {
	expiresAt := now.Add(30 * 24 * time.Hour)
	return AccountObservation{
		Email: "buyer@example.com", Tier: domain.TierPlus, Currency: "PHP",
		AutoRenew: accountBool(false), IsOverdue: accountBool(false), IsDelinquent: accountBool(false),
		ExpiresAt: &expiresAt, ObservedAt: now,
	}
}

func TestAccountObservationRejectsUnknownOrContradictoryState(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		mutate func(*AccountObservation)
	}{
		{"missing identity", func(o *AccountObservation) { o.Email = "" }},
		{"unknown tier", func(o *AccountObservation) { o.Tier = "enterprise-new" }},
		{"absent tier", func(o *AccountObservation) { o.Tier = "" }},
		{"unknown renewal", func(o *AccountObservation) { o.AutoRenew = nil }},
		{"unknown overdue", func(o *AccountObservation) { o.IsOverdue = nil }},
		{"unknown delinquency", func(o *AccountObservation) { o.IsDelinquent = nil }},
		{"missing expiry", func(o *AccountObservation) { o.ExpiresAt = nil }},
		{"zero expiry", func(o *AccountObservation) { o.ExpiresAt = &time.Time{} }},
		{"missing currency", func(o *AccountObservation) { o.Currency = "" }},
		{"invalid currency", func(o *AccountObservation) { o.Currency = "P3P" }},
		{"missing timestamp", func(o *AccountObservation) { o.ObservedAt = time.Time{} }},
		{"stale timestamp", func(o *AccountObservation) { o.ObservedAt = now.Add(-AccountObservationMaxAge - time.Second) }},
		{"future timestamp", func(o *AccountObservation) { o.ObservedAt = now.Add(AccountObservationMaxAge + time.Second) }},
		{"free with future expiry", func(o *AccountObservation) { o.Tier = domain.TierFree }},
		{"free with renewal", func(o *AccountObservation) {
			o.Tier = domain.TierFree
			o.ExpiresAt = nil
			o.AutoRenew = accountBool(true)
		}},
		{"free with debt", func(o *AccountObservation) {
			o.Tier = domain.TierFree
			o.ExpiresAt = nil
			o.IsDelinquent = accountBool(true)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := accountTestObservation(now)
			tc.mutate(&input)
			result, err := normalizeAccountObservation(input, "buyer@example.com", now)
			if result != nil || errorCode(err) != "MEMBERSHIP_CONTRACT_UNKNOWN" {
				t.Fatalf("unsafe account observation accepted: result=%v error=%v", result, err)
			}
		})
	}
}

func TestAccountObservationDoesNotInferFreeFromExpiredPaidTier(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	input := accountTestObservation(now)
	expired := now.Add(-time.Hour)
	input.ExpiresAt = &expired
	observation, err := normalizeAccountObservation(input, "BUYER@example.com", now)
	if err != nil {
		t.Fatal(err)
	}
	classification, err := domain.ClassifyStartingMembership(observation)
	if err != nil || observation.AccountType != domain.TierPlus || classification != domain.StartingMembershipUnknown {
		t.Fatalf("expired paid tier inferred as eligible: classification=%v error=%v", classification, err)
	}
	input.Tier, input.Currency, input.ExpiresAt = domain.TierFree, "", nil
	observation, err = normalizeAccountObservation(input, "buyer@example.com", now)
	if err != nil {
		t.Fatal(err)
	}
	classification, err = domain.ClassifyStartingMembership(observation)
	if err != nil || classification != domain.StartingMembershipFree {
		t.Fatalf("explicit free evidence not accepted: classification=%v error=%v", classification, err)
	}
}

func TestOwnedAccountServiceNeverFallsBackOnFailure(t *testing.T) {
	wantErr := coded("MEMBERSHIP_PROVIDER_UNAVAILABLE", "owned service unavailable")
	observeCalls, cancelCalls := 0, 0
	service := &accountServiceStub{
		observe: func(context.Context, json.RawMessage) (AccountObservation, error) {
			observeCalls++
			return AccountObservation{}, wantErr
		},
		cancel: func(context.Context, json.RawMessage) error {
			cancelCalls++
			return ErrAccountRenewalUnsupported
		},
	}
	// The nil store/decrypter also prove cancellation cannot read an old token.
	p, err := NewWithAccountService(config.Config{}, nil, store.Lease{}, nil, nil, service)
	if err != nil {
		t.Fatal(err)
	}
	if p.membership != nil || p.renewal != nil || !p.requireLeaseFence {
		t.Fatal("owned constructor retained a legacy account client or disabled fencing")
	}
	session := json.RawMessage(`{"access_token":"synthetic-test-session"}`)
	if _, err := p.fetchMembershipObservation(context.Background(), session, time.Now()); !errors.Is(err, wantErr) {
		t.Fatalf("owned observation failure replaced: %v", err)
	}
	if err := p.cancelAutoRenewal(context.Background(), session); !errors.Is(err, ErrAccountRenewalUnsupported) {
		t.Fatalf("unsupported cancellation was not propagated: %v", err)
	}
	if observeCalls != 1 || cancelCalls != 1 {
		t.Fatalf("service calls: observe=%d cancel=%d", observeCalls, cancelCalls)
	}
	if _, err := NewWithAccountService(config.Config{}, nil, store.Lease{}, nil, nil, nil); errorCode(err) != "ACCOUNT_SERVICE_NOT_CONFIGURED" {
		t.Fatalf("missing account service accepted: %v", err)
	}
}

func TestOwnedEligibilityCancelsWithoutLegacyTokenAndReobserves(t *testing.T) {
	p, repository, fulfillment, now := newEligibilityDependencyProcessor(t, func(*http.Request) (*http.Response, error) {
		t.Error("owned account mode attempted a legacy HTTP request")
		return nil, errors.New("unexpected legacy request")
	})
	if _, err := repository.DB().Exec(`DROP TABLE extension_delivery_settings`); err != nil {
		t.Fatal(err)
	}
	observeCalls, cancelCalls := 0, 0
	p.accounts = &accountServiceStub{
		observe: func(context.Context, json.RawMessage) (AccountObservation, error) {
			observeCalls++
			observation := accountTestObservation(now)
			observation.AutoRenew = accountBool(observeCalls == 1)
			return observation, nil
		},
		cancel: func(context.Context, json.RawMessage) error { cancelCalls++; return nil },
	}
	if err := p.processEligibility(context.Background(), fulfillment, now); err != nil {
		t.Fatal(err)
	}
	var state string
	var observations int
	if err := repository.DB().QueryRow(`SELECT state FROM membership_fulfillments WHERE id='mf-1'`).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if err := repository.DB().QueryRow(`SELECT COUNT(*) FROM membership_observations WHERE fulfillment_id='mf-1'`).Scan(&observations); err != nil {
		t.Fatal(err)
	}
	if observeCalls != 2 || cancelCalls != 1 || state != "ACCOUNT_ALREADY_SUBSCRIBED" || observations != 2 {
		t.Fatalf("owned eligibility: observe=%d cancel=%d state=%s persisted=%d", observeCalls, cancelCalls, state, observations)
	}
}

func TestOwnedUnclassifiedFailureDoesNotCloseDependencyProbe(t *testing.T) {
	p, repository, fulfillment, now := newEligibilityDependencyProcessor(t, func(*http.Request) (*http.Response, error) {
		return nil, errors.New("unexpected legacy request")
	})
	if _, err := repository.DB().Exec(`INSERT INTO fulfillment_dependency_circuits
	  (id,dependency,scope_key,state,failure_count,opened_at,retry_at,recovery_revision,reason_code,updated_at)
	  VALUES ('owned-probe','membership_provider','default','open',3,?,?,0,'MEMBERSHIP_PROVIDER_UNAVAILABLE',?)`,
		store.ISO(now.Add(-time.Hour)), store.ISO(now.Add(-time.Second)), store.ISO(now.Add(-time.Hour))); err != nil {
		t.Fatal(err)
	}
	upstreamErr := errors.New("upstream unavailable with sensitive-test-marker")
	p.accounts = &accountServiceStub{observe: func(context.Context, json.RawMessage) (AccountObservation, error) {
		return AccountObservation{}, upstreamErr
	}}
	err := p.processEligibility(context.Background(), fulfillment, now)
	if errorCode(err) != "MEMBERSHIP_PROVIDER_UNAVAILABLE" || !errors.Is(err, upstreamErr) || strings.Contains(err.Error(), "sensitive-test-marker") {
		t.Fatalf("unclassified service failure was not classified and sanitized: %v", err)
	}
	assertEligibilityFailure(t, repository, "ACCOUNT_CHECKING", "MEMBERSHIP_PROVIDER_UNAVAILABLE")
	var state string
	if err := repository.DB().QueryRow(`SELECT state FROM fulfillment_dependency_circuits WHERE id='owned-probe'`).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "open" {
		t.Fatalf("failed owned-service probe was accepted as success: state=%s", state)
	}
}

func TestOwnedEligibilityPersistsUnknownInsteadOfFree(t *testing.T) {
	p, repository, fulfillment, now := newEligibilityDependencyProcessor(t, func(*http.Request) (*http.Response, error) {
		return nil, errors.New("unexpected legacy request")
	})
	p.accounts = &accountServiceStub{observe: func(context.Context, json.RawMessage) (AccountObservation, error) {
		return AccountObservation{Email: "buyer@example.com", Tier: domain.TierFree, ObservedAt: now}, nil
	}}
	if err := p.processEligibility(context.Background(), fulfillment, now); errorCode(err) != "MEMBERSHIP_CONTRACT_UNKNOWN" {
		t.Fatalf("unknown service facts did not stop eligibility: %v", err)
	}
	assertEligibilityFailure(t, repository, "MEMBERSHIP_CONTRACT_UNKNOWN", "MEMBERSHIP_CONTRACT_UNKNOWN")
}

func TestOwnedIdentityMismatchIsAnOrderFailureNotAGlobalCircuit(t *testing.T) {
	p, repository, fulfillment, now := newEligibilityDependencyProcessor(t, func(*http.Request) (*http.Response, error) {
		return nil, errors.New("unexpected legacy request")
	})
	session, err := p.decrypter.Encrypt(`{"access_token":"synthetic-session","user":{"email":"another@example.com"}}`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.DB().Exec(`UPDATE redeem_orders SET session_payload=? WHERE id='order-1'`, session); err != nil {
		t.Fatal(err)
	}
	p.accounts = &accountServiceStub{observe: func(context.Context, json.RawMessage) (AccountObservation, error) {
		return accountTestObservation(now), nil
	}}
	if err := p.processEligibility(context.Background(), fulfillment, now); errorCode(err) != "CHATGPT_SESSION_IDENTITY_MISMATCH" {
		t.Fatalf("identity mismatch did not retain session failure code: %v", err)
	}
	assertEligibilityTerminal(t, repository, "CANCELLED", "SESSION_INVALID_PRE_BOUNDARY")
	var circuits int
	if err := repository.DB().QueryRow(`SELECT COUNT(*) FROM fulfillment_dependency_circuits WHERE state='open'`).Scan(&circuits); err != nil {
		t.Fatal(err)
	}
	if circuits != 0 {
		t.Fatal("one Session identity mismatch opened a global account dependency circuit")
	}
}

func TestOwnedPostCancellationRejectsCachedObservation(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name        string
		observedAt  time.Time
		previousAt  time.Time
		cancelledAt time.Time
		wantError   bool
	}{
		{"predates cancel", now.Add(-time.Second), now.Add(-time.Minute), now, true},
		{"predates previous", now, now.Add(time.Second), now, true},
		{"current", now.Add(time.Second), now, now, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &Processor{accounts: &accountServiceStub{observe: func(context.Context, json.RawMessage) (AccountObservation, error) {
				observation := accountTestObservation(now)
				observation.ObservedAt = tc.observedAt
				return observation, nil
			}}}
			previous := &domain.MembershipObservation{ObservedAt: tc.previousAt}
			observation, err := p.fetchPostCancellationObservation(context.Background(), json.RawMessage(`{}`), previous, tc.cancelledAt, now)
			if tc.wantError {
				if observation != nil || errorCode(err) != "MEMBERSHIP_CONTRACT_UNKNOWN" {
					t.Fatalf("cached observation accepted after cancellation: result=%v err=%v", observation, err)
				}
			} else if err != nil || observation == nil {
				t.Fatalf("current observation rejected: %v", err)
			}
		})
	}
}

func TestOwnedReconciliationRequiresObservationAfterCancellation(t *testing.T) {
	p, repository, fulfillment, now := newEligibilityDependencyProcessor(t, func(*http.Request) (*http.Response, error) {
		return nil, errors.New("unexpected legacy request")
	})
	if _, err := repository.DB().Exec(`DROP TABLE extension_delivery_settings;
	  UPDATE membership_fulfillments SET state='FINAL_TIER_CONFIRMED' WHERE id='mf-1'`); err != nil {
		t.Fatal(err)
	}
	fulfillment.State = "FINAL_TIER_CONFIRMED"
	observeCalls, cancelCalls := 0, 0
	p.accounts = &accountServiceStub{
		observe: func(context.Context, json.RawMessage) (AccountObservation, error) {
			observeCalls++
			observation := accountTestObservation(now)
			observation.AutoRenew = accountBool(true)
			if observeCalls > 1 {
				observation.AutoRenew = nil
			}
			return observation, nil
		},
		cancel: func(context.Context, json.RawMessage) error { cancelCalls++; return nil },
	}
	if err := p.rcProtectRenewal(context.Background(), fulfillment, now); errorCode(err) != "MEMBERSHIP_CONTRACT_UNKNOWN" {
		t.Fatalf("cancellation acknowledgement accepted as membership truth: %v", err)
	}
	var state string
	if err := repository.DB().QueryRow(`SELECT state FROM membership_fulfillments WHERE id='mf-1'`).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if observeCalls != 2 || cancelCalls != 1 || state != "FINAL_TIER_CONFIRMED" {
		t.Fatalf("unconfirmed cancellation: observe=%d cancel=%d state=%s", observeCalls, cancelCalls, state)
	}
}

type billingServiceStub struct{}

func (*billingServiceStub) Cardholder(context.Context) (provider.Cardholder, error) {
	return provider.Cardholder{FirstName: "Test", LastName: "User"}, nil
}

func (*billingServiceStub) Billing(context.Context) (provider.BillingAddress, error) {
	return provider.BillingAddress{Name: "Test User"}, nil
}

func TestNewWithServicesRequiresExplicitDependencies(t *testing.T) {
	accounts, billing := &accountServiceStub{}, &billingServiceStub{}
	p, err := NewWithServices(config.Config{}, nil, store.Lease{}, nil, nil, Services{Accounts: accounts, Billing: billing})
	if err != nil {
		t.Fatal(err)
	}
	if p.accounts != accounts || p.address != billing || p.membership != nil || p.renewal != nil || !p.requireLeaseFence {
		t.Fatal("owned dependencies were not selected with legacy fallback disabled and fencing enabled")
	}
	if _, err := NewWithServices(config.Config{}, nil, store.Lease{}, nil, nil, Services{Accounts: accounts}); errorCode(err) != "BILLING_SERVICE_NOT_CONFIGURED" {
		t.Fatalf("missing billing dependency accepted: %v", err)
	}
	if _, err := NewWithServices(config.Config{}, nil, store.Lease{}, nil, nil, Services{Billing: billing}); errorCode(err) != "ACCOUNT_SERVICE_NOT_CONFIGURED" {
		t.Fatalf("missing account dependency accepted: %v", err)
	}
}

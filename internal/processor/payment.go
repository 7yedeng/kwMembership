package processor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"kwmembership/internal/domain"
	"kwmembership/internal/provider"
	"kwmembership/internal/store"
)

const (
	paymentAdapterVersion = "python-session-card-checkout-v1"
	paymentCardPageSize   = 100
	paymentMaxCardPages   = 100
	paymentRetryDelay     = 5 * time.Minute
)

var paymentRunnableStates = map[string]bool{
	"FUNDING_READY":                 true,
	"FUNDING":                       true,
	"PLATFORM_BALANCE_INSUFFICIENT": true,
}

type paymentError struct {
	code      string
	message   string
	retryable bool
}

func (e *paymentError) Error() string { return e.message }
func (e *paymentError) Code() string  { return e.code }

func paymentFailure(code, message string, retryable ...bool) error {
	return &paymentError{code: code, message: message, retryable: len(retryable) > 0 && retryable[0]}
}

type paymentGate struct {
	enabled bool
	mode    string
}

// FundingRecoveryOptions is deliberately separate from the normal payment gate.
// Recovery is never scheduled by RunOnce and must be enabled for each explicit call.
type FundingRecoveryOptions struct {
	Enabled                bool
	AllowOrphanedSubmitted bool
}

type paymentReservation struct {
	ID                 string
	FulfillmentID      string
	ProviderKey        string
	CardID             sql.NullString
	PlannedProductCode sql.NullString
	TargetLane         string
	SlotIndex          sql.NullInt64
	State              string
	ReservedAt         string
}

type paymentCard struct {
	ID                    string
	ProviderKey           string
	UpstreamCardID        int64
	VMCardID              string
	ProductCode           string
	UpstreamStatus        string
	CachedAvailableAmount float64
	Lane                  sql.NullString
	ConsumedSlots         int
	CapacityState         string
	ReconciliationState   string
}

type paymentProduct struct {
	Code               string
	OpenEnabled        bool
	OpenFeeCents       int64
	OpenFeeRate        float64
	RechargeFeeRate    float64
	MinimumAmountCents int64
	MaximumAmountCents int64
}

type paymentFundingFacts struct {
	PlatformBalanceCents int64
	Products             map[string]paymentProduct
}

type paymentStageDefinition struct {
	StageKey     string
	ExpectedTier domain.Tier
	SignalTier   domain.Tier
}

type paymentSnapshotStage struct {
	paymentStageDefinition
	AmountCents    int64
	MinimumCents   int64
	MaximumCents   int64
	ProviderTime   string
	ContractID     string
	AdapterVersion string
}

type paymentPriceSnapshot struct {
	TargetTier domain.Tier
	TotalCents int64
	Stages     []paymentSnapshotStage
}

type paymentFundingPlan struct {
	Kind                      string
	Operation                 string
	FullOrderBudgetCents      int64
	AvailableAmountCents      int64
	FundingAmountCents        int64
	FeeCents                  int64
	PlatformDebitCents        int64
	PlatformBalanceCents      int64
	PlatformBalanceSufficient bool
}

type paymentSelection struct {
	ProviderKey string
	Kind        string
	Reservation paymentReservation
	Card        *paymentCard
	Product     *paymentProduct
	Snapshot    paymentPriceSnapshot
	Plan        paymentFundingPlan
	Live        *provider.Card
}

type paymentPriceContract struct {
	ID      string
	Tier    string
	Version int
}

type paymentAutomaticScope struct {
	ID              string
	SiteID          string
	ProductID       string
	Tier            string
	AdapterVersion  string
	PriceContractID string
}

type paymentRequestBody struct {
	Operation   string
	CardID      int64
	AmountCents int64
	ProductCode string
	FirstName   string
	LastName    string
}

type paymentFundingRequest struct {
	Operation      string
	TargetCardID   string
	ProductCode    string
	AmountCents    int64
	FeeCents       int64
	CanonicalBody  string
	RequestBody    paymentRequestBody
	IdempotencyKey string
	Fingerprint    string
}

type paymentFundingIntent struct {
	ID                   string
	FulfillmentID        string
	ProviderKey          string
	Operation            string
	TargetCardID         sql.NullString
	ProductCode          sql.NullString
	AmountCents          int64
	FeeCents             int64
	IdempotencyKey       string
	RequestFingerprint   string
	RequestBodyEncrypted string
	State                string
	ProviderResourceID   sql.NullString
	CreatedAt            string
	SubmittedAt          sql.NullString
	ResolvedAt           sql.NullString
}

func (p *Processor) tickPayment(ctx context.Context) (bool, error) {
	now := p.now().UTC()
	rows, err := p.store.DB().QueryContext(ctx, `
    SELECT id FROM membership_fulfillments
    WHERE state IN ('FUNDING_READY','FUNDING','PLATFORM_BALANCE_INSUFFICIENT')
      AND (retry_at IS NULL OR retry_at <= ?)
    ORDER BY created_at,id LIMIT 50`, store.ISO(now))
	if err != nil {
		return false, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return false, err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return false, err
	}
	for _, id := range ids {
		fulfillment, err := loadFulfillment(ctx, p.store.DB(), id)
		if err != nil {
			return false, err
		}
		gate, err := p.paymentResolveGate(ctx, fulfillment)
		if err != nil {
			return false, err
		}
		if !gate.enabled {
			continue
		}
		return true, p.paymentProcessFulfillment(ctx, fulfillment, gate, now)
	}
	return false, nil
}

func (p *Processor) paymentResolveGate(ctx context.Context, fulfillment Fulfillment) (paymentGate, error) {
	var enabled int
	var mode string
	if err := p.store.DB().QueryRowContext(ctx, `
    SELECT enabled,rollout_mode FROM membership_fulfillment_settings WHERE id='default'`).Scan(&enabled, &mode); err != nil {
		return paymentGate{}, err
	}
	if !fulfillment.RunMode.Valid || (fulfillment.RunMode.String != "canary" && fulfillment.RunMode.String != "automatic") {
		return paymentGate{}, nil
	}
	mode = strings.TrimSpace(mode)
	return paymentGate{
		enabled: enabled == 1 && mode == fulfillment.RunMode.String,
		mode:    fulfillment.RunMode.String,
	}, nil
}

func (p *Processor) paymentProcessFulfillment(ctx context.Context, fulfillment Fulfillment, gate paymentGate, now time.Time) error {
	if !gate.enabled || !paymentRunnableStates[fulfillment.State] {
		return nil
	}
	intent, err := p.paymentLoadFundingIntent(ctx, fulfillment.ID)
	if err != nil {
		return err
	}
	if intent != nil {
		switch intent.State {
		case "submitted", "outcome_unknown":
			if err := p.paymentMarkStages(ctx, fulfillment.ID, "funding_unknown", "", now); err != nil {
				return err
			}
			return p.paymentTransitionFailure(ctx, fulfillment.ID, "FUNDING_OUTCOME_UNKNOWN", "FUNDING_OUTCOME_UNKNOWN", now, false)
		case "failed":
			if err := p.paymentMarkStages(ctx, fulfillment.ID, "funding_failed", "", now); err != nil {
				return err
			}
			return p.paymentTransitionFailure(ctx, fulfillment.ID, "CHECKOUT_PRE_SUBMIT_FAILED", "FUNDING_PROVIDER_REJECTED", now, false)
		case "succeeded", "prepared":
			reservation, found, err := p.paymentExistingReservation(ctx, fulfillment.ID)
			if err != nil {
				return err
			}
			if !found {
				return paymentFailure("PAYMENT_STAGE_SNAPSHOT_CONFLICT", "persisted funding intent is missing its card reservation")
			}
			snapshot, found, err := p.paymentPersistedSnapshot(ctx, fulfillment, reservation)
			if err != nil {
				return err
			}
			if !found {
				return paymentFailure("PAYMENT_STAGE_SNAPSHOT_CONFLICT", "persisted funding intent is missing its payment stages")
			}
			selection := paymentSelection{ProviderKey: intent.ProviderKey, Reservation: reservation, Snapshot: snapshot, Plan: paymentFundingPlan{Operation: intent.Operation, FeeCents: intent.FeeCents}}
			if intent.State == "succeeded" {
				return p.paymentFinalize(ctx, fulfillment, selection, now)
			}
			if err := p.paymentAcquirePlatformCircuit(ctx, intent.ProviderKey, now); err != nil {
				return p.paymentHandleError(ctx, fulfillment.ID, err, now)
			}
			if _, err := p.transition(ctx, fulfillment.ID, "FUNDING", now, transitionOptions{CurrentStage: pointer("plus")}); err != nil {
				return err
			}
			client, err := p.cardPlatform(ctx, intent.ProviderKey)
			if err != nil {
				p.paymentRecordPlatformFailure(ctx, intent.ProviderKey, err, now)
			} else {
				err = p.paymentSubmitFundingIntent(ctx, client, fulfillment.ID, gate, now)
			}
			if err != nil {
				return p.paymentHandleError(ctx, fulfillment.ID, err, now)
			}
			return p.paymentFinalize(ctx, fulfillment, selection, now)
		default:
			return paymentFailure("FUNDING_INTENT_STORAGE_INVALID", "funding intent has an invalid state")
		}
	}

	// Reload after provider I/O so all durable selection decisions use current state.
	fulfillment, err = loadFulfillment(ctx, p.store.DB(), fulfillment.ID)
	if err != nil {
		return err
	}
	contracts, err := p.paymentActivePriceContracts(ctx, domain.Tier(fulfillment.TargetTier))
	if err != nil {
		return p.paymentHandleError(ctx, fulfillment.ID, err, now)
	}
	if !regexp.MustCompile(`^[A-Za-z0-9._:-]{1,200}$`).MatchString(paymentAdapterVersion) {
		return paymentFailure("PAYMENT_ADAPTER_VERSION_INVALID", "payment adapter version is invalid")
	}
	var scope *paymentAutomaticScope
	if fulfillment.RunMode.Valid && fulfillment.RunMode.String == "automatic" {
		scope, err = p.paymentResolveAutomaticScope(ctx, fulfillment, contracts)
		if err != nil {
			return p.paymentHandleError(ctx, fulfillment.ID, err, now)
		}
	}
	selection, client, err := p.paymentSelectPlatform(ctx, fulfillment, now)
	if err != nil {
		return p.paymentHandleError(ctx, fulfillment.ID, err, now)
	}
	if err := p.paymentEnsureStageSnapshots(ctx, fulfillment, selection, contracts, paymentAdapterVersion, now); err != nil {
		return p.paymentHandleError(ctx, fulfillment.ID, err, now)
	}
	if !selection.Plan.PlatformBalanceSufficient {
		return p.paymentTransitionFailure(ctx, fulfillment.ID, "PLATFORM_BALANCE_INSUFFICIENT", "PLATFORM_BALANCE_INSUFFICIENT", now, true)
	}

	request, err := p.paymentBuildFundingRequest(ctx, fulfillment, selection)
	if err != nil {
		return p.paymentHandleError(ctx, fulfillment.ID, err, now)
	}
	if scope != nil {
		if err := p.paymentReserveAutomaticQuota(ctx, fulfillment.ID, *scope, paymentAdapterVersion,
			selection.Snapshot.TotalCents, selection.Plan.FeeCents, now); err != nil {
			return p.paymentHandleError(ctx, fulfillment.ID, err, now)
		}
	}
	if request == nil {
		return p.paymentFinalize(ctx, fulfillment, selection, now)
	}
	if _, err := p.paymentPrepareFundingIntent(ctx, fulfillment, *request, now); err != nil {
		return p.paymentHandleError(ctx, fulfillment.ID, err, now)
	}
	if _, err := p.transition(ctx, fulfillment.ID, "FUNDING", now, transitionOptions{CurrentStage: pointer("plus")}); err != nil {
		return err
	}
	if err := p.paymentSubmitFundingIntent(ctx, client, fulfillment.ID, gate, now); err != nil {
		return p.paymentHandleError(ctx, fulfillment.ID, err, now)
	}
	return p.paymentFinalize(ctx, fulfillment, selection, now)
}

func (p *Processor) paymentAcquireSpaceXCircuit(ctx context.Context, now time.Time) error {
	return p.paymentAcquirePlatformCircuit(ctx, provider.CardPlatformSpaceX, now)
}

func (p *Processor) paymentAcquirePlatformCircuit(ctx context.Context, platformKey string, now time.Time) error {
	dependency, scope := cardPlatformCircuit(platformKey)
	allowed, err := p.acquireCircuit(ctx, dependency, scope, now)
	if err != nil {
		return err
	}
	if !allowed {
		code := "CARD_PLATFORM_CIRCUIT_OPEN"
		if platformKey == provider.CardPlatformSpaceX {
			code = "SPACEXCARD_CIRCUIT_OPEN"
		}
		return paymentFailure(code, "card platform dependency circuit is open", true)
	}
	return nil
}

func paymentIsSpaceXFailure(err error) bool {
	code := errorCode(err)
	return strings.HasPrefix(code, "SPACEXCARD_") || strings.HasPrefix(code, "PAYMENT_PROVIDER_")
}

func paymentIsPlatformFailure(err error) bool {
	code := errorCode(err)
	return strings.HasPrefix(code, "SPACEXCARD_") || strings.HasPrefix(code, "EFUNCARD_") ||
		strings.HasPrefix(code, "CARD_PLATFORM_") || strings.HasPrefix(code, "PAYMENT_PROVIDER_")
}

func (p *Processor) paymentRecordSpaceXFailure(ctx context.Context, cause error, now time.Time) {
	p.paymentRecordPlatformFailure(ctx, provider.CardPlatformSpaceX, cause, now)
}

func (p *Processor) paymentRecordPlatformFailure(ctx context.Context, platformKey string, cause error, now time.Time) {
	// Circuit bookkeeping must never replace the fulfillment's original durable transition.
	dependency, scope := cardPlatformCircuit(platformKey)
	_ = p.recordCircuitFailure(ctx, dependency, scope, cause, now)
}

func (p *Processor) paymentRecordSpaceXSuccess(ctx context.Context, now time.Time) {
	p.paymentRecordPlatformSuccess(ctx, provider.CardPlatformSpaceX, now)
}

func (p *Processor) paymentRecordPlatformSuccess(ctx context.Context, platformKey string, now time.Time) {
	// Circuit bookkeeping must never replace the fulfillment's original durable transition.
	dependency, scope := cardPlatformCircuit(platformKey)
	_ = p.recordCircuitSuccess(ctx, dependency, scope, now)
}

func (p *Processor) paymentSelectPlatform(ctx context.Context, fulfillment Fulfillment, now time.Time) (paymentSelection, provider.CardPlatform, error) {
	keys, err := p.enabledCardPlatformKeys(ctx)
	if err != nil {
		return paymentSelection{}, nil, err
	}
	if reservation, found, err := p.paymentExistingReservation(ctx, fulfillment.ID); err != nil {
		return paymentSelection{}, nil, err
	} else if found {
		keys = []string{reservation.ProviderKey}
	}
	if len(keys) == 0 {
		return paymentSelection{}, nil, paymentFailure("CARD_PLATFORM_NOT_CONFIGURED", "no enabled card platform is configured", true)
	}
	var lastUnavailable error
	for _, key := range keys {
		if err := p.paymentAcquirePlatformCircuit(ctx, key, now); err != nil {
			lastUnavailable = err
			continue
		}
		client, err := p.cardPlatform(ctx, key)
		if err != nil {
			p.paymentRecordPlatformFailure(ctx, key, err, now)
			lastUnavailable = err
			continue
		}
		facts, err := p.paymentLoadFundingFacts(ctx, client)
		if err != nil {
			p.paymentRecordPlatformFailure(ctx, key, err, now)
			lastUnavailable = err
			continue
		}
		selection, err := p.paymentChooseAndReserve(ctx, client, fulfillment, facts, now)
		if err == nil {
			p.paymentRecordPlatformSuccess(ctx, key, now)
			return selection, client, nil
		}
		if paymentIsPlatformFailure(err) {
			p.paymentRecordPlatformFailure(ctx, key, err, now)
		}
		if errorCode(err) == "CARD_PRICE_UNAVAILABLE" || errorCode(err) == "PAYMENT_RESERVED_CARD_UNSUPPORTED" {
			lastUnavailable = err
			continue
		}
		return paymentSelection{}, nil, err
	}
	if lastUnavailable != nil {
		return paymentSelection{}, nil, lastUnavailable
	}
	return paymentSelection{}, nil, paymentFailure("CARD_PRICE_UNAVAILABLE", "no enabled card platform can satisfy this order", true)
}

func (p *Processor) paymentHandleError(ctx context.Context, fulfillmentID string, cause error, now time.Time) error {
	code := errorCode(cause)
	switch code {
	case "FUNDING_OUTCOME_UNKNOWN", "FUNDING_RECOVERY_REQUIRED":
		if err := p.paymentMarkStages(ctx, fulfillmentID, "funding_unknown", "", now); err != nil {
			return err
		}
		return p.paymentTransitionFailure(ctx, fulfillmentID, "FUNDING_OUTCOME_UNKNOWN", code, now, false)
	case "FUNDING_PROVIDER_REJECTED":
		if err := p.paymentMarkStages(ctx, fulfillmentID, "funding_failed", "", now); err != nil {
			return err
		}
		return p.paymentTransitionFailure(ctx, fulfillmentID, "CHECKOUT_PRE_SUBMIT_FAILED", code, now, false)
	case "FUNDING_SUBMISSION_IN_PROGRESS", "FUNDING_INTENT_CONFLICT":
		return nil
	}
	priceUnavailable := code == "CARD_PRICE_UNAVAILABLE" || code == "PAYMENT_PRICE_CONTRACT_MISSING" || code == "PAYMENT_PRICE_CONTRACT_AMBIGUOUS"
	automatic := strings.HasPrefix(code, "AUTOMATIC_")
	providerRetry := strings.HasPrefix(code, "SPACEXCARD_") || strings.HasPrefix(code, "EFUNCARD_") ||
		strings.HasPrefix(code, "CARD_PLATFORM_") || paymentIsRetryable(cause)
	if priceUnavailable || automatic || providerRetry {
		next := "FUNDING_READY"
		if priceUnavailable {
			next = "CARD_PRICE_UNAVAILABLE"
		}
		return p.paymentTransitionFailure(ctx, fulfillmentID, next, code, now, true)
	}
	return cause
}

func paymentIsRetryable(err error) bool {
	var local *paymentError
	if errors.As(err, &local) {
		return local.retryable
	}
	var remote *provider.Error
	if errors.As(err, &remote) {
		return remote.Retryable
	}
	var rule *domain.CodedError
	return errors.As(err, &rule) && rule.Retryable
}

func (p *Processor) paymentTransitionFailure(ctx context.Context, id, state, code string, now time.Time, retry bool) error {
	var retryAt *string
	if retry {
		value := store.ISO(now.Add(paymentRetryDelay))
		retryAt = &value
	}
	_, err := p.transition(ctx, id, state, now, transitionOptions{
		CurrentStage: pointer("plus"), FailureCode: pointer(code), RetryAt: retryAt,
	})
	return err
}

func (p *Processor) paymentMarkStages(ctx context.Context, fulfillmentID, state, cardID string, now time.Time) error {
	var card any
	if cardID != "" {
		card = cardID
	}
	_, err := p.fencedExec(ctx, `
    UPDATE membership_payment_stages
    SET state=?,card_id=COALESCE(card_id,?),updated_at=?
    WHERE fulfillment_id=? AND state IN ('funding_pending','funding_unknown','funding_failed')`,
		state, card, store.ISO(now), fulfillmentID)
	return err
}

func (p *Processor) paymentFinalize(ctx context.Context, fulfillment Fulfillment, selection paymentSelection, now time.Time) error {
	reservation, found, err := p.paymentExistingReservation(ctx, fulfillment.ID)
	if err != nil {
		return err
	}
	if !found || !reservation.CardID.Valid || reservation.CardID.String == "" {
		return paymentFailure("PAYMENT_CARD_NOT_ATTACHED", "funding succeeded but the card reservation is not attached")
	}
	if err := p.paymentMarkStages(ctx, fulfillment.ID, "checkout_pending", reservation.CardID.String, now); err != nil {
		return err
	}
	_, err = p.transition(ctx, fulfillment.ID, "CHECKOUT_EXECUTION_WAIT", now, transitionOptions{
		CurrentStage: pointer("plus"),
	})
	return err
}

func paymentRequiredStages(target domain.Tier) ([]paymentStageDefinition, error) {
	if !domain.IsMembershipTier(target) {
		return nil, paymentFailure("TARGET_TIER_INVALID", "target membership tier is invalid")
	}
	stages := []paymentStageDefinition{{StageKey: "plus", ExpectedTier: domain.TierPlus, SignalTier: domain.TierPlus}}
	if target != domain.TierPlus {
		stages = append(stages, paymentStageDefinition{StageKey: "upgrade", ExpectedTier: target, SignalTier: target})
	}
	return stages, nil
}

func paymentRoundedCents(value float64, positive bool) (int64, error) {
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || (positive && value <= 0) {
		return 0, domain.ErrInvalidMoney
	}
	return domain.CentsFromUSD(value)
}

func paymentExactCents(value float64, positive bool) (int64, error) {
	cents, err := paymentRoundedCents(value, positive)
	if err != nil || math.Abs(value*100-float64(cents)) > 1e-7 {
		return 0, domain.ErrInvalidMoney
	}
	return cents, nil
}

func paymentAddCents(left, right int64) (int64, error) {
	if right > 0 && left > math.MaxInt64-right {
		return 0, domain.ErrInvalidMoney
	}
	if right < 0 && left < math.MinInt64-right {
		return 0, domain.ErrInvalidMoney
	}
	return left + right, nil
}

func (p *Processor) paymentLoadFundingFacts(ctx context.Context, client provider.CardPlatform) (paymentFundingFacts, error) {
	balance, err := client.GetBalance(ctx)
	if err != nil {
		return paymentFundingFacts{}, err
	}
	if err := p.assertWorkAllowed(ctx); err != nil {
		return paymentFundingFacts{}, err
	}
	if strings.ToUpper(strings.TrimSpace(balance.Currency)) != "USD" {
		return paymentFundingFacts{}, paymentFailure("PAYMENT_PROVIDER_BALANCE_INVALID", "card platform balance is not normalized to USD", true)
	}
	balanceCents, err := paymentRoundedCents(balance.Balance, false)
	if err != nil {
		return paymentFundingFacts{}, paymentFailure("PAYMENT_PROVIDER_BALANCE_INVALID", "card platform balance is invalid", true)
	}
	products, err := client.ListProducts(ctx)
	if err != nil {
		return paymentFundingFacts{}, err
	}
	if err := p.assertWorkAllowed(ctx); err != nil {
		return paymentFundingFacts{}, err
	}
	catalog := make(map[string]paymentProduct, len(products))
	for _, product := range products {
		code := strings.TrimSpace(product.ProductCode)
		openFee, openErr := paymentRoundedCents(product.OpenFee, false)
		minimum, minErr := paymentRoundedCents(product.MinAmount, false)
		maximum, maxErr := paymentRoundedCents(product.MaxAmount, true)
		if code == "" || catalog[code].Code != "" || openErr != nil || minErr != nil || maxErr != nil || maximum < minimum ||
			math.IsNaN(product.OpenFeeRate) || math.IsInf(product.OpenFeeRate, 0) || product.OpenFeeRate < 0 || product.OpenFeeRate > 1 ||
			math.IsNaN(product.RechargeFee) || math.IsInf(product.RechargeFee, 0) || product.RechargeFee < 0 || product.RechargeFee > 1 {
			return paymentFundingFacts{}, paymentFailure("PAYMENT_PROVIDER_PRODUCTS_INVALID", "card platform product catalog is invalid", true)
		}
		catalog[code] = paymentProduct{
			Code: code, OpenEnabled: product.OpenEnabled, OpenFeeCents: openFee, OpenFeeRate: product.OpenFeeRate, RechargeFeeRate: product.RechargeFee,
			MinimumAmountCents: minimum, MaximumAmountCents: maximum,
		}
	}
	return paymentFundingFacts{PlatformBalanceCents: balanceCents, Products: catalog}, nil
}

func (p *Processor) paymentLoadLiveCards(ctx context.Context, client provider.CardPlatform) (map[int64]provider.Card, error) {
	cards := map[int64]provider.Card{}
	expectedTotal := -1
	for page := 1; page <= paymentMaxCardPages; page++ {
		total, batch, err := client.ListCards(ctx, page, paymentCardPageSize, true)
		if err != nil {
			return nil, err
		}
		if err := p.assertWorkAllowed(ctx); err != nil {
			return nil, err
		}
		if total < 0 {
			return nil, paymentFailure("PAYMENT_PROVIDER_CARD_LIST_INVALID", "card platform list total is invalid", true)
		}
		if len(batch) > paymentCardPageSize {
			return nil, paymentFailure("PAYMENT_PROVIDER_CARD_LIST_INVALID", "card platform list page exceeds the requested size", true)
		}
		if expectedTotal < 0 {
			expectedTotal = total
		} else if total != expectedTotal {
			return nil, paymentFailure("PAYMENT_PROVIDER_CARD_LIST_INVALID", "card platform list total changed during pagination", true)
		}
		for _, card := range batch {
			if card.UpstreamCardID <= 0 {
				return nil, paymentFailure("PAYMENT_PROVIDER_CARD_LIST_INVALID", "card platform list contains an invalid card", true)
			}
			if _, duplicate := cards[card.UpstreamCardID]; duplicate {
				return nil, paymentFailure("PAYMENT_PROVIDER_CARD_LIST_INVALID", "card platform list contains a duplicate card", true)
			}
			cards[card.UpstreamCardID] = card
		}
		if len(cards) > expectedTotal {
			return nil, paymentFailure("PAYMENT_PROVIDER_CARD_LIST_INVALID", "card platform list exceeded its declared total", true)
		}
		if len(cards) == expectedTotal {
			return cards, nil
		}
		if len(batch) < paymentCardPageSize {
			return nil, paymentFailure("PAYMENT_PROVIDER_CARD_LIST_INVALID", "card platform list ended before its declared total", true)
		}
	}
	return nil, paymentFailure("PAYMENT_PROVIDER_CARD_LIST_EXCEEDED", "card platform list exceeded the safe page limit", true)
}

func paymentPlanNewCard(snapshot paymentPriceSnapshot, product paymentProduct, platformBalance int64) (paymentFundingPlan, error) {
	funding := snapshot.TotalCents
	if product.MinimumAmountCents > funding {
		funding = product.MinimumAmountCents
	}
	if funding > product.MaximumAmountCents {
		return paymentFundingPlan{}, paymentFailure("CARD_PRODUCT_AMOUNT_UNSUPPORTED", "card product cannot cover the full order budget")
	}
	variableFee, err := domain.CentsFromUSD(domain.USDFromCents(funding) * product.OpenFeeRate)
	if err != nil || variableFee < 0 {
		return paymentFundingPlan{}, paymentFailure("FUNDING_REQUEST_INVALID", "open-card fee is invalid")
	}
	fee, err := paymentAddCents(product.OpenFeeCents, variableFee)
	if err != nil {
		return paymentFundingPlan{}, paymentFailure("FUNDING_REQUEST_INVALID", "open-card fee overflows")
	}
	debit, err := paymentAddCents(funding, fee)
	if err != nil {
		return paymentFundingPlan{}, paymentFailure("FUNDING_REQUEST_INVALID", "funding plan overflows")
	}
	return paymentFundingPlan{
		Kind: "new_card", Operation: "open", FullOrderBudgetCents: snapshot.TotalCents,
		FundingAmountCents: funding, FeeCents: fee, PlatformDebitCents: debit,
		PlatformBalanceCents: platformBalance, PlatformBalanceSufficient: platformBalance >= debit,
	}, nil
}

func paymentPlanExistingCard(snapshot paymentPriceSnapshot, card paymentCard, live provider.Card, product paymentProduct, platformBalance int64) (paymentFundingPlan, error) {
	if strings.ToUpper(strings.TrimSpace(live.Status)) != "ACTIVE" {
		return paymentFundingPlan{}, paymentFailure("PAYMENT_RESERVED_CARD_UNSUPPORTED", "managed card is not active")
	}
	if live.UpstreamCardID != card.UpstreamCardID || strings.TrimSpace(live.ProductCode) != card.ProductCode {
		return paymentFundingPlan{}, paymentFailure("PAYMENT_PROVIDER_CARD_IDENTITY_MISMATCH", "live card does not match the managed card")
	}
	available, err := paymentRoundedCents(live.AvailableAmount, false)
	if err != nil {
		return paymentFundingPlan{}, paymentFailure("PAYMENT_PROVIDER_CARD_BALANCE_UNAVAILABLE", "live card balance is invalid", true)
	}
	shortfall := snapshot.TotalCents - available
	if shortfall < 0 {
		shortfall = 0
	}
	plan := paymentFundingPlan{
		Kind: "existing_card", FullOrderBudgetCents: snapshot.TotalCents, AvailableAmountCents: available,
		FundingAmountCents: shortfall, PlatformBalanceCents: platformBalance,
	}
	if shortfall == 0 {
		plan.Operation = "none"
		plan.PlatformBalanceSufficient = true
		return plan, nil
	}
	if shortfall < product.MinimumAmountCents {
		return paymentFundingPlan{}, paymentFailure("CARD_RECHARGE_MINIMUM_EXCEEDS_SHORTFALL", "minimum recharge exceeds the exact card shortfall")
	}
	if shortfall > product.MaximumAmountCents {
		return paymentFundingPlan{}, paymentFailure("CARD_PRODUCT_AMOUNT_UNSUPPORTED", "card product cannot cover the exact card shortfall")
	}
	fee, err := domain.CentsFromUSD(domain.USDFromCents(shortfall) * product.RechargeFeeRate)
	if err != nil || fee < 0 {
		return paymentFundingPlan{}, paymentFailure("FUNDING_REQUEST_INVALID", "recharge fee is invalid")
	}
	debit, err := paymentAddCents(shortfall, fee)
	if err != nil {
		return paymentFundingPlan{}, paymentFailure("FUNDING_REQUEST_INVALID", "funding plan overflows")
	}
	plan.Operation = "recharge"
	plan.FeeCents = fee
	plan.PlatformDebitCents = debit
	plan.PlatformBalanceSufficient = platformBalance >= debit
	return plan, nil
}

func (p *Processor) paymentLoadCard(ctx context.Context, query store.Execer, id string) (paymentCard, error) {
	var card paymentCard
	hasProvider, err := tableHasColumn(ctx, query, "managed_cards", "provider_key")
	if err != nil {
		return paymentCard{}, err
	}
	if hasProvider {
		err = query.QueryRowContext(ctx, `
	    SELECT id,provider_key,upstream_card_id,vm_card_id,product_code,upstream_status,cached_available_amount,
	      lane,consumed_slots,capacity_state,reconciliation_state
	    FROM managed_cards WHERE id=?`, id).Scan(
			&card.ID, &card.ProviderKey, &card.UpstreamCardID, &card.VMCardID, &card.ProductCode, &card.UpstreamStatus,
			&card.CachedAvailableAmount, &card.Lane, &card.ConsumedSlots, &card.CapacityState, &card.ReconciliationState,
		)
	} else {
		card.ProviderKey = provider.CardPlatformSpaceX
		err = query.QueryRowContext(ctx, `
    SELECT id,upstream_card_id,vm_card_id,product_code,upstream_status,cached_available_amount,
      lane,consumed_slots,capacity_state,reconciliation_state
    FROM managed_cards WHERE id=?`, id).Scan(
			&card.ID, &card.UpstreamCardID, &card.VMCardID, &card.ProductCode, &card.UpstreamStatus,
			&card.CachedAvailableAmount, &card.Lane, &card.ConsumedSlots, &card.CapacityState, &card.ReconciliationState,
		)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return paymentCard{}, paymentFailure("MEMBERSHIP_CARD_NOT_FOUND", "managed card does not exist")
	}
	return card, err
}

func paymentAssertCardReservable(card paymentCard, target domain.Tier) error {
	if card.UpstreamStatus != "ACTIVE" || card.ReconciliationState != "READY" || card.CapacityState == "HOLD" {
		return paymentFailure("MEMBERSHIP_CARD_NOT_READY", "managed card is not reservable", true)
	}
	if card.Lane.Valid && card.Lane.String != "" && card.Lane.String != string(target) {
		return paymentFailure("CARD_LANE_MISMATCH", "managed card belongs to another membership lane")
	}
	return nil
}

func (p *Processor) paymentCardHasCapacity(ctx context.Context, card paymentCard, target domain.Tier) (bool, error) {
	if err := paymentAssertCardReservable(card, target); err != nil {
		return false, nil
	}
	capacity, ok := domain.CapacityForTier(target)
	if !ok {
		return false, paymentFailure("TARGET_TIER_INVALID", "target membership tier is invalid")
	}
	var active int
	if err := p.store.DB().QueryRowContext(ctx, `
    SELECT COUNT(*) FROM card_capacity_reservations
    WHERE card_id=? AND target_lane=? AND state IN ('reserved','consumed','retained_partial')`,
		card.ID, string(target)).Scan(&active); err != nil {
		return false, err
	}
	if card.ConsumedSlots > active {
		active = card.ConsumedSlots
	}
	return active < capacity, nil
}

func (p *Processor) paymentLoadPriceSnapshot(ctx context.Context, cardID string, target domain.Tier, now time.Time) (paymentPriceSnapshot, error) {
	rows, err := p.store.DB().QueryContext(ctx, `
    SELECT tier,found,amount,min_usd,max_usd,provider_time
    FROM card_price_signals WHERE card_id=?`, cardID)
	if err != nil {
		return paymentPriceSnapshot{}, err
	}
	type signalRow struct {
		found            bool
		amount, min, max float64
		providerTime     sql.NullString
	}
	byTier := map[domain.Tier]signalRow{}
	var signals []domain.PriceSignal
	for rows.Next() {
		var tier string
		var found int
		var item signalRow
		if err := rows.Scan(&tier, &found, &item.amount, &item.min, &item.max, &item.providerTime); err != nil {
			rows.Close()
			return paymentPriceSnapshot{}, err
		}
		item.found = found == 1
		byTier[domain.Tier(tier)] = item
		signal := domain.PriceSignal{Tier: domain.Tier(tier), Found: item.found, AmountUSD: item.amount}
		if item.providerTime.Valid {
			signal.Time = item.providerTime.String
		}
		signals = append(signals, signal)
	}
	if err := rows.Close(); err != nil {
		return paymentPriceSnapshot{}, err
	}
	budget, err := domain.CalculateMembershipBudget(signals, target, now)
	if err != nil {
		code := domain.ErrorCode(err)
		if code == "" {
			code = "CARD_PRICE_UNAVAILABLE"
		}
		return paymentPriceSnapshot{}, paymentFailure(code, err.Error(), true)
	}
	definitions, err := paymentRequiredStages(target)
	if err != nil {
		return paymentPriceSnapshot{}, err
	}
	budgetByTier := make(map[domain.Tier]domain.BudgetStage, len(budget.Stages))
	for _, stage := range budget.Stages {
		budgetByTier[stage.Tier] = stage
	}
	snapshot := paymentPriceSnapshot{TargetTier: target, TotalCents: budget.TotalCents, Stages: make([]paymentSnapshotStage, 0, len(definitions))}
	for _, definition := range definitions {
		row, exists := byTier[definition.SignalTier]
		amount, amountErr := paymentRoundedCents(row.amount, true)
		minimum, minErr := paymentRoundedCents(row.min, false)
		maximum, maxErr := paymentRoundedCents(row.max, false)
		budgetStage, budgetFound := budgetByTier[definition.SignalTier]
		if !exists || !row.found || !budgetFound || amountErr != nil || minErr != nil || maxErr != nil || maximum < minimum || amount != budgetStage.PriceCents {
			return paymentPriceSnapshot{}, paymentFailure("CARD_PRICE_UNAVAILABLE", "card is missing a complete fresh price snapshot", true)
		}
		snapshot.Stages = append(snapshot.Stages, paymentSnapshotStage{
			paymentStageDefinition: definition, AmountCents: amount, MinimumCents: minimum, MaximumCents: maximum,
			ProviderTime: store.ISO(budgetStage.ProviderTime),
		})
	}
	return snapshot, nil
}

func (p *Processor) paymentLoadProvenProductSnapshot(ctx context.Context, productCode string, target domain.Tier, now time.Time) (paymentPriceSnapshot, error) {
	return p.paymentLoadProvenProductSnapshotForPlatform(ctx, provider.CardPlatformSpaceX, productCode, target, now)
}

func (p *Processor) paymentLoadProvenProductSnapshotForPlatform(ctx context.Context, platformKey, productCode string, target domain.Tier, now time.Time) (paymentPriceSnapshot, error) {
	hasProvider, err := tableHasColumn(ctx, p.store.DB(), "managed_cards", "provider_key")
	if err != nil {
		return paymentPriceSnapshot{}, err
	}
	var rows *sql.Rows
	if hasProvider {
		rows, err = p.store.DB().QueryContext(ctx, `
	    SELECT c.id FROM managed_cards c
	    WHERE c.provider_key=? AND c.product_code=? AND `+productPriceEvidenceCardWhereSQL+`
	    ORDER BY c.id`, platformKey, productCode)
	} else {
		rows, err = p.store.DB().QueryContext(ctx, `
	    SELECT c.id FROM managed_cards c
	    WHERE c.product_code=? AND `+productPriceEvidenceCardWhereSQL+`
	    ORDER BY c.id`, productCode)
	}
	if err != nil {
		return paymentPriceSnapshot{}, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return paymentPriceSnapshot{}, err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return paymentPriceSnapshot{}, err
	}
	for _, id := range ids {
		snapshot, err := p.paymentLoadPriceSnapshot(ctx, id, target, now)
		if err == nil {
			return snapshot, nil
		}
		if errorCode(err) != "CARD_PRICE_UNAVAILABLE" {
			return paymentPriceSnapshot{}, err
		}
	}
	return paymentPriceSnapshot{}, paymentFailure("CARD_PRICE_UNAVAILABLE", "new-card product lacks fresh same-product price evidence", true)
}

func (p *Processor) paymentPersistedSnapshot(ctx context.Context, fulfillment Fulfillment, reservation paymentReservation) (paymentPriceSnapshot, bool, error) {
	rows, err := p.store.DB().QueryContext(ctx, `
    SELECT stage_key,expected_tier,card_id,price_signal_amount,price_signal_min,price_signal_max,
      price_signal_time,adapter_version,price_contract_id
    FROM membership_payment_stages WHERE fulfillment_id=? ORDER BY stage_key`, fulfillment.ID)
	if err != nil {
		return paymentPriceSnapshot{}, false, err
	}
	type persisted struct {
		stageKey, expectedTier string
		cardID                 sql.NullString
		amount, min, max       sql.NullFloat64
		providerTime           sql.NullString
		adapterVersion         sql.NullString
		contractID             sql.NullString
	}
	var items []persisted
	for rows.Next() {
		var item persisted
		if err := rows.Scan(&item.stageKey, &item.expectedTier, &item.cardID, &item.amount, &item.min, &item.max,
			&item.providerTime, &item.adapterVersion, &item.contractID); err != nil {
			rows.Close()
			return paymentPriceSnapshot{}, false, err
		}
		items = append(items, item)
	}
	if err := rows.Close(); err != nil {
		return paymentPriceSnapshot{}, false, err
	}
	if len(items) == 0 {
		return paymentPriceSnapshot{}, false, nil
	}
	target := domain.Tier(fulfillment.TargetTier)
	definitions, err := paymentRequiredStages(target)
	if err != nil {
		return paymentPriceSnapshot{}, false, err
	}
	if len(items) != len(definitions) {
		return paymentPriceSnapshot{}, false, paymentFailure("PAYMENT_STAGE_SNAPSHOT_CONFLICT", "persisted payment-stage snapshot is incomplete")
	}
	byKey := make(map[string]persisted, len(items))
	for _, item := range items {
		byKey[item.stageKey] = item
	}
	snapshot := paymentPriceSnapshot{TargetTier: target, Stages: make([]paymentSnapshotStage, 0, len(definitions))}
	for _, definition := range definitions {
		row, found := byKey[definition.StageKey]
		amount, amountErr := paymentRoundedCents(row.amount.Float64, true)
		minimum, minErr := paymentRoundedCents(row.min.Float64, false)
		maximum, maxErr := paymentRoundedCents(row.max.Float64, false)
		parsedTime, timeErr := time.Parse(time.RFC3339Nano, row.providerTime.String)
		if !found || row.expectedTier != string(definition.ExpectedTier) || !row.amount.Valid || !row.min.Valid || !row.max.Valid ||
			!row.providerTime.Valid || !row.adapterVersion.Valid || !row.contractID.Valid || row.adapterVersion.String == "" || row.contractID.String == "" ||
			amountErr != nil || minErr != nil || maxErr != nil || maximum < minimum || timeErr != nil ||
			(row.cardID.Valid && reservation.CardID.Valid && row.cardID.String != reservation.CardID.String) {
			return paymentPriceSnapshot{}, false, paymentFailure("PAYMENT_STAGE_SNAPSHOT_CONFLICT", "persisted payment-stage snapshot conflicts with its reservation")
		}
		snapshot.Stages = append(snapshot.Stages, paymentSnapshotStage{
			paymentStageDefinition: definition, AmountCents: amount, MinimumCents: minimum, MaximumCents: maximum,
			ProviderTime: store.ISO(parsedTime), ContractID: row.contractID.String, AdapterVersion: row.adapterVersion.String,
		})
		stageBudget, addErr := paymentAddCents(amount, domain.MembershipStageAllowanceCents)
		if addErr != nil {
			return paymentPriceSnapshot{}, false, paymentFailure("PAYMENT_STAGE_SNAPSHOT_CONFLICT", "payment-stage budget overflows")
		}
		snapshot.TotalCents, addErr = paymentAddCents(snapshot.TotalCents, stageBudget)
		if addErr != nil {
			return paymentPriceSnapshot{}, false, paymentFailure("PAYMENT_STAGE_SNAPSHOT_CONFLICT", "payment-stage budget overflows")
		}
	}
	return snapshot, true, nil
}

func (p *Processor) paymentActivePriceContracts(ctx context.Context, target domain.Tier) (map[string]paymentPriceContract, error) {
	definitions, err := paymentRequiredStages(target)
	if err != nil {
		return nil, err
	}
	contracts := make(map[string]paymentPriceContract, len(definitions))
	for _, definition := range definitions {
		rows, err := p.store.DB().QueryContext(ctx, `
      SELECT id,tier,version FROM checkout_price_contracts
      WHERE tier=? AND currency='PHP' AND status='active'
      ORDER BY version DESC,id`, string(definition.ExpectedTier))
		if err != nil {
			return nil, err
		}
		var matches []paymentPriceContract
		for rows.Next() {
			var contract paymentPriceContract
			if err := rows.Scan(&contract.ID, &contract.Tier, &contract.Version); err != nil {
				rows.Close()
				return nil, err
			}
			matches = append(matches, contract)
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
		if len(matches) != 1 {
			code := "PAYMENT_PRICE_CONTRACT_MISSING"
			if len(matches) > 1 {
				code = "PAYMENT_PRICE_CONTRACT_AMBIGUOUS"
			}
			return nil, paymentFailure(code, "each payment stage requires exactly one active PHP price contract", true)
		}
		contracts[definition.StageKey] = matches[0]
	}
	return contracts, nil
}

func (p *Processor) paymentResolveAutomaticScope(ctx context.Context, fulfillment Fulfillment, contracts map[string]paymentPriceContract) (*paymentAutomaticScope, error) {
	var siteID, productID sql.NullString
	if err := p.store.DB().QueryRowContext(ctx, `SELECT site_id,product_id FROM redeem_orders WHERE id=?`, fulfillment.OrderID).
		Scan(&siteID, &productID); err != nil {
		return nil, err
	}
	if !siteID.Valid || !productID.Valid || strings.TrimSpace(siteID.String) == "" || strings.TrimSpace(productID.String) == "" {
		return nil, paymentFailure("AUTOMATIC_SCOPE_NOT_FOUND", "automatic order lacks an exact site/product binding")
	}
	rows, err := p.store.DB().QueryContext(ctx, `
    SELECT id,site_id,product_id,tier,adapter_version,price_contract_id
    FROM automatic_checkout_scopes
    WHERE site_id=? AND product_id=? AND tier=? AND status='active'
    ORDER BY revision DESC`, siteID.String, productID.String, fulfillment.TargetTier)
	if err != nil {
		return nil, err
	}
	var scopes []paymentAutomaticScope
	for rows.Next() {
		var scope paymentAutomaticScope
		if err := rows.Scan(&scope.ID, &scope.SiteID, &scope.ProductID, &scope.Tier, &scope.AdapterVersion, &scope.PriceContractID); err != nil {
			rows.Close()
			return nil, err
		}
		scopes = append(scopes, scope)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if len(scopes) != 1 {
		return nil, paymentFailure("AUTOMATIC_SCOPE_NOT_FOUND", "automatic checkout requires exactly one active exact scope")
	}
	finalStage := "upgrade"
	if fulfillment.TargetTier == "plus" {
		finalStage = "plus"
	}
	scope := scopes[0]
	if scope.AdapterVersion != paymentAdapterVersion || scope.PriceContractID != contracts[finalStage].ID {
		return nil, paymentFailure("AUTOMATIC_SCOPE_VERSION_STALE", "automatic scope does not match the current adapter and price contract")
	}
	return &scope, nil
}

func (p *Processor) paymentExistingReservation(ctx context.Context, fulfillmentID string) (paymentReservation, bool, error) {
	return paymentLoadReservationWith(ctx, p.store.DB(), fulfillmentID)
}

func (p *Processor) paymentChooseAndReserve(ctx context.Context, client provider.CardPlatform, fulfillment Fulfillment, facts paymentFundingFacts, now time.Time) (paymentSelection, error) {
	platformKey := client.Key()
	liveCards, err := p.paymentLoadLiveCards(ctx, client)
	if err != nil {
		return paymentSelection{}, err
	}
	if existing, found, err := p.paymentExistingReservation(ctx, fulfillment.ID); err != nil {
		return paymentSelection{}, err
	} else if found {
		return p.paymentSelectionFromReservation(ctx, fulfillment, existing, liveCards, facts, now)
	}

	target := domain.Tier(fulfillment.TargetTier)
	rows, err := p.store.DB().QueryContext(ctx, `
    SELECT id,upstream_card_id,vm_card_id,product_code,upstream_status,cached_available_amount,
      lane,consumed_slots,capacity_state,reconciliation_state
    FROM managed_cards
	    WHERE provider_key=? AND upstream_status='ACTIVE' AND reconciliation_state='READY'
      AND capacity_state<>'HOLD' AND (lane IS NULL OR lane=?)
	    ORDER BY id`, platformKey, fulfillment.TargetTier)
	if err != nil {
		return paymentSelection{}, err
	}
	type existingCandidate struct {
		card     paymentCard
		snapshot paymentPriceSnapshot
		plan     paymentFundingPlan
		live     provider.Card
	}
	var cards []paymentCard
	for rows.Next() {
		var card paymentCard
		card.ProviderKey = platformKey
		if err := rows.Scan(&card.ID, &card.UpstreamCardID, &card.VMCardID, &card.ProductCode, &card.UpstreamStatus,
			&card.CachedAvailableAmount, &card.Lane, &card.ConsumedSlots, &card.CapacityState, &card.ReconciliationState); err != nil {
			rows.Close()
			return paymentSelection{}, err
		}
		cards = append(cards, card)
	}
	if err := rows.Close(); err != nil {
		return paymentSelection{}, err
	}
	candidates := map[string]existingCandidate{}
	rankInputs := make([]domain.CardCandidate, 0, len(cards))
	for _, card := range cards {
		hasCapacity, err := p.paymentCardHasCapacity(ctx, card, target)
		if err != nil {
			return paymentSelection{}, err
		}
		if !hasCapacity {
			continue
		}
		snapshot, err := p.paymentLoadPriceSnapshot(ctx, card.ID, target, now)
		if err != nil {
			if errorCode(err) == "CARD_PRICE_UNAVAILABLE" {
				continue
			}
			return paymentSelection{}, err
		}
		live, found := liveCards[card.UpstreamCardID]
		if !found {
			continue
		}
		product, found := facts.Products[card.ProductCode]
		if !found {
			continue
		}
		plan, err := paymentPlanExistingCard(snapshot, card, live, product, facts.PlatformBalanceCents)
		if err != nil {
			if code := errorCode(err); code == "CARD_RECHARGE_MINIMUM_EXCEEDS_SHORTFALL" || code == "CARD_PRODUCT_AMOUNT_UNSUPPORTED" || code == "PAYMENT_RESERVED_CARD_UNSUPPORTED" {
				continue
			}
			return paymentSelection{}, err
		}
		lane := domain.Tier("")
		if card.Lane.Valid {
			lane = domain.Tier(card.Lane.String)
		}
		candidates[card.ID] = existingCandidate{card: card, snapshot: snapshot, plan: plan, live: live}
		rankInputs = append(rankInputs, domain.CardCandidate{
			ID: card.ID, Eligible: true, Lane: lane, BudgetCents: snapshot.TotalCents,
			AvailableAmountCents: plan.AvailableAmountCents,
		})
	}
	ranked, err := domain.RankMembershipCardCandidates(rankInputs, target)
	if err != nil {
		return paymentSelection{}, err
	}
	for _, rankedCard := range ranked {
		candidate := candidates[rankedCard.ID]
		reservation, err := p.paymentReserveExistingCard(ctx, fulfillment.ID, candidate.card.ProviderKey, candidate.card.ID, target, now)
		if err != nil {
			if won, found, loadErr := p.paymentExistingReservation(ctx, fulfillment.ID); loadErr != nil {
				return paymentSelection{}, loadErr
			} else if found {
				return p.paymentSelectionFromReservation(ctx, fulfillment, won, liveCards, facts, now)
			}
			code := errorCode(err)
			if code == "CARD_CAPACITY_FULL" || code == "CARD_RESERVATION_BUSY" || code == "CARD_RESERVATION_CONFLICT" {
				continue
			}
			return paymentSelection{}, err
		}
		_, err = p.fencedExec(ctx, `
      UPDATE managed_cards SET cached_available_amount=?,upstream_status='ACTIVE',last_balance_sync_at=?,updated_at=?
      WHERE id=?`, domain.USDFromCents(candidate.plan.AvailableAmountCents), store.ISO(now), store.ISO(now), candidate.card.ID)
		if err != nil {
			return paymentSelection{}, err
		}
		card, live := candidate.card, candidate.live
		return paymentSelection{ProviderKey: platformKey, Kind: "existing_card", Reservation: reservation, Card: &card,
			Snapshot: candidate.snapshot, Plan: candidate.plan, Live: &live}, nil
	}

	productRows, err := p.store.DB().QueryContext(ctx, `
	    SELECT product_code FROM card_product_policies WHERE provider_key=? AND enabled=1 ORDER BY product_code`, platformKey)
	if err != nil {
		return paymentSelection{}, err
	}
	var productCodes []string
	for productRows.Next() {
		var code string
		if err := productRows.Scan(&code); err != nil {
			productRows.Close()
			return paymentSelection{}, err
		}
		productCodes = append(productCodes, code)
	}
	if err := productRows.Close(); err != nil {
		return paymentSelection{}, err
	}
	type newCandidate struct {
		product  paymentProduct
		snapshot paymentPriceSnapshot
		plan     paymentFundingPlan
	}
	var newCards []newCandidate
	for _, code := range productCodes {
		product, found := facts.Products[code]
		if !found || !product.OpenEnabled {
			continue
		}
		snapshot, err := p.paymentLoadProvenProductSnapshotForPlatform(ctx, platformKey, code, target, now)
		if err != nil {
			if errorCode(err) == "CARD_PRICE_UNAVAILABLE" {
				continue
			}
			return paymentSelection{}, err
		}
		plan, err := paymentPlanNewCard(snapshot, product, facts.PlatformBalanceCents)
		if err != nil {
			if errorCode(err) == "CARD_PRODUCT_AMOUNT_UNSUPPORTED" {
				continue
			}
			return paymentSelection{}, err
		}
		newCards = append(newCards, newCandidate{product: product, snapshot: snapshot, plan: plan})
	}
	sort.SliceStable(newCards, func(i, j int) bool {
		if newCards[i].plan.PlatformDebitCents != newCards[j].plan.PlatformDebitCents {
			return newCards[i].plan.PlatformDebitCents < newCards[j].plan.PlatformDebitCents
		}
		return newCards[i].product.Code < newCards[j].product.Code
	})
	if len(newCards) == 0 {
		return paymentSelection{}, paymentFailure("CARD_PRICE_UNAVAILABLE", "no existing card or enabled new-card product is eligible", true)
	}
	selected := newCards[0]
	reservation, err := p.paymentReserveNewCard(ctx, fulfillment.ID, platformKey, selected.product.Code, target, now)
	if err != nil {
		return paymentSelection{}, err
	}
	product := selected.product
	return paymentSelection{ProviderKey: platformKey, Kind: "new_card", Reservation: reservation, Product: &product,
		Snapshot: selected.snapshot, Plan: selected.plan}, nil
}

func (p *Processor) paymentSelectionFromReservation(ctx context.Context, fulfillment Fulfillment, reservation paymentReservation,
	liveCards map[int64]provider.Card, facts paymentFundingFacts, now time.Time) (paymentSelection, error) {
	platformKey := reservation.ProviderKey
	if platformKey == "" {
		platformKey = provider.CardPlatformSpaceX
	}
	if reservation.State != "reserved" {
		return paymentSelection{}, paymentFailure("PAYMENT_CARD_RESERVATION_INACTIVE", "payment reservation is no longer active")
	}
	target := domain.Tier(fulfillment.TargetTier)
	if reservation.TargetLane != fulfillment.TargetTier {
		return paymentSelection{}, paymentFailure("CARD_RESERVATION_TIER_MISMATCH", "payment reservation tier does not match fulfillment")
	}
	if reservation.CardID.Valid {
		card, err := p.paymentLoadCard(ctx, p.store.DB(), reservation.CardID.String)
		if err != nil {
			return paymentSelection{}, err
		}
		if err := paymentAssertCardReservable(card, target); err != nil {
			return paymentSelection{}, paymentFailure("PAYMENT_CARD_RESERVATION_STALE", "reserved card is no longer usable", true)
		}
		snapshot, found, err := p.paymentPersistedSnapshot(ctx, fulfillment, reservation)
		if err != nil {
			return paymentSelection{}, err
		}
		if !found {
			snapshot, err = p.paymentLoadPriceSnapshot(ctx, card.ID, target, now)
			if err != nil {
				return paymentSelection{}, err
			}
		}
		live, found := liveCards[card.UpstreamCardID]
		if !found {
			return paymentSelection{}, paymentFailure("PAYMENT_RESERVED_CARD_UNSUPPORTED", "reserved card is absent from the live provider list")
		}
		product, found := facts.Products[card.ProductCode]
		if !found {
			return paymentSelection{}, paymentFailure("PAYMENT_RESERVED_CARD_UNSUPPORTED", "reserved card product is unavailable")
		}
		plan, err := paymentPlanExistingCard(snapshot, card, live, product, facts.PlatformBalanceCents)
		if err != nil {
			code := errorCode(err)
			if code == "CARD_RECHARGE_MINIMUM_EXCEEDS_SHORTFALL" || code == "CARD_PRODUCT_AMOUNT_UNSUPPORTED" || code == "PAYMENT_RESERVED_CARD_UNSUPPORTED" {
				return paymentSelection{}, paymentFailure("PAYMENT_RESERVED_CARD_UNSUPPORTED", "reserved card no longer satisfies its funding plan")
			}
			return paymentSelection{}, err
		}
		return paymentSelection{ProviderKey: platformKey, Kind: "existing_card", Reservation: reservation, Card: &card,
			Snapshot: snapshot, Plan: plan, Live: &live}, nil
	}
	if !reservation.PlannedProductCode.Valid || reservation.PlannedProductCode.String == "" {
		return paymentSelection{}, paymentFailure("NEW_CARD_PLAN_REQUIRED", "new-card reservation lacks its product plan")
	}
	snapshot, found, err := p.paymentPersistedSnapshot(ctx, fulfillment, reservation)
	if err != nil {
		return paymentSelection{}, err
	}
	if !found {
		snapshot, err = p.paymentLoadProvenProductSnapshotForPlatform(ctx, platformKey, reservation.PlannedProductCode.String, target, now)
		if err != nil {
			return paymentSelection{}, err
		}
	}
	product, found := facts.Products[reservation.PlannedProductCode.String]
	if !found {
		return paymentSelection{}, paymentFailure("PAYMENT_CARD_PRODUCT_UNAVAILABLE", "reserved new-card product is unavailable", true)
	}
	plan, err := paymentPlanNewCard(snapshot, product, facts.PlatformBalanceCents)
	if err != nil {
		return paymentSelection{}, err
	}
	return paymentSelection{ProviderKey: platformKey, Kind: "new_card", Reservation: reservation, Product: &product, Snapshot: snapshot, Plan: plan}, nil
}

func paymentScanReservation(row scanner, providerAware bool) (paymentReservation, error) {
	var reservation paymentReservation
	var err error
	if providerAware {
		err = row.Scan(&reservation.ID, &reservation.FulfillmentID, &reservation.ProviderKey, &reservation.CardID, &reservation.PlannedProductCode,
			&reservation.TargetLane, &reservation.SlotIndex, &reservation.State, &reservation.ReservedAt)
	} else {
		reservation.ProviderKey = provider.CardPlatformSpaceX
		err = row.Scan(&reservation.ID, &reservation.FulfillmentID, &reservation.CardID, &reservation.PlannedProductCode,
			&reservation.TargetLane, &reservation.SlotIndex, &reservation.State, &reservation.ReservedAt)
	}
	return reservation, err
}

func paymentLoadReservationWith(ctx context.Context, query store.Execer, fulfillmentID string) (paymentReservation, bool, error) {
	hasProvider, err := tableHasColumn(ctx, query, "card_capacity_reservations", "provider_key")
	if err != nil {
		return paymentReservation{}, false, err
	}
	if hasProvider {
		reservation, err := paymentScanReservation(query.QueryRowContext(ctx, `
	    SELECT id,fulfillment_id,provider_key,card_id,planned_product_code,target_lane,slot_index,state,reserved_at
	    FROM card_capacity_reservations WHERE fulfillment_id=?`, fulfillmentID), true)
		if errors.Is(err, sql.ErrNoRows) {
			return paymentReservation{}, false, nil
		}
		return reservation, err == nil, err
	}
	reservation, err := paymentScanReservation(query.QueryRowContext(ctx, `
    SELECT id,fulfillment_id,card_id,planned_product_code,target_lane,slot_index,state,reserved_at
	    FROM card_capacity_reservations WHERE fulfillment_id=?`, fulfillmentID), false)
	if errors.Is(err, sql.ErrNoRows) {
		return paymentReservation{}, false, nil
	}
	return reservation, err == nil, err
}

func paymentReservationDBError(err error) error {
	if err == nil {
		return nil
	}
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "database is locked") || strings.Contains(message, "database is busy") || strings.Contains(message, "sqlite_busy") || strings.Contains(message, "sqlite_locked") {
		return paymentFailure("CARD_RESERVATION_BUSY", "card reservation is being updated", true)
	}
	if strings.Contains(message, "constraint failed") || strings.Contains(message, "unique constraint") || strings.Contains(message, "sqlite_constraint") {
		return paymentFailure("CARD_RESERVATION_CONFLICT", "card reservation encountered a concurrent conflict", true)
	}
	return err
}

func paymentActiveSlots(ctx context.Context, tx *sql.Tx, card paymentCard, target domain.Tier) (map[int]bool, error) {
	occupied := map[int]bool{}
	for slot := 1; slot <= card.ConsumedSlots; slot++ {
		occupied[slot] = true
	}
	rows, err := tx.QueryContext(ctx, `
    SELECT slot_index FROM card_capacity_reservations
    WHERE card_id=? AND target_lane=? AND state IN ('reserved','consumed','retained_partial')`, card.ID, string(target))
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var slot sql.NullInt64
		if err := rows.Scan(&slot); err != nil {
			rows.Close()
			return nil, err
		}
		if slot.Valid && slot.Int64 > 0 && slot.Int64 <= math.MaxInt32 {
			occupied[int(slot.Int64)] = true
		}
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	return occupied, nil
}

func paymentFirstFreeSlot(ctx context.Context, tx *sql.Tx, card paymentCard, target domain.Tier) (int, error) {
	capacity, ok := domain.CapacityForTier(target)
	if !ok {
		return 0, paymentFailure("TARGET_TIER_INVALID", "target membership tier is invalid")
	}
	occupied, err := paymentActiveSlots(ctx, tx, card, target)
	if err != nil {
		return 0, err
	}
	for slot := 1; slot <= capacity; slot++ {
		if !occupied[slot] {
			return slot, nil
		}
	}
	return 0, paymentFailure("CARD_CAPACITY_FULL", "card membership capacity is full", true)
}

func paymentUpdateCapacityState(ctx context.Context, tx *sql.Tx, card paymentCard, target domain.Tier, now time.Time) error {
	if card.CapacityState == "HOLD" {
		return nil
	}
	capacity, ok := domain.CapacityForTier(target)
	if !ok {
		return paymentFailure("TARGET_TIER_INVALID", "target membership tier is invalid")
	}
	occupied, err := paymentActiveSlots(ctx, tx, card, target)
	if err != nil {
		return err
	}
	state := "AVAILABLE"
	if len(occupied) >= capacity {
		state = "CAPACITY_FULL"
	}
	_, err = tx.ExecContext(ctx, `UPDATE managed_cards SET capacity_state=?,updated_at=? WHERE id=?`, state, store.ISO(now), card.ID)
	return err
}

func paymentReservationMatches(existing paymentReservation, platformKey string, target domain.Tier, cardID, productCode string) bool {
	if existing.ProviderKey != platformKey || existing.TargetLane != string(target) {
		return false
	}
	if cardID != "" {
		return existing.CardID.Valid && existing.CardID.String == cardID
	}
	if productCode != "" {
		return existing.PlannedProductCode.Valid && existing.PlannedProductCode.String == productCode
	}
	return false
}

func (p *Processor) paymentReserveExistingCard(ctx context.Context, fulfillmentID, platformKey, cardID string, target domain.Tier, now time.Time) (paymentReservation, error) {
	var reserved paymentReservation
	err := p.withFencedImmediate(ctx, func(tx *sql.Tx) error {
		var fulfillmentTier string
		var linked sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT target_tier,card_reservation_id FROM membership_fulfillments WHERE id=?`, fulfillmentID).
			Scan(&fulfillmentTier, &linked); errors.Is(err, sql.ErrNoRows) {
			return paymentFailure("MEMBERSHIP_FULFILLMENT_NOT_FOUND", "membership fulfillment does not exist")
		} else if err != nil {
			return err
		}
		if fulfillmentTier != string(target) {
			return paymentFailure("CARD_RESERVATION_TIER_MISMATCH", "card reservation tier does not match fulfillment")
		}
		if existing, found, err := paymentLoadReservationWith(ctx, tx, fulfillmentID); err != nil {
			return err
		} else if found {
			if existing.State == "released" {
				return paymentFailure("CARD_RESERVATION_RELEASED", "released card reservation cannot be replaced")
			}
			if !paymentReservationMatches(existing, platformKey, target, cardID, "") || (linked.Valid && linked.String != existing.ID) {
				return paymentFailure("CARD_RESERVATION_CONFLICT", "fulfillment cannot replace its card reservation", true)
			}
			if !linked.Valid {
				if _, err := tx.ExecContext(ctx, `UPDATE membership_fulfillments
          SET card_reservation_id=?,money_boundary_at=COALESCE(money_boundary_at,?),updated_at=?
          WHERE id=?`, existing.ID, store.ISO(now), store.ISO(now), fulfillmentID); err != nil {
					return err
				}
			} else if _, err := tx.ExecContext(ctx, `UPDATE membership_fulfillments
        SET money_boundary_at=COALESCE(money_boundary_at,?),updated_at=? WHERE id=?`,
				store.ISO(now), store.ISO(now), fulfillmentID); err != nil {
				return err
			}
			reserved = existing
			return nil
		}
		card, err := p.paymentLoadCard(ctx, tx, cardID)
		if err != nil {
			return err
		}
		if err := paymentAssertCardReservable(card, target); err != nil {
			return err
		}
		if card.ProviderKey != platformKey {
			return paymentFailure("CARD_RESERVATION_CONFLICT", "managed card belongs to another platform")
		}
		slot, err := paymentFirstFreeSlot(ctx, tx, card, target)
		if err != nil {
			return err
		}
		id, err := store.NewID("mcr_")
		if err != nil {
			return err
		}
		at := store.ISO(now)
		if !card.Lane.Valid || card.Lane.String == "" {
			if _, err := tx.ExecContext(ctx, `UPDATE managed_cards SET lane=?,updated_at=? WHERE id=?`, string(target), at, card.ID); err != nil {
				return err
			}
			card.Lane = sql.NullString{String: string(target), Valid: true}
		}
		if _, err := tx.ExecContext(ctx, `
      INSERT INTO card_capacity_reservations
	        (id,fulfillment_id,provider_key,card_id,planned_product_code,target_lane,slot_index,state,reserved_at)
	      VALUES (?,?,?, ?,NULL,?,?,'reserved',?)`, id, fulfillmentID, card.ProviderKey, card.ID, string(target), slot, at); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE membership_fulfillments
      SET card_reservation_id=?,money_boundary_at=COALESCE(money_boundary_at,?),updated_at=?
      WHERE id=?`, id, at, at, fulfillmentID); err != nil {
			return err
		}
		if err := paymentUpdateCapacityState(ctx, tx, card, target, now); err != nil {
			return err
		}
		reserved, err = paymentScanReservation(tx.QueryRowContext(ctx, `
	      SELECT id,fulfillment_id,provider_key,card_id,planned_product_code,target_lane,slot_index,state,reserved_at
	      FROM card_capacity_reservations WHERE id=?`, id), true)
		return err
	})
	return reserved, paymentReservationDBError(err)
}

func (p *Processor) paymentReserveNewCard(ctx context.Context, fulfillmentID, platformKey, productCode string, target domain.Tier, now time.Time) (paymentReservation, error) {
	var reserved paymentReservation
	err := p.withFencedImmediate(ctx, func(tx *sql.Tx) error {
		var fulfillmentTier string
		var linked sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT target_tier,card_reservation_id FROM membership_fulfillments WHERE id=?`, fulfillmentID).
			Scan(&fulfillmentTier, &linked); errors.Is(err, sql.ErrNoRows) {
			return paymentFailure("MEMBERSHIP_FULFILLMENT_NOT_FOUND", "membership fulfillment does not exist")
		} else if err != nil {
			return err
		}
		if fulfillmentTier != string(target) {
			return paymentFailure("CARD_RESERVATION_TIER_MISMATCH", "new-card reservation tier does not match fulfillment")
		}
		if existing, found, err := paymentLoadReservationWith(ctx, tx, fulfillmentID); err != nil {
			return err
		} else if found {
			if existing.State == "released" {
				return paymentFailure("CARD_RESERVATION_RELEASED", "released card reservation cannot be replaced")
			}
			if !paymentReservationMatches(existing, platformKey, target, "", productCode) || (linked.Valid && linked.String != existing.ID) {
				return paymentFailure("CARD_RESERVATION_CONFLICT", "fulfillment cannot replace its new-card plan", true)
			}
			if !linked.Valid {
				if _, err := tx.ExecContext(ctx, `UPDATE membership_fulfillments SET card_reservation_id=? WHERE id=?`, existing.ID, fulfillmentID); err != nil {
					return err
				}
			}
			reserved = existing
			return nil
		}
		id, err := store.NewID("mcr_")
		if err != nil {
			return err
		}
		at := store.ISO(now)
		if _, err := tx.ExecContext(ctx, `
      INSERT INTO card_capacity_reservations
	        (id,fulfillment_id,provider_key,card_id,planned_product_code,target_lane,slot_index,state,reserved_at)
	      VALUES (?,?,?,NULL,?,?,NULL,'reserved',?)`, id, fulfillmentID, platformKey, productCode, string(target), at); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE membership_fulfillments SET card_reservation_id=?,updated_at=? WHERE id=?`, id, at, fulfillmentID); err != nil {
			return err
		}
		reserved, err = paymentScanReservation(tx.QueryRowContext(ctx, `
	      SELECT id,fulfillment_id,provider_key,card_id,planned_product_code,target_lane,slot_index,state,reserved_at
	      FROM card_capacity_reservations WHERE id=?`, id), true)
		return err
	})
	return reserved, paymentReservationDBError(err)
}

func (p *Processor) paymentEnsureStageSnapshots(ctx context.Context, fulfillment Fulfillment, selection paymentSelection,
	contracts map[string]paymentPriceContract, adapterVersion string, now time.Time) error {
	var cardID any
	if selection.Reservation.CardID.Valid {
		cardID = selection.Reservation.CardID.String
	}
	return p.withFencedImmediate(ctx, func(tx *sql.Tx) error {
		at := store.ISO(now)
		for _, stage := range selection.Snapshot.Stages {
			contract, ok := contracts[stage.StageKey]
			if !ok || contract.ID == "" {
				return paymentFailure("PAYMENT_PRICE_CONTRACT_MISSING", "payment stage lacks an active PHP price contract", true)
			}
			type existingStage struct {
				expectedTier   string
				cardID         sql.NullString
				amount         sql.NullFloat64
				minimum        sql.NullFloat64
				maximum        sql.NullFloat64
				providerTime   sql.NullString
				adapterVersion sql.NullString
				contractID     sql.NullString
			}
			var existing existingStage
			err := tx.QueryRowContext(ctx, `
        SELECT expected_tier,card_id,price_signal_amount,price_signal_min,price_signal_max,
          price_signal_time,adapter_version,price_contract_id
        FROM membership_payment_stages WHERE fulfillment_id=? AND stage_key=?`,
				fulfillment.ID, stage.StageKey).Scan(&existing.expectedTier, &existing.cardID, &existing.amount,
				&existing.minimum, &existing.maximum, &existing.providerTime, &existing.adapterVersion, &existing.contractID)
			if err == nil {
				amount, amountErr := paymentRoundedCents(existing.amount.Float64, true)
				minimum, minErr := paymentRoundedCents(existing.minimum.Float64, false)
				maximum, maxErr := paymentRoundedCents(existing.maximum.Float64, false)
				parsed, timeErr := time.Parse(time.RFC3339Nano, existing.providerTime.String)
				cardConflict := existing.cardID.Valid && selection.Reservation.CardID.Valid && existing.cardID.String != selection.Reservation.CardID.String
				if existing.expectedTier != string(stage.ExpectedTier) || !existing.amount.Valid || !existing.minimum.Valid || !existing.maximum.Valid ||
					!existing.providerTime.Valid || !existing.adapterVersion.Valid || !existing.contractID.Valid ||
					amountErr != nil || minErr != nil || maxErr != nil || timeErr != nil || cardConflict ||
					amount != stage.AmountCents || minimum != stage.MinimumCents || maximum != stage.MaximumCents ||
					store.ISO(parsed) != stage.ProviderTime || existing.adapterVersion.String != adapterVersion || existing.contractID.String != contract.ID {
					return paymentFailure("PAYMENT_STAGE_SNAPSHOT_CONFLICT", "payment-stage snapshot is immutable")
				}
				continue
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			id, err := store.NewID("mps_")
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `
        INSERT INTO membership_payment_stages (
          id,fulfillment_id,stage_key,expected_tier,state,card_id,
          price_signal_amount,price_signal_min,price_signal_max,price_signal_time,
          adapter_version,price_contract_id,created_at,updated_at
        ) VALUES (?,?,?,?,'funding_pending',?,?,?,?,?,?,?,?,?)`, id, fulfillment.ID, stage.StageKey,
				string(stage.ExpectedTier), cardID, domain.USDFromCents(stage.AmountCents), domain.USDFromCents(stage.MinimumCents),
				domain.USDFromCents(stage.MaximumCents), stage.ProviderTime, adapterVersion, contract.ID, at, at); err != nil {
				return err
			}
		}
		return nil
	})
}

func paymentBusinessDate(now time.Time) string {
	// ponytail: China has used a fixed UTC+8 civil clock since 1991; avoid a runtime tzdata dependency.
	shanghai := time.FixedZone("Asia/Shanghai", 8*60*60)
	return now.In(shanghai).Format("2006-01-02")
}

func (p *Processor) paymentReserveAutomaticQuota(ctx context.Context, fulfillmentID string, requested paymentAutomaticScope,
	adapterVersion string, paymentBudgetCents, feeCents int64, now time.Time) error {
	riskCents, err := paymentAddCents(paymentBudgetCents, feeCents)
	if err != nil || paymentBudgetCents <= 0 || feeCents < 0 {
		return paymentFailure("AUTOMATIC_SCOPE_DAILY_RISK_LIMIT", "automatic checkout risk amount is invalid")
	}
	var logicalErr error
	err = p.withFencedImmediate(ctx, func(tx *sql.Tx) error {
		type scopeRow struct {
			ID, SiteID, ProductID, Tier, AdapterVersion, PriceContractID string
			Status, ActivatedAt, ScopeKey                                string
			DailyOrderLimit                                              int64
			DailyRiskLimit                                               float64
		}
		var scope scopeRow
		var activated sql.NullString
		err := tx.QueryRowContext(ctx, `
      SELECT id,site_id,product_id,tier,adapter_version,price_contract_id,status,activated_at,
        scope_key,daily_order_limit,daily_risk_limit_usd
      FROM automatic_checkout_scopes WHERE id=?`, requested.ID).Scan(
			&scope.ID, &scope.SiteID, &scope.ProductID, &scope.Tier, &scope.AdapterVersion, &scope.PriceContractID,
			&scope.Status, &activated, &scope.ScopeKey, &scope.DailyOrderLimit, &scope.DailyRiskLimit,
		)
		if errors.Is(err, sql.ErrNoRows) {
			logicalErr = paymentFailure("AUTOMATIC_SCOPE_MISMATCH", "automatic scope does not match the fulfillment")
			return nil
		}
		if err != nil {
			return err
		}
		if activated.Valid {
			scope.ActivatedAt = activated.String
		}
		type fulfillmentRow struct {
			SiteID, ProductID, TargetTier, RunMode, OrderCreated string
			MoneyBoundary                                        sql.NullString
		}
		var fulfillment fulfillmentRow
		var runMode sql.NullString
		if err := tx.QueryRowContext(ctx, `
      SELECT o.site_id,o.product_id,f.target_tier,f.run_mode,f.money_boundary_at,o.created_at
      FROM membership_fulfillments f JOIN redeem_orders o ON o.id=f.order_id WHERE f.id=?`, fulfillmentID).
			Scan(&fulfillment.SiteID, &fulfillment.ProductID, &fulfillment.TargetTier, &runMode,
				&fulfillment.MoneyBoundary, &fulfillment.OrderCreated); errors.Is(err, sql.ErrNoRows) {
			logicalErr = paymentFailure("AUTOMATIC_SCOPE_MISMATCH", "automatic scope does not match the fulfillment")
			return nil
		} else if err != nil {
			return err
		}
		if runMode.Valid {
			fulfillment.RunMode = runMode.String
		}
		if scope.SiteID != fulfillment.SiteID || scope.ProductID != fulfillment.ProductID || scope.Tier != fulfillment.TargetTier {
			logicalErr = paymentFailure("AUTOMATIC_SCOPE_MISMATCH", "automatic scope does not match the fulfillment")
			return nil
		}

		type quotaRow struct {
			ID, ScopeID, State string
			OrderUnits         int64
			RiskUSD            float64
		}
		var quota quotaRow
		quotaErr := tx.QueryRowContext(ctx, `
      SELECT id,scope_id,state,order_units,risk_reserved_usd
      FROM automatic_checkout_quota_reservations WHERE fulfillment_id=?`, fulfillmentID).
			Scan(&quota.ID, &quota.ScopeID, &quota.State, &quota.OrderUnits, &quota.RiskUSD)
		hasQuota := quotaErr == nil
		if quotaErr != nil && !errors.Is(quotaErr, sql.ErrNoRows) {
			return quotaErr
		}

		var activeContract sql.NullString
		activeErr := tx.QueryRowContext(ctx, `SELECT id FROM checkout_price_contracts WHERE tier=? AND status='active'`, scope.Tier).
			Scan(&activeContract)
		if activeErr != nil && !errors.Is(activeErr, sql.ErrNoRows) {
			return activeErr
		}
		versionStale := scope.AdapterVersion != adapterVersion || scope.PriceContractID != requested.PriceContractID ||
			!activeContract.Valid || activeContract.String != scope.PriceContractID
		if versionStale {
			if _, err := tx.ExecContext(ctx, `UPDATE automatic_checkout_scopes SET status='paused' WHERE id=? AND status='active'`, scope.ID); err != nil {
				return err
			}
		}

		if hasQuota {
			savedRisk, centsErr := paymentExactCents(quota.RiskUSD, false)
			if quota.ScopeID != scope.ID || centsErr != nil || savedRisk != riskCents || quota.OrderUnits != 1 {
				logicalErr = paymentFailure("AUTOMATIC_QUOTA_RESERVATION_CONFLICT", "fulfillment already has a different automatic quota reservation")
				return nil
			}
			if quota.State == "released" {
				logicalErr = paymentFailure("AUTOMATIC_QUOTA_ALREADY_RELEASED", "released automatic quota cannot be reserved again")
				return nil
			}
			if quota.State != "reserved" {
				logicalErr = paymentFailure("AUTOMATIC_QUOTA_STATE_INVALID", "automatic quota ledger state is invalid")
				return nil
			}
			if !fulfillment.MoneyBoundary.Valid && versionStale {
				logicalErr = paymentFailure("AUTOMATIC_SCOPE_VERSION_STALE", "automatic scope version changed and was paused")
				return nil
			}
			if !fulfillment.MoneyBoundary.Valid && scope.Status != "active" {
				logicalErr = paymentFailure("AUTOMATIC_SCOPE_INACTIVE", "automatic scope is paused before the money boundary")
			}
			return nil
		}

		if fulfillment.RunMode != "" && fulfillment.RunMode != "automatic" {
			logicalErr = paymentFailure("AUTOMATIC_SCOPE_RUN_MODE_CONFLICT", "fulfillment is bound to another run mode")
			return nil
		}
		if fulfillment.MoneyBoundary.Valid {
			logicalErr = paymentFailure("AUTOMATIC_SCOPE_MONEY_BOUNDARY_MISSED", "automatic quota cannot be added after the money boundary")
			return nil
		}
		if versionStale {
			logicalErr = paymentFailure("AUTOMATIC_SCOPE_VERSION_STALE", "automatic scope version changed and was paused")
			return nil
		}
		activationTime, activationErr := time.Parse(time.RFC3339Nano, scope.ActivatedAt)
		orderTime, orderErr := time.Parse(time.RFC3339Nano, fulfillment.OrderCreated)
		if scope.Status != "active" || scope.ActivatedAt == "" || activationErr != nil || orderErr != nil || orderTime.Before(activationTime) {
			logicalErr = paymentFailure("AUTOMATIC_SCOPE_INACTIVE", "order is outside the automatic scope activation window")
			return nil
		}

		date := paymentBusinessDate(now)
		var currentUnits int64
		var currentRiskUSD float64
		usageErr := tx.QueryRowContext(ctx, `
      SELECT order_units,risk_reserved_usd FROM automatic_checkout_daily_usage
      WHERE scope_id=? AND business_date=?`, scope.ID, date).Scan(&currentUnits, &currentRiskUSD)
		if errors.Is(usageErr, sql.ErrNoRows) {
			currentUnits, currentRiskUSD = 0, 0
		} else if usageErr != nil {
			return usageErr
		}
		currentRisk, centsErr := paymentExactCents(currentRiskUSD, false)
		limitCents, limitErr := paymentExactCents(scope.DailyRiskLimit, true)
		nextRisk, addErr := paymentAddCents(currentRisk, riskCents)
		if centsErr != nil || limitErr != nil || addErr != nil {
			logicalErr = paymentFailure("AUTOMATIC_SCOPE_DAILY_RISK_LIMIT", "automatic quota ledger contains an invalid money amount")
			return nil
		}
		nextUnits := currentUnits + 1
		if nextUnits > scope.DailyOrderLimit {
			logicalErr = paymentFailure("AUTOMATIC_SCOPE_DAILY_ORDER_LIMIT", "automatic scope daily order quota is exhausted")
			return nil
		}
		if nextRisk > limitCents {
			logicalErr = paymentFailure("AUTOMATIC_SCOPE_DAILY_RISK_LIMIT", "automatic scope daily risk quota is exhausted")
			return nil
		}
		id, err := store.NewID("acqr_")
		if err != nil {
			return err
		}
		at := store.ISO(now)
		if _, err := tx.ExecContext(ctx, `
      INSERT INTO automatic_checkout_quota_reservations
        (id,scope_id,fulfillment_id,business_date,order_units,risk_reserved_usd,state,reserved_at)
      VALUES (?,?,?,?,1,?,'reserved',?)`, id, scope.ID, fulfillmentID, date, domain.USDFromCents(riskCents), at); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
      INSERT INTO automatic_checkout_daily_usage (scope_id,business_date,order_units,risk_reserved_usd,updated_at)
      VALUES (?,?,?,?,?) ON CONFLICT(scope_id,business_date) DO UPDATE SET
        order_units=excluded.order_units,risk_reserved_usd=excluded.risk_reserved_usd,updated_at=excluded.updated_at`,
			scope.ID, date, nextUnits, domain.USDFromCents(nextRisk), at); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `
      UPDATE membership_fulfillments SET run_mode='automatic',state_revision=state_revision+1,updated_at=?
      WHERE id=? AND money_boundary_at IS NULL`, at, fulfillmentID)
		return err
	})
	if err != nil {
		return err
	}
	return logicalErr
}

func paymentJSONString(value string) (string, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return "", err
	}
	return strings.TrimSuffix(buffer.String(), "\n"), nil
}

func paymentCanonicalJSON(value any) (string, error) {
	switch typed := value.(type) {
	case nil:
		return "null", nil
	case string:
		return paymentJSONString(typed)
	case bool:
		if typed {
			return "true", nil
		}
		return "false", nil
	case json.Number:
		number, err := strconv.ParseFloat(string(typed), 64)
		if err != nil || math.IsNaN(number) || math.IsInf(number, 0) {
			return "", paymentFailure("FUNDING_REQUEST_INVALID", "funding request contains an invalid number")
		}
		return paymentFormatJSONNumber(number), nil
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) {
			return "", paymentFailure("FUNDING_REQUEST_INVALID", "funding request contains an invalid number")
		}
		return paymentFormatJSONNumber(typed), nil
	case int:
		return strconv.Itoa(typed), nil
	case int64:
		return strconv.FormatInt(typed, 10), nil
	case []any:
		parts := make([]string, len(typed))
		for index, item := range typed {
			canonical, err := paymentCanonicalJSON(item)
			if err != nil {
				return "", err
			}
			parts[index] = canonical
		}
		return "[" + strings.Join(parts, ",") + "]", nil
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, key := range keys {
			encodedKey, err := paymentJSONString(key)
			if err != nil {
				return "", err
			}
			encodedValue, err := paymentCanonicalJSON(typed[key])
			if err != nil {
				return "", err
			}
			parts = append(parts, encodedKey+":"+encodedValue)
		}
		return "{" + strings.Join(parts, ",") + "}", nil
	default:
		return "", paymentFailure("FUNDING_REQUEST_INVALID", "funding request must contain only plain JSON values")
	}
}

func paymentFormatJSONNumber(value float64) string {
	if value == 0 {
		return "0"
	}
	absolute := math.Abs(value)
	// ECMAScript JSON.stringify uses decimal notation in [1e-6, 1e21).
	if absolute >= 1e-6 && absolute < 1e21 {
		return strconv.FormatFloat(value, 'f', -1, 64)
	}
	formatted := strconv.FormatFloat(value, 'e', -1, 64)
	mantissa, exponent, _ := strings.Cut(formatted, "e")
	sign := ""
	if strings.HasPrefix(exponent, "+") || strings.HasPrefix(exponent, "-") {
		sign, exponent = exponent[:1], exponent[1:]
	}
	exponent = strings.TrimLeft(exponent, "0")
	if exponent == "" {
		exponent = "0"
	}
	return mantissa + "e" + sign + exponent
}

func paymentFingerprint(canonical string) string {
	digest := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(digest[:])
}

func paymentIdempotencyKey(orderNo, operation string) (string, error) {
	orderNo = strings.TrimSpace(orderNo)
	if orderNo == "" || len(orderNo) > 140 || !regexp.MustCompile(`^[A-Za-z0-9._-]+$`).MatchString(orderNo) {
		return "", paymentFailure("FUNDING_IDEMPOTENCY_KEY_INVALID", "order number cannot be used as a funding idempotency key")
	}
	if operation != "open" && operation != "recharge" {
		return "", paymentFailure("FUNDING_REQUEST_INVALID", "funding operation is invalid")
	}
	return "kwr:" + orderNo + ":" + operation + ":v1", nil
}

func (p *Processor) paymentBuildFundingRequest(ctx context.Context, fulfillment Fulfillment, selection paymentSelection) (*paymentFundingRequest, error) {
	if selection.Plan.Operation == "none" {
		return nil, nil
	}
	request := &paymentFundingRequest{
		Operation: selection.Plan.Operation, AmountCents: selection.Plan.FundingAmountCents, FeeCents: selection.Plan.FeeCents,
	}
	var body map[string]any
	if selection.Plan.Operation == "recharge" {
		if selection.Card == nil || selection.Card.UpstreamCardID <= 0 || selection.Card.ID == "" || selection.Plan.FundingAmountCents <= 0 {
			return nil, paymentFailure("FUNDING_REQUEST_INVALID", "recharge plan is incomplete")
		}
		request.TargetCardID = selection.Card.ID
		request.RequestBody = paymentRequestBody{
			Operation: "recharge", CardID: selection.Card.UpstreamCardID, AmountCents: selection.Plan.FundingAmountCents,
		}
		body = map[string]any{
			"card_id": selection.Card.UpstreamCardID,
			"amount":  domain.USDFromCents(selection.Plan.FundingAmountCents),
		}
	} else if selection.Plan.Operation == "open" {
		if selection.Product == nil || selection.Product.Code == "" || selection.Plan.FundingAmountCents <= 0 {
			return nil, paymentFailure("FUNDING_REQUEST_INVALID", "new-card funding plan is incomplete")
		}
		holder, err := p.address.Cardholder(ctx)
		if err != nil {
			return nil, err
		}
		firstName, lastName := strings.TrimSpace(holder.FirstName), strings.TrimSpace(holder.LastName)
		if firstName == "" || lastName == "" || len(firstName) > 100 || len(lastName) > 100 {
			return nil, paymentFailure("NEW_CARD_HOLDER_NOT_CONFIGURED", "new-card funding requires a valid cardholder")
		}
		request.ProductCode = selection.Product.Code
		request.RequestBody = paymentRequestBody{
			Operation: "open", ProductCode: selection.Product.Code, FirstName: firstName, LastName: lastName,
			AmountCents: selection.Plan.FundingAmountCents,
		}
		body = map[string]any{
			"product_code": selection.Product.Code,
			"first_name":   firstName,
			"last_name":    lastName,
			"init_amount":  domain.USDFromCents(selection.Plan.FundingAmountCents),
		}
	} else {
		return nil, paymentFailure("FUNDING_REQUEST_INVALID", "funding operation is invalid")
	}
	canonical, err := paymentCanonicalJSON(body)
	if err != nil {
		return nil, err
	}
	key, err := paymentIdempotencyKey(fulfillment.OrderNo, request.Operation)
	if err != nil {
		return nil, err
	}
	request.CanonicalBody = canonical
	request.Fingerprint = paymentFingerprint(canonical)
	request.IdempotencyKey = key
	return request, nil
}

func paymentScanFundingIntent(row scanner) (paymentFundingIntent, error) {
	var intent paymentFundingIntent
	var amount, fee float64
	err := row.Scan(&intent.ID, &intent.FulfillmentID, &intent.ProviderKey, &intent.Operation, &intent.TargetCardID, &intent.ProductCode,
		&amount, &fee, &intent.IdempotencyKey, &intent.RequestFingerprint, &intent.RequestBodyEncrypted,
		&intent.State, &intent.ProviderResourceID, &intent.CreatedAt, &intent.SubmittedAt, &intent.ResolvedAt)
	if err != nil {
		return paymentFundingIntent{}, err
	}
	intent.AmountCents, err = paymentExactCents(amount, true)
	if err != nil {
		return paymentFundingIntent{}, paymentFailure("FUNDING_INTENT_STORAGE_INVALID", "stored funding amount is invalid")
	}
	intent.FeeCents, err = paymentExactCents(fee, false)
	if err != nil {
		return paymentFundingIntent{}, paymentFailure("FUNDING_INTENT_STORAGE_INVALID", "stored funding fee is invalid")
	}
	return intent, nil
}

func paymentLoadFundingIntentWith(ctx context.Context, query store.Execer, fulfillmentID string) (*paymentFundingIntent, error) {
	hasProvider, err := tableHasColumn(ctx, query, "funding_intents", "provider_key")
	if err != nil {
		return nil, err
	}
	if !hasProvider {
		return paymentLoadLegacyFundingIntentWith(ctx, query, fulfillmentID)
	}
	intent, err := paymentScanFundingIntent(query.QueryRowContext(ctx, `
	    SELECT id,fulfillment_id,provider_key,operation,target_card_id,product_code,amount,fee,idempotency_key,
      request_fingerprint,request_body_encrypted,state,provider_resource_id,created_at,submitted_at,resolved_at
    FROM funding_intents WHERE fulfillment_id=?`, fulfillmentID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &intent, nil
}

func paymentLoadLegacyFundingIntentWith(ctx context.Context, query store.Execer, fulfillmentID string) (*paymentFundingIntent, error) {
	var intent paymentFundingIntent
	var amount, fee float64
	err := query.QueryRowContext(ctx, `
	  SELECT id,fulfillment_id,operation,target_card_id,product_code,amount,fee,idempotency_key,
	    request_fingerprint,request_body_encrypted,state,provider_resource_id,created_at,submitted_at,resolved_at
	  FROM funding_intents WHERE fulfillment_id=?`, fulfillmentID).Scan(
		&intent.ID, &intent.FulfillmentID, &intent.Operation, &intent.TargetCardID, &intent.ProductCode,
		&amount, &fee, &intent.IdempotencyKey, &intent.RequestFingerprint, &intent.RequestBodyEncrypted,
		&intent.State, &intent.ProviderResourceID, &intent.CreatedAt, &intent.SubmittedAt, &intent.ResolvedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	intent.ProviderKey = provider.CardPlatformSpaceX
	intent.AmountCents, err = paymentExactCents(amount, true)
	if err != nil {
		return nil, paymentFailure("FUNDING_INTENT_STORAGE_INVALID", "stored funding amount is invalid")
	}
	intent.FeeCents, err = paymentExactCents(fee, false)
	if err != nil {
		return nil, paymentFailure("FUNDING_INTENT_STORAGE_INVALID", "stored funding fee is invalid")
	}
	return &intent, nil
}

func (p *Processor) paymentLoadFundingIntent(ctx context.Context, fulfillmentID string) (*paymentFundingIntent, error) {
	return paymentLoadFundingIntentWith(ctx, p.store.DB(), fulfillmentID)
}

func paymentNullableEquals(value sql.NullString, expected string) bool {
	if expected == "" {
		return !value.Valid || value.String == ""
	}
	return value.Valid && value.String == expected
}

func (p *Processor) paymentPrepareFundingIntent(ctx context.Context, fulfillment Fulfillment, request paymentFundingRequest, now time.Time) (paymentFundingIntent, error) {
	if request.Operation != "open" && request.Operation != "recharge" || request.AmountCents <= 0 || request.FeeCents < 0 {
		return paymentFundingIntent{}, paymentFailure("FUNDING_REQUEST_INVALID", "funding request is invalid")
	}
	expectedKey, err := paymentIdempotencyKey(fulfillment.OrderNo, request.Operation)
	if err != nil {
		return paymentFundingIntent{}, err
	}
	if request.IdempotencyKey != expectedKey || paymentFingerprint(request.CanonicalBody) != request.Fingerprint {
		return paymentFundingIntent{}, paymentFailure("FUNDING_REQUEST_PLAN_MISMATCH", "funding request fingerprint or idempotency key is invalid")
	}
	encrypted, err := p.decrypter.Encrypt(request.CanonicalBody)
	if err != nil {
		return paymentFundingIntent{}, err
	}
	var prepared paymentFundingIntent
	err = p.withFencedImmediate(ctx, func(tx *sql.Tx) error {
		var orderNo string
		var linked sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT order_no,card_reservation_id FROM membership_fulfillments WHERE id=?`, fulfillment.ID).
			Scan(&orderNo, &linked); errors.Is(err, sql.ErrNoRows) {
			return paymentFailure("MEMBERSHIP_FULFILLMENT_NOT_FOUND", "membership fulfillment does not exist")
		} else if err != nil {
			return err
		}
		if orderNo != fulfillment.OrderNo {
			return paymentFailure("FUNDING_ORDER_MISMATCH", "funding idempotency order does not match fulfillment")
		}
		reservation, found, err := paymentLoadReservationWith(ctx, tx, fulfillment.ID)
		if err != nil {
			return err
		}
		if !found || reservation.State != "reserved" || (linked.Valid && linked.String != reservation.ID) {
			return paymentFailure("ACTIVE_CARD_RESERVATION_REQUIRED", "funding intent requires an active card reservation")
		}
		if request.Operation == "recharge" {
			if !reservation.CardID.Valid || reservation.CardID.String != request.TargetCardID {
				return paymentFailure("FUNDING_REQUEST_PLAN_MISMATCH", "recharge card differs from the capacity reservation")
			}
			card, err := p.paymentLoadCard(ctx, tx, request.TargetCardID)
			if err != nil {
				return err
			}
			if card.UpstreamCardID != request.RequestBody.CardID || request.RequestBody.AmountCents != request.AmountCents {
				return paymentFailure("FUNDING_REQUEST_PLAN_MISMATCH", "recharge body differs from the funding plan")
			}
		} else {
			if reservation.CardID.Valid || !reservation.PlannedProductCode.Valid || reservation.PlannedProductCode.String != request.ProductCode ||
				request.RequestBody.ProductCode != request.ProductCode || request.RequestBody.AmountCents != request.AmountCents ||
				strings.TrimSpace(request.RequestBody.FirstName) == "" || strings.TrimSpace(request.RequestBody.LastName) == "" {
				return paymentFailure("FUNDING_REQUEST_PLAN_MISMATCH", "open-card body differs from the funding plan")
			}
		}
		existing, err := paymentLoadFundingIntentWith(ctx, tx, fulfillment.ID)
		if err != nil {
			return err
		}
		if existing != nil {
			if existing.ProviderKey != reservation.ProviderKey || existing.Operation != request.Operation || !paymentNullableEquals(existing.TargetCardID, request.TargetCardID) ||
				!paymentNullableEquals(existing.ProductCode, request.ProductCode) || existing.AmountCents != request.AmountCents ||
				existing.FeeCents != request.FeeCents || existing.IdempotencyKey != request.IdempotencyKey ||
				existing.RequestFingerprint != request.Fingerprint {
				return paymentFailure("FUNDING_INTENT_CONFLICT", "funding intent is immutable")
			}
			prepared = *existing
			return nil
		}
		id, err := store.NewID("mfi_")
		if err != nil {
			return err
		}
		var targetCardID, productCode any
		if request.TargetCardID != "" {
			targetCardID = request.TargetCardID
		}
		if request.ProductCode != "" {
			productCode = request.ProductCode
		}
		if _, err := tx.ExecContext(ctx, `
      INSERT INTO funding_intents (
	        id,fulfillment_id,provider_key,operation,target_card_id,product_code,amount,fee,idempotency_key,
        request_fingerprint,request_body_encrypted,state,created_at
	      ) VALUES (?,?,?,?,?,?,?,?,?,?,?,'prepared',?)`, id, fulfillment.ID, reservation.ProviderKey, request.Operation, targetCardID, productCode,
			domain.USDFromCents(request.AmountCents), domain.USDFromCents(request.FeeCents), request.IdempotencyKey,
			request.Fingerprint, encrypted, store.ISO(now)); err != nil {
			return err
		}
		created, err := paymentLoadFundingIntentWith(ctx, tx, fulfillment.ID)
		if err != nil {
			return err
		}
		prepared = *created
		return nil
	})
	return prepared, err
}

func paymentDecodeFundingBody(serialized, operation string) (paymentRequestBody, string, error) {
	decoder := json.NewDecoder(strings.NewReader(serialized))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return paymentRequestBody{}, "", paymentFailure("FUNDING_INTENT_STORAGE_INVALID", "stored funding request is invalid JSON")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return paymentRequestBody{}, "", paymentFailure("FUNDING_INTENT_STORAGE_INVALID", "stored funding request contains trailing JSON")
	}
	object, ok := value.(map[string]any)
	if !ok {
		return paymentRequestBody{}, "", paymentFailure("FUNDING_INTENT_STORAGE_INVALID", "stored funding request is not an object")
	}
	canonical, err := paymentCanonicalJSON(object)
	if err != nil {
		return paymentRequestBody{}, "", paymentFailure("FUNDING_INTENT_STORAGE_INVALID", "stored funding request is not canonical JSON")
	}
	amountField := "amount"
	expectedKeys := []string{"amount", "card_id"}
	if operation == "open" {
		amountField = "init_amount"
		expectedKeys = []string{"first_name", "init_amount", "last_name", "product_code"}
	} else if operation != "recharge" {
		return paymentRequestBody{}, "", paymentFailure("FUNDING_INTENT_STORAGE_INVALID", "stored funding operation is invalid")
	}
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if strings.Join(keys, ",") != strings.Join(expectedKeys, ",") {
		return paymentRequestBody{}, "", paymentFailure("FUNDING_INTENT_STORAGE_INVALID", "stored funding request fields are invalid")
	}
	number, ok := object[amountField].(json.Number)
	if !ok {
		return paymentRequestBody{}, "", paymentFailure("FUNDING_INTENT_STORAGE_INVALID", "stored funding amount is invalid")
	}
	amount, err := number.Float64()
	if err != nil {
		return paymentRequestBody{}, "", paymentFailure("FUNDING_INTENT_STORAGE_INVALID", "stored funding amount is invalid")
	}
	amountCents, err := paymentExactCents(amount, true)
	if err != nil {
		return paymentRequestBody{}, "", paymentFailure("FUNDING_INTENT_STORAGE_INVALID", "stored funding amount is invalid")
	}
	body := paymentRequestBody{Operation: operation, AmountCents: amountCents}
	if operation == "recharge" {
		cardNumber, ok := object["card_id"].(json.Number)
		if !ok {
			return paymentRequestBody{}, "", paymentFailure("FUNDING_INTENT_STORAGE_INVALID", "stored recharge card is invalid")
		}
		body.CardID, err = cardNumber.Int64()
		if err != nil || body.CardID <= 0 {
			return paymentRequestBody{}, "", paymentFailure("FUNDING_INTENT_STORAGE_INVALID", "stored recharge card is invalid")
		}
	} else {
		body.ProductCode, ok = object["product_code"].(string)
		if !ok {
			return paymentRequestBody{}, "", paymentFailure("FUNDING_INTENT_STORAGE_INVALID", "stored open-card product is invalid")
		}
		body.FirstName, ok = object["first_name"].(string)
		if !ok {
			return paymentRequestBody{}, "", paymentFailure("FUNDING_INTENT_STORAGE_INVALID", "stored cardholder is invalid")
		}
		body.LastName, ok = object["last_name"].(string)
		if !ok || strings.TrimSpace(body.ProductCode) == "" || strings.TrimSpace(body.FirstName) == "" || strings.TrimSpace(body.LastName) == "" {
			return paymentRequestBody{}, "", paymentFailure("FUNDING_INTENT_STORAGE_INVALID", "stored open-card request is invalid")
		}
	}
	return body, canonical, nil
}

func (p *Processor) paymentAssertInitialGate(ctx context.Context, fulfillmentID string, gate paymentGate) error {
	if !gate.enabled || (gate.mode != "canary" && gate.mode != "automatic") {
		return paymentFailure("MEMBERSHIP_PAYMENT_GATE_LOCKED", "membership funding payment gate is locked")
	}
	var runMode sql.NullString
	if err := p.store.DB().QueryRowContext(ctx, `SELECT run_mode FROM membership_fulfillments WHERE id=?`, fulfillmentID).Scan(&runMode); errors.Is(err, sql.ErrNoRows) {
		return paymentFailure("MEMBERSHIP_FULFILLMENT_NOT_FOUND", "membership fulfillment does not exist")
	} else if err != nil {
		return err
	}
	if gate.mode == "automatic" {
		var reservationState, scopeStatus string
		err := p.store.DB().QueryRowContext(ctx, `
      SELECT reservation.state,scope.status
      FROM automatic_checkout_quota_reservations reservation
      JOIN automatic_checkout_scopes scope ON scope.id=reservation.scope_id
      WHERE reservation.fulfillment_id=?`, fulfillmentID).Scan(&reservationState, &scopeStatus)
		if errors.Is(err, sql.ErrNoRows) {
			return paymentFailure("AUTOMATIC_FUNDING_QUOTA_REQUIRED", "automatic funding requires a reserved daily quota")
		}
		if err != nil {
			return err
		}
		if !runMode.Valid || runMode.String != "automatic" || reservationState != "reserved" {
			return paymentFailure("AUTOMATIC_FUNDING_QUOTA_REQUIRED", "automatic funding requires a reserved daily quota")
		}
		if scopeStatus != "active" {
			return paymentFailure("AUTOMATIC_FUNDING_SCOPE_INACTIVE", "automatic scope is inactive before funding")
		}
		return nil
	}
	if runMode.Valid && runMode.String != "canary" {
		return paymentFailure("MEMBERSHIP_FUNDING_RUN_MODE_CONFLICT", "fulfillment run mode differs from the funding gate")
	}
	return nil
}

type paymentAcquiredFunding struct {
	Intent           paymentFundingIntent
	Body             paymentRequestBody
	AlreadySucceeded bool
}

type paymentRecoveryCandidate struct {
	Fulfillment Fulfillment
	Intent      paymentFundingIntent
	Body        paymentRequestBody
	Reservation paymentReservation
}

func (p *Processor) paymentValidateRecoveryCandidate(
	ctx context.Context,
	query store.Execer,
	fulfillmentID string,
	allowOrphanedSubmitted bool,
) (paymentRecoveryCandidate, error) {
	fulfillment, err := loadFulfillment(ctx, query, fulfillmentID)
	if err != nil {
		return paymentRecoveryCandidate{}, err
	}
	if !fulfillment.RunMode.Valid ||
		(fulfillment.RunMode.String != "canary" && fulfillment.RunMode.String != "automatic") {
		return paymentRecoveryCandidate{}, paymentFailure("MEMBERSHIP_PAYMENT_GATE_LOCKED", "funding recovery requires an explicit fulfillment run mode")
	}
	intent, err := paymentLoadFundingIntentWith(ctx, query, fulfillmentID)
	if err != nil {
		return paymentRecoveryCandidate{}, err
	}
	if intent == nil {
		return paymentRecoveryCandidate{}, paymentFailure("FUNDING_INTENT_NOT_FOUND", "funding intent does not exist")
	}
	switch intent.State {
	case "outcome_unknown":
		if fulfillment.State != "FUNDING_OUTCOME_UNKNOWN" {
			return paymentRecoveryCandidate{}, paymentFailure("FUNDING_RECOVERY_NOT_ALLOWED", "unknown funding intent is outside its recovery state")
		}
	case "submitted":
		if !allowOrphanedSubmitted {
			return paymentRecoveryCandidate{}, paymentFailure("FUNDING_SUBMISSION_IN_PROGRESS", "submitted funding requires explicit orphan confirmation")
		}
		if fulfillment.State != "FUNDING" && fulfillment.State != "FUNDING_OUTCOME_UNKNOWN" {
			return paymentRecoveryCandidate{}, paymentFailure("FUNDING_RECOVERY_NOT_ALLOWED", "orphaned funding intent is outside its recovery state")
		}
	default:
		return paymentRecoveryCandidate{}, paymentFailure("FUNDING_RECOVERY_NOT_ALLOWED", "only an unknown funding outcome can be recovered")
	}
	if !intent.SubmittedAt.Valid || strings.TrimSpace(intent.SubmittedAt.String) == "" ||
		!fulfillment.MoneyBoundaryAt.Valid || strings.TrimSpace(fulfillment.MoneyBoundaryAt.String) == "" ||
		intent.ProviderResourceID.Valid {
		return paymentRecoveryCandidate{}, paymentFailure("FUNDING_INTENT_STORAGE_INVALID", "unresolved funding intent is missing its money-boundary evidence")
	}
	serialized, err := p.decrypter.Decrypt(intent.RequestBodyEncrypted)
	if err != nil {
		return paymentRecoveryCandidate{}, paymentFailure("FUNDING_INTENT_STORAGE_INVALID", "stored funding request cannot be decrypted")
	}
	body, canonical, err := paymentDecodeFundingBody(serialized, intent.Operation)
	if err != nil || serialized != canonical || paymentFingerprint(canonical) != intent.RequestFingerprint ||
		body.AmountCents != intent.AmountCents {
		return paymentRecoveryCandidate{}, paymentFailure("FUNDING_INTENT_STORAGE_INVALID", "stored funding request does not match its immutable fingerprint")
	}
	expectedKey, keyErr := paymentIdempotencyKey(fulfillment.OrderNo, intent.Operation)
	if keyErr != nil || intent.IdempotencyKey != expectedKey {
		return paymentRecoveryCandidate{}, paymentFailure("FUNDING_INTENT_STORAGE_INVALID", "stored funding idempotency key is invalid")
	}
	reservation, found, err := paymentLoadReservationWith(ctx, query, fulfillmentID)
	if err != nil {
		return paymentRecoveryCandidate{}, err
	}
	if !found || reservation.State != "reserved" || reservation.TargetLane != fulfillment.TargetTier ||
		!fulfillment.CardReservationID.Valid || fulfillment.CardReservationID.String != reservation.ID {
		return paymentRecoveryCandidate{}, paymentFailure("FUNDING_INTENT_STORAGE_INVALID", "stored funding intent does not match its active reservation")
	}
	if intent.Operation == "recharge" {
		if !intent.TargetCardID.Valid || intent.TargetCardID.String == "" ||
			!paymentNullableEquals(intent.ProductCode, "") ||
			!reservation.CardID.Valid || reservation.CardID.String != intent.TargetCardID.String ||
			!paymentNullableEquals(reservation.PlannedProductCode, "") ||
			!reservation.SlotIndex.Valid || reservation.SlotIndex.Int64 <= 0 {
			return paymentRecoveryCandidate{}, paymentFailure("FUNDING_INTENT_STORAGE_INVALID", "stored recharge intent conflicts with its reservation")
		}
		card, err := p.paymentLoadCard(ctx, query, intent.TargetCardID.String)
		if err != nil || card.ProviderKey != intent.ProviderKey || card.UpstreamCardID != body.CardID {
			return paymentRecoveryCandidate{}, paymentFailure("FUNDING_INTENT_STORAGE_INVALID", "stored recharge target does not match its request body")
		}
	} else if intent.Operation == "open" {
		if !paymentNullableEquals(intent.TargetCardID, "") || !intent.ProductCode.Valid || intent.ProductCode.String == "" ||
			reservation.CardID.Valid || !reservation.PlannedProductCode.Valid ||
			reservation.PlannedProductCode.String != intent.ProductCode.String || reservation.SlotIndex.Valid ||
			body.ProductCode != intent.ProductCode.String {
			return paymentRecoveryCandidate{}, paymentFailure("FUNDING_INTENT_STORAGE_INVALID", "stored open-card intent conflicts with its reservation")
		}
	} else {
		return paymentRecoveryCandidate{}, paymentFailure("FUNDING_INTENT_STORAGE_INVALID", "stored funding operation is invalid")
	}
	return paymentRecoveryCandidate{
		Fulfillment: fulfillment,
		Intent:      *intent,
		Body:        body,
		Reservation: reservation,
	}, nil
}

func (p *Processor) paymentAcquireRecoveryCall(
	ctx context.Context,
	fulfillmentID string,
	allowOrphanedSubmitted bool,
	now time.Time,
) (paymentRecoveryCandidate, error) {
	var acquired paymentRecoveryCandidate
	err := p.withFencedImmediate(ctx, func(tx *sql.Tx) error {
		candidate, err := p.paymentValidateRecoveryCandidate(ctx, tx, fulfillmentID, allowOrphanedSubmitted)
		if err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `
      UPDATE funding_intents SET state='submitted',submitted_at=COALESCE(submitted_at,?),resolved_at=NULL
      WHERE id=? AND state=?`, store.ISO(now), candidate.Intent.ID, candidate.Intent.State)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if changed != 1 {
			return paymentFailure("FUNDING_INTENT_STATE_CONFLICT", "funding recovery encountered a concurrent state conflict")
		}
		if _, err := tx.ExecContext(ctx, `
      UPDATE membership_fulfillments SET money_boundary_at=COALESCE(money_boundary_at,?),updated_at=?
      WHERE id=?`, store.ISO(now), store.ISO(now), fulfillmentID); err != nil {
			return err
		}
		refreshed, err := paymentLoadFundingIntentWith(ctx, tx, fulfillmentID)
		if err != nil {
			return err
		}
		if refreshed == nil || refreshed.State != "submitted" {
			return paymentFailure("FUNDING_INTENT_STATE_CONFLICT", "funding recovery lost its submitted intent")
		}
		candidate.Intent = *refreshed
		acquired = candidate
		return nil
	})
	return acquired, err
}

func paymentInvokePersistedFunding(
	ctx context.Context,
	client provider.CardPlatform,
	intent paymentFundingIntent,
	body paymentRequestBody,
) (*provider.OpenCardResult, error) {
	if intent.Operation == "recharge" {
		return nil, client.RechargeCard(ctx, body.CardID, domain.USDFromCents(body.AmountCents), intent.IdempotencyKey)
	}
	result, err := client.OpenCard(ctx, provider.OpenCardInput{
		ProductCode: body.ProductCode,
		FirstName:   body.FirstName,
		LastName:    body.LastName,
		InitAmount:  domain.USDFromCents(body.AmountCents),
	}, intent.IdempotencyKey)
	if err != nil {
		return nil, err
	}
	return &result, nil
}

// RecoverFundingOutcome explicitly replays one immutable funding request. It is
// intentionally absent from RunOnce and cannot be reached through ordinary retry.
func (p *Processor) RecoverFundingOutcome(ctx context.Context, fulfillmentID string, options FundingRecoveryOptions) error {
	if !options.Enabled {
		return paymentFailure("MEMBERSHIP_PAYMENT_GATE_LOCKED", "funding recovery gate is locked")
	}
	now := p.now().UTC()
	candidate, err := p.paymentValidateRecoveryCandidate(ctx, p.store.DB(), fulfillmentID, options.AllowOrphanedSubmitted)
	if err != nil {
		return err
	}
	if _, found, err := p.paymentPersistedSnapshot(ctx, candidate.Fulfillment, candidate.Reservation); err != nil {
		return err
	} else if !found {
		return paymentFailure("PAYMENT_STAGE_SNAPSHOT_CONFLICT", "funding recovery is missing its immutable payment-stage snapshot")
	}
	if err := p.assertWorkAllowed(ctx); err != nil {
		return err
	}
	if err := p.paymentAcquirePlatformCircuit(ctx, candidate.Intent.ProviderKey, now); err != nil {
		return err
	}
	client, err := p.cardPlatform(ctx, candidate.Intent.ProviderKey)
	if err != nil {
		p.paymentRecordPlatformFailure(ctx, candidate.Intent.ProviderKey, err, now)
		return err
	}
	capabilities := client.Capabilities()
	if candidate.Intent.Operation == "recharge" && !capabilities.RechargeIdempotent {
		return paymentFailure("FUNDING_RECOVERY_UNSUPPORTED", "this card platform cannot safely replay recharge requests")
	}
	if capabilities.FundingReplayWindow > 0 {
		submittedAt, parseErr := time.Parse(time.RFC3339Nano, candidate.Intent.SubmittedAt.String)
		if parseErr != nil || now.Sub(submittedAt) > capabilities.FundingReplayWindow {
			return paymentFailure("FUNDING_RECOVERY_WINDOW_EXPIRED", "card platform funding replay window has expired")
		}
	}
	acquired, err := p.paymentAcquireRecoveryCall(ctx, fulfillmentID, options.AllowOrphanedSubmitted, now)
	if err != nil {
		return err
	}
	if err := p.assertWorkAllowed(ctx); err != nil {
		return err
	}
	openResult, callErr := paymentInvokePersistedFunding(ctx, client, acquired.Intent, acquired.Body)
	if err := p.assertWorkAllowed(ctx); err != nil {
		return err
	}
	if callErr != nil {
		p.paymentRecordPlatformFailure(ctx, acquired.Intent.ProviderKey, callErr, now)
	} else {
		p.paymentRecordPlatformSuccess(ctx, acquired.Intent.ProviderKey, now)
		if openResult != nil {
			callErr = paymentValidateOpenResult(acquired.Intent, *openResult)
		}
	}
	if callErr == nil {
		callErr = p.paymentPersistFundingSuccess(ctx, acquired.Intent, openResult, now)
	}
	if callErr != nil {
		if errorCode(callErr) == "FUNDING_INTENT_STATE_CONFLICT" {
			return callErr
		}
		return p.paymentPersistRecoveryFailure(ctx, acquired.Intent, callErr, now)
	}
	fulfillment, err := loadFulfillment(ctx, p.store.DB(), fulfillmentID)
	if err != nil {
		return err
	}
	reservation, found, err := p.paymentExistingReservation(ctx, fulfillmentID)
	if err != nil {
		return err
	}
	if !found || !reservation.CardID.Valid || reservation.CardID.String == "" {
		return paymentFailure("PAYMENT_STAGE_SNAPSHOT_CONFLICT", "funding recovery did not attach its original card reservation")
	}
	snapshot, found, err := p.paymentPersistedSnapshot(ctx, fulfillment, reservation)
	if err != nil {
		return err
	}
	if !found {
		return paymentFailure("PAYMENT_STAGE_SNAPSHOT_CONFLICT", "funding recovery is missing its immutable payment-stage snapshot")
	}
	selection := paymentSelection{
		ProviderKey: acquired.Intent.ProviderKey,
		Reservation: reservation,
		Snapshot:    snapshot,
		Plan:        paymentFundingPlan{Operation: acquired.Intent.Operation, FeeCents: acquired.Intent.FeeCents},
	}
	return p.paymentFinalize(ctx, fulfillment, selection, now)
}

func (p *Processor) paymentPersistRecoveryFailure(
	ctx context.Context,
	intent paymentFundingIntent,
	cause error,
	now time.Time,
) error {
	state := "outcome_unknown"
	resultErr := paymentFailure("FUNDING_OUTCOME_UNKNOWN", "funding recovery outcome remains unknown", true)
	stageState := "funding_unknown"
	fulfillmentState := "FUNDING_OUTCOME_UNKNOWN"
	if paymentKnownNoWrite(cause) {
		state = "failed"
		resultErr = paymentFailure("FUNDING_PROVIDER_REJECTED", "funding provider explicitly rejected the recovered request")
		stageState = "funding_failed"
		fulfillmentState = "CHECKOUT_PRE_SUBMIT_FAILED"
	}
	err := p.withFencedImmediate(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `
      UPDATE funding_intents SET state=?,resolved_at=? WHERE id=? AND state='submitted'`,
			state, store.ISO(now), intent.ID)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if changed != 1 {
			return paymentFailure("FUNDING_INTENT_STATE_CONFLICT", "funding recovery result encountered a concurrent state conflict")
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err := p.paymentMarkStages(ctx, intent.FulfillmentID, stageState, "", now); err != nil {
		return err
	}
	if err := p.paymentTransitionFailure(ctx, intent.FulfillmentID, fulfillmentState, errorCode(resultErr), now, false); err != nil {
		return err
	}
	return resultErr
}

func (p *Processor) paymentAcquireFundingCall(ctx context.Context, fulfillmentID string, now time.Time) (paymentAcquiredFunding, error) {
	var acquired paymentAcquiredFunding
	err := p.withFencedImmediate(ctx, func(tx *sql.Tx) error {
		intent, err := paymentLoadFundingIntentWith(ctx, tx, fulfillmentID)
		if err != nil {
			return err
		}
		if intent == nil {
			return paymentFailure("FUNDING_INTENT_NOT_FOUND", "funding intent does not exist")
		}
		if intent.State == "succeeded" {
			acquired.Intent = *intent
			acquired.AlreadySucceeded = true
			return nil
		}
		if intent.State == "outcome_unknown" {
			return paymentFailure("FUNDING_RECOVERY_REQUIRED", "unknown funding outcome cannot use ordinary submission")
		}
		if intent.State == "submitted" {
			return paymentFailure("FUNDING_SUBMISSION_IN_PROGRESS", "funding request is already marked submitted")
		}
		if intent.State != "prepared" {
			return paymentFailure("FUNDING_SUBMISSION_NOT_ALLOWED", "funding intent state does not allow submission")
		}
		serialized, err := p.decrypter.Decrypt(intent.RequestBodyEncrypted)
		if err != nil {
			return paymentFailure("FUNDING_INTENT_STORAGE_INVALID", "stored funding request cannot be decrypted")
		}
		body, canonical, err := paymentDecodeFundingBody(serialized, intent.Operation)
		if err != nil || serialized != canonical || paymentFingerprint(canonical) != intent.RequestFingerprint || body.AmountCents != intent.AmountCents {
			return paymentFailure("FUNDING_INTENT_STORAGE_INVALID", "stored funding request fingerprint does not match")
		}
		if intent.Operation == "recharge" {
			if !intent.TargetCardID.Valid || intent.TargetCardID.String == "" {
				return paymentFailure("FUNDING_INTENT_STORAGE_INVALID", "stored recharge target is missing")
			}
			card, err := p.paymentLoadCard(ctx, tx, intent.TargetCardID.String)
			if err != nil || card.ProviderKey != intent.ProviderKey || card.UpstreamCardID != body.CardID {
				return paymentFailure("FUNDING_INTENT_STORAGE_INVALID", "stored recharge target does not match its body")
			}
		} else if !intent.ProductCode.Valid || intent.ProductCode.String != body.ProductCode {
			return paymentFailure("FUNDING_INTENT_STORAGE_INVALID", "stored open-card product does not match its body")
		}
		at := store.ISO(now)
		if _, err := tx.ExecContext(ctx, `
      UPDATE funding_intents SET state='submitted',submitted_at=COALESCE(submitted_at,?),resolved_at=NULL
      WHERE id=?`, at, intent.ID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
      UPDATE membership_fulfillments SET money_boundary_at=COALESCE(money_boundary_at,?),updated_at=?
      WHERE id=?`, at, at, fulfillmentID); err != nil {
			return err
		}
		refreshed, err := paymentLoadFundingIntentWith(ctx, tx, fulfillmentID)
		if err != nil {
			return err
		}
		acquired = paymentAcquiredFunding{Intent: *refreshed, Body: body}
		return nil
	})
	return acquired, err
}

func paymentKnownNoWrite(err error) bool {
	var remote *provider.Error
	if !errors.As(err, &remote) {
		return false
	}
	if remote.KnownNoWrite {
		return true
	}
	return remote.ErrorCode == "SPACEXCARD_AUTH_FAILED" || remote.ErrorCode == "SPACEXCARD_ACCESS_DENIED" ||
		remote.ErrorCode == "SPACEXCARD_OPERATION_REJECTED"
}

func (p *Processor) paymentSubmitFundingIntent(ctx context.Context, client provider.CardPlatform, fulfillmentID string, gate paymentGate, now time.Time) error {
	if err := p.paymentAssertInitialGate(ctx, fulfillmentID, gate); err != nil {
		return err
	}
	if err := p.assertWorkAllowed(ctx); err != nil {
		return err
	}
	acquired, err := p.paymentAcquireFundingCall(ctx, fulfillmentID, now)
	if err != nil || acquired.AlreadySucceeded {
		return err
	}
	// The acquire transaction deliberately crosses the durable money boundary.
	// If authority was lost during it, leave the intent submitted for evidence-gated recovery.
	if err := p.assertWorkAllowed(ctx); err != nil {
		return err
	}
	intent := acquired.Intent
	if intent.ProviderKey != client.Key() {
		return paymentFailure("FUNDING_INTENT_STORAGE_INVALID", "funding intent card platform does not match its executor")
	}
	openResult, err := paymentInvokePersistedFunding(ctx, client, intent, acquired.Body)
	// No result, including an explicit provider rejection, is persisted after
	// lease loss or standby. The submitted intent is the recovery evidence.
	if workErr := p.assertWorkAllowed(ctx); workErr != nil {
		return workErr
	}
	if err != nil {
		p.paymentRecordPlatformFailure(ctx, intent.ProviderKey, err, now)
	} else {
		p.paymentRecordPlatformSuccess(ctx, intent.ProviderKey, now)
		if openResult != nil {
			err = paymentValidateOpenResult(intent, *openResult)
		}
	}
	if err == nil {
		err = p.paymentPersistFundingSuccess(ctx, intent, openResult, now)
	}
	if err == nil {
		return nil
	}
	if errorCode(err) == "FUNDING_INTENT_STATE_CONFLICT" {
		return err
	}
	state := "outcome_unknown"
	resultError := paymentFailure("FUNDING_OUTCOME_UNKNOWN", "funding request outcome is unknown and requires evidence-gated recovery", true)
	if paymentKnownNoWrite(err) {
		state = "failed"
		resultError = paymentFailure("FUNDING_PROVIDER_REJECTED", "funding provider explicitly rejected the request")
	}
	markErr := p.withFencedImmediate(ctx, func(tx *sql.Tx) error {
		_, updateErr := tx.ExecContext(ctx, `
      UPDATE funding_intents SET state=?,resolved_at=? WHERE id=? AND state='submitted'`,
			state, store.ISO(now), intent.ID)
		return updateErr
	})
	if markErr != nil {
		return markErr
	}
	return resultError
}

func paymentValidateOpenResult(intent paymentFundingIntent, result provider.OpenCardResult) error {
	available, availableErr := paymentRoundedCents(result.AvailableAmount, false)
	openFee, feeErr := paymentRoundedCents(result.OpenFee, false)
	if result.UpstreamCardID <= 0 || strings.TrimSpace(result.VMCardID) == "" || !intent.ProductCode.Valid ||
		strings.TrimSpace(result.ProductCode) != intent.ProductCode.String || availableErr != nil || feeErr != nil ||
		available != intent.AmountCents || openFee != intent.FeeCents || strings.ToUpper(strings.TrimSpace(result.Status)) != "ACTIVE" {
		return paymentFailure("FUNDING_PROVIDER_RESULT_MISMATCH", "open-card result does not match the immutable funding intent")
	}
	return nil
}

func (p *Processor) paymentPersistFundingSuccess(ctx context.Context, intent paymentFundingIntent, result *provider.OpenCardResult, now time.Time) error {
	return p.withFencedImmediate(ctx, func(tx *sql.Tx) error {
		var providerResource any
		if intent.Operation == "open" {
			if result == nil {
				return paymentFailure("FUNDING_PROVIDER_RESULT_MISMATCH", "open-card result is missing")
			}
			if _, err := p.paymentPersistOpenedCard(ctx, tx, intent, *result, now); err != nil {
				return err
			}
			providerResource = strconv.FormatInt(result.UpstreamCardID, 10)
		}
		update, err := tx.ExecContext(ctx, `
      UPDATE funding_intents SET state='succeeded',provider_resource_id=?,resolved_at=?
      WHERE id=? AND state='submitted'`, providerResource, store.ISO(now), intent.ID)
		if err != nil {
			return err
		}
		changed, err := update.RowsAffected()
		if err != nil {
			return err
		}
		if changed != 1 {
			return paymentFailure("FUNDING_INTENT_STATE_CONFLICT", "funding result encountered a concurrent state conflict")
		}
		return nil
	})
}

func (p *Processor) paymentPersistOpenedCard(ctx context.Context, tx *sql.Tx, intent paymentFundingIntent, result provider.OpenCardResult, now time.Time) (string, error) {
	reservation, found, err := paymentLoadReservationWith(ctx, tx, intent.FulfillmentID)
	if err != nil {
		return "", err
	}
	if !found || reservation.State != "reserved" || !reservation.PlannedProductCode.Valid ||
		reservation.PlannedProductCode.String != result.ProductCode {
		return "", paymentFailure("OPEN_CARD_RESERVATION_MISMATCH", "open-card result differs from the persisted capacity plan")
	}
	var card paymentCard
	hasProvider, err := tableHasColumn(ctx, tx, "managed_cards", "provider_key")
	if err != nil {
		return "", err
	}
	if hasProvider {
		err = tx.QueryRowContext(ctx, `
	    SELECT id,provider_key,upstream_card_id,vm_card_id,product_code,upstream_status,cached_available_amount,
      lane,consumed_slots,capacity_state,reconciliation_state
	    FROM managed_cards WHERE provider_key=? AND upstream_card_id=?`, intent.ProviderKey, result.UpstreamCardID).Scan(
			&card.ID, &card.ProviderKey, &card.UpstreamCardID, &card.VMCardID, &card.ProductCode, &card.UpstreamStatus,
			&card.CachedAvailableAmount, &card.Lane, &card.ConsumedSlots, &card.CapacityState, &card.ReconciliationState,
		)
	} else {
		card.ProviderKey = provider.CardPlatformSpaceX
		err = tx.QueryRowContext(ctx, `
	    SELECT id,upstream_card_id,vm_card_id,product_code,upstream_status,cached_available_amount,
	      lane,consumed_slots,capacity_state,reconciliation_state
	    FROM managed_cards WHERE upstream_card_id=?`, result.UpstreamCardID).Scan(
			&card.ID, &card.UpstreamCardID, &card.VMCardID, &card.ProductCode, &card.UpstreamStatus,
			&card.CachedAvailableAmount, &card.Lane, &card.ConsumedSlots, &card.CapacityState, &card.ReconciliationState,
		)
	}
	if errors.Is(err, sql.ErrNoRows) {
		id, idErr := store.NewID("mc_")
		if idErr != nil {
			return "", idErr
		}
		at := store.ISO(now)
		if hasProvider {
			_, err = tx.ExecContext(ctx, `
      INSERT INTO managed_cards (
	        id,provider_key,upstream_card_id,vm_card_id,product_code,bin,last4,upstream_status,cached_available_amount,
        lane,consumed_slots,capacity_state,reconciliation_state,last_balance_sync_at,created_at,updated_at
	      ) VALUES (?,?,?,?,?,NULL,NULL,?,?,NULL,0,'AVAILABLE','READY',?,?,?)`, id, intent.ProviderKey, result.UpstreamCardID,
				strings.TrimSpace(result.VMCardID), strings.TrimSpace(result.ProductCode), "ACTIVE", result.AvailableAmount, at, at, at)
		} else {
			_, err = tx.ExecContext(ctx, `
	      INSERT INTO managed_cards (
	        id,upstream_card_id,vm_card_id,product_code,bin,last4,upstream_status,cached_available_amount,
	        lane,consumed_slots,capacity_state,reconciliation_state,last_balance_sync_at,created_at,updated_at
	      ) VALUES (?,?,?,?,NULL,NULL,?,?,NULL,0,'AVAILABLE','READY',?,?,?)`, id, result.UpstreamCardID,
				strings.TrimSpace(result.VMCardID), strings.TrimSpace(result.ProductCode), "ACTIVE", result.AvailableAmount, at, at, at)
		}
		if err != nil {
			return "", err
		}
		card, err = p.paymentLoadCard(ctx, tx, id)
	} else if err != nil {
		return "", err
	}
	if err != nil {
		return "", err
	}
	target := domain.Tier(reservation.TargetLane)
	if err := paymentAssertCardReservable(card, target); err != nil {
		return "", err
	}
	if card.VMCardID != strings.TrimSpace(result.VMCardID) || card.ProductCode != strings.TrimSpace(result.ProductCode) {
		return "", paymentFailure("OPENED_CARD_RESULT_MISMATCH", "open-card result conflicts with an existing managed card")
	}
	if reservation.CardID.Valid && reservation.CardID.String != card.ID {
		return "", paymentFailure("CARD_RESERVATION_CONFLICT", "new-card plan is already attached to another card")
	}
	if !reservation.CardID.Valid {
		slot, err := paymentFirstFreeSlot(ctx, tx, card, target)
		if err != nil {
			return "", err
		}
		if !card.Lane.Valid || card.Lane.String == "" {
			if _, err := tx.ExecContext(ctx, `UPDATE managed_cards SET lane=?,updated_at=? WHERE id=?`, string(target), store.ISO(now), card.ID); err != nil {
				return "", err
			}
			card.Lane = sql.NullString{String: string(target), Valid: true}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE card_capacity_reservations SET card_id=?,slot_index=? WHERE id=?`, card.ID, slot, reservation.ID); err != nil {
			return "", err
		}
		if err := paymentUpdateCapacityState(ctx, tx, card, target, now); err != nil {
			return "", err
		}
	}
	return card.ID, nil
}

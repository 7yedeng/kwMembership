// Package domain contains the deterministic membership-fulfillment rules shared
// by the Go processor. It deliberately has no database or network dependencies.
package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

type Tier string

const (
	TierFree Tier = "free"
	TierPlus Tier = "plus"
	TierX5   Tier = "x5"
	TierX20  Tier = "x20"

	MembershipStageAllowanceCents int64 = 20
	MembershipStageAllowanceUSD         = 0.20
	CardPriceFreshness                  = 72 * time.Hour
	HistoricalUpgradePairWindow         = 2 * time.Hour
	providerFutureTolerance             = 5 * time.Minute
)

var (
	ErrInvalidTargetTier       = errors.New("targetTier 无效")
	ErrInvalidTransactionRange = errors.New("交易金额范围无效")
	ErrInvalidMoney            = errors.New("金额无效")
)

func MembershipTiers() []Tier {
	return []Tier{TierPlus, TierX5, TierX20}
}

func IsMembershipTier(tier Tier) bool {
	switch tier {
	case TierPlus, TierX5, TierX20:
		return true
	default:
		return false
	}
}

func CapacityForTier(tier Tier) (int, bool) {
	switch tier {
	case TierPlus:
		return 5, true
	case TierX5:
		return 2, true
	case TierX20:
		return 1, true
	default:
		return 0, false
	}
}

type RecognitionRange struct {
	MinCents int64
	MaxCents int64
}

func RecognitionRangeForTier(tier Tier) (RecognitionRange, bool) {
	switch tier {
	case TierPlus:
		return RecognitionRange{MinCents: 1500, MaxCents: 2000}, true
	case TierX5:
		return RecognitionRange{MinCents: 9000, MaxCents: 10000}, true
	case TierX20:
		// Historical PH x20 settlements can be below the quote API's $140 floor.
		return RecognitionRange{MinCents: 12000, MaxCents: 16000}, true
	default:
		return RecognitionRange{}, false
	}
}

const (
	ErrorCodeMembershipContractUnknown = "MEMBERSHIP_CONTRACT_UNKNOWN"
	ErrorCodeCardPriceUnavailable      = "CARD_PRICE_UNAVAILABLE"
	ErrorCodeSessionInvalid            = "SESSION_INVALID"
)

type CodedError struct {
	Message    string
	Code       string
	Retryable  bool
	RetryScope string
}

func (e *CodedError) Error() string { return e.Message }

func ErrorCode(err error) string {
	var coded *CodedError
	if errors.As(err, &coded) {
		return coded.Code
	}
	return ""
}

func contractError(message string) error {
	return &CodedError{
		Message:    message,
		Code:       ErrorCodeMembershipContractUnknown,
		Retryable:  true,
		RetryScope: "global",
	}
}

func sessionError(message string) error {
	return &CodedError{
		Message:    message,
		Code:       ErrorCodeSessionInvalid,
		Retryable:  false,
		RetryScope: "order",
	}
}

func cardPriceUnavailable(tier Tier) error {
	return &CodedError{
		Message:    fmt.Sprintf("卡段 %s 行情不可用", tier),
		Code:       ErrorCodeCardPriceUnavailable,
		Retryable:  true,
		RetryScope: "order",
	}
}

type MembershipObservation struct {
	ProviderCode        int
	ProviderAccountType string
	AccountType         Tier
	Currency            string
	AutoRenew           *bool
	IsOverdue           bool
	IsDelinquent        bool
	ExpireTime          *time.Time
	ExpireTimeValid     bool
	ExpireTimeFuture    bool
	ObservedAt          time.Time
}

func NormalizeMembershipEnvelope(payload []byte, now time.Time) (*MembershipObservation, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(payload, &root); err != nil || root == nil {
		return nil, contractError("会员状态响应不是对象")
	}

	var providerCode float64
	if raw, ok := root["code"]; !ok || json.Unmarshal(raw, &providerCode) != nil {
		return nil, contractError("会员状态响应业务码不是 200")
	}
	if providerCode == 401 {
		return nil, sessionError("会员状态服务拒绝了订单 Session")
	}
	if providerCode != 200 {
		return nil, contractError("会员状态响应业务码不是 200")
	}
	if rawData, ok := root["data"]; ok && strings.TrimSpace(string(rawData)) == "null" {
		var message string
		if json.Unmarshal(root["message"], &message) == nil &&
			strings.TrimSpace(message) == "您还没有订阅,允许您生成订阅链接" {
			autoRenew := false
			return &MembershipObservation{
				ProviderCode: 200,
				AccountType:  TierFree,
				AutoRenew:    &autoRenew,
				ObservedAt:   utcNow(now),
			}, nil
		}
	}

	data, err := requireJSONObject(root["data"], "会员状态响应缺少 data")
	if err != nil {
		return nil, err
	}
	providerType, err := nullableString(data["account_type"], "account_type")
	if err != nil {
		return nil, err
	}
	providerType = strings.ToLower(providerType)
	accountType, ok := mapProviderMembershipType(providerType)
	if !ok {
		return nil, contractError("会员状态 account_type 未知或缺失")
	}

	expireTimeJSON, hasExpireTime := data["expire_time"]
	expiresAtJSON, hasExpiresAt := data["expires_at"]
	expireTimeRaw, err := nullableString(expireTimeJSON, "expire_time")
	if err != nil {
		return nil, err
	}
	expiresAtRaw, err := nullableString(expiresAtJSON, "expires_at")
	if err != nil {
		return nil, err
	}
	if expireTimeRaw == "" {
		expireTimeRaw = expiresAtRaw
	}
	// The provider can retain a paid account_type after expiry; no expiry in
	// either field is its authoritative free-account signal.
	if hasExpireTime && hasExpiresAt && isJSONNull(expireTimeJSON) && isJSONNull(expiresAtJSON) {
		accountType = TierFree
	}
	currency, err := nullableString(data["currency"], "currency")
	if err != nil {
		return nil, err
	}
	autoRenew, err := nullableBoolean(data["auto_renew"], "auto_renew")
	if err != nil {
		return nil, err
	}
	isOverdue, err := requireBoolean(data["is_overdue"], "is_overdue")
	if err != nil {
		return nil, err
	}
	isDelinquent, err := requireBoolean(data["is_delinquent"], "is_delinquent")
	if err != nil {
		return nil, err
	}

	now = utcNow(now)
	parsedExpire, expireValid := parseProviderTimestamp(expireTimeRaw)
	var expireTime *time.Time
	if expireValid {
		parsedExpire = parsedExpire.UTC()
		expireTime = &parsedExpire
	}

	return &MembershipObservation{
		ProviderCode:        200,
		ProviderAccountType: providerType,
		AccountType:         accountType,
		Currency:            strings.ToUpper(currency),
		AutoRenew:           autoRenew,
		IsOverdue:           isOverdue,
		IsDelinquent:        isDelinquent,
		ExpireTime:          expireTime,
		ExpireTimeValid:     expireValid,
		ExpireTimeFuture:    expireValid && parsedExpire.After(now),
		ObservedAt:          now,
	}, nil
}

func requireJSONObject(raw json.RawMessage, message string) (map[string]json.RawMessage, error) {
	var value map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &value) != nil || value == nil {
		return nil, contractError(message)
	}
	return value, nil
}

func nullableString(raw json.RawMessage, field string) (string, error) {
	if isJSONNull(raw) {
		return "", nil
	}
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return "", contractError(fmt.Sprintf("会员状态字段 %s 不是字符串", field))
	}
	return strings.TrimSpace(value), nil
}

func nullableBoolean(raw json.RawMessage, field string) (*bool, error) {
	if isJSONNull(raw) {
		return nil, nil
	}
	value, err := requireBoolean(raw, field)
	if err != nil {
		return nil, err
	}
	return &value, nil
}

func requireBoolean(raw json.RawMessage, field string) (bool, error) {
	var value bool
	if isJSONNull(raw) || json.Unmarshal(raw, &value) != nil {
		return false, contractError(fmt.Sprintf("会员状态字段 %s 不是布尔值", field))
	}
	return value, nil
}

func isJSONNull(raw json.RawMessage) bool {
	return len(raw) == 0 || string(strings.TrimSpace(string(raw))) == "null"
}

func mapProviderMembershipType(providerType string) (Tier, bool) {
	switch providerType {
	case "free":
		return TierFree, true
	case "plus":
		return TierPlus, true
	case "prolite":
		return TierX5, true
	case "pro":
		return TierX20, true
	default:
		return "", false
	}
}

type StartingMembershipClassification string

const (
	StartingMembershipFree       StartingMembershipClassification = "free"
	StartingMembershipUnknown    StartingMembershipClassification = "unknown"
	StartingMembershipDelinquent StartingMembershipClassification = "delinquent"
	StartingMembershipSubscribed StartingMembershipClassification = "subscribed"
)

func ClassifyStartingMembership(observation *MembershipObservation) (StartingMembershipClassification, error) {
	if observation == nil {
		return "", contractError("会员状态观察无效")
	}
	if observation.AccountType == TierFree {
		if !observation.IsOverdue && !observation.IsDelinquent &&
			observation.AutoRenew != nil && !*observation.AutoRenew {
			return StartingMembershipFree, nil
		}
		return StartingMembershipUnknown, nil
	}
	if !IsMembershipTier(observation.AccountType) {
		return StartingMembershipUnknown, nil
	}
	if observation.IsOverdue || observation.IsDelinquent {
		return StartingMembershipDelinquent, nil
	}
	if observation.Currency != "" && observation.ExpireTimeFuture {
		return StartingMembershipSubscribed, nil
	}
	return StartingMembershipUnknown, nil
}

func IsStrictMembershipStageConfirmed(observation *MembershipObservation, expectedTier Tier, requireAutoRenewFalse bool) bool {
	if observation == nil || !IsMembershipTier(expectedTier) {
		return false
	}
	if observation.AccountType != expectedTier || observation.Currency != "PHP" {
		return false
	}
	if observation.IsOverdue || observation.IsDelinquent || !observation.ExpireTimeFuture {
		return false
	}
	return !requireAutoRenewFalse || (observation.AutoRenew != nil && !*observation.AutoRenew)
}

type PriceSignal struct {
	Tier      Tier
	Found     bool
	AmountUSD float64
	Time      string
}

type BudgetStage struct {
	Tier           Tier
	PriceCents     int64
	AllowanceCents int64
	BudgetCents    int64
	ProviderTime   time.Time
}

type MembershipBudget struct {
	TargetTier Tier
	Stages     []BudgetStage
	TotalCents int64
}

func CalculateMembershipBudget(signals []PriceSignal, targetTier Tier, now time.Time) (MembershipBudget, error) {
	if !IsMembershipTier(targetTier) {
		return MembershipBudget{}, ErrInvalidTargetTier
	}
	now = utcNow(now)
	byTier := make(map[Tier]PriceSignal, len(signals))
	for _, signal := range signals {
		byTier[signal.Tier] = signal
	}
	required := []Tier{TierPlus}
	if targetTier != TierPlus {
		required = append(required, targetTier)
	}

	budget := MembershipBudget{TargetTier: targetTier, Stages: make([]BudgetStage, 0, len(required))}
	for _, tier := range required {
		signal, found := byTier[tier]
		providerTime, timeValid := parseSpaceXCardPriceTime(signal.Time, now)
		age := now.Sub(providerTime)
		priceCents, moneyErr := CentsFromUSD(signal.AmountUSD)
		if !found || !signal.Found || math.IsNaN(signal.AmountUSD) || math.IsInf(signal.AmountUSD, 0) || signal.AmountUSD <= 0 || moneyErr != nil ||
			!timeValid || age < -providerFutureTolerance || age > CardPriceFreshness {
			return MembershipBudget{}, cardPriceUnavailable(tier)
		}
		stage := BudgetStage{
			Tier:           tier,
			PriceCents:     priceCents,
			AllowanceCents: MembershipStageAllowanceCents,
			BudgetCents:    priceCents + MembershipStageAllowanceCents,
			ProviderTime:   providerTime.UTC(),
		}
		if budget.TotalCents > math.MaxInt64-stage.BudgetCents {
			return MembershipBudget{}, cardPriceUnavailable(tier)
		}
		budget.Stages = append(budget.Stages, stage)
		budget.TotalCents += stage.BudgetCents
	}
	return budget, nil
}

func ParseSpaceXCardTime(value string) (time.Time, bool) {
	return parseProviderTimestamp(strings.TrimSpace(value))
}

func parseSpaceXCardPriceTime(value string, now time.Time) (time.Time, bool) {
	value = strings.TrimSpace(value)
	parsed, valid := ParseSpaceXCardTime(value)
	if !valid || !parsed.After(now.Add(providerFutureTolerance)) || !strings.HasSuffix(value, "Z") {
		return parsed, valid
	}
	// The price endpoint documents a timezone-less provider-local timestamp.
	// Some responses append Z without converting the UTC+8 wall clock. Correct
	// only an otherwise-impossible future value when that interpretation is
	// plausible; truly current UTC timestamps remain untouched.
	providerLocal, err := time.Parse(time.RFC3339Nano, strings.TrimSuffix(value, "Z")+"+08:00")
	if err != nil || providerLocal.After(now.Add(providerFutureTolerance)) {
		return parsed, valid
	}
	return providerLocal.UTC(), true
}

func CentsFromUSD(value float64) (int64, error) {
	if math.IsNaN(value) || math.IsInf(value, 0) || value > float64(math.MaxInt64)/100-1 || value < float64(math.MinInt64)/100+1 {
		return 0, ErrInvalidMoney
	}
	// Match Math.round((value + Number.EPSILON) * 100), while keeping all
	// subsequent calculations in integer cents.
	epsilon := math.Nextafter(1, 2) - 1
	return int64(math.Floor((value+epsilon)*100 + 0.5)), nil
}

func USDFromCents(cents int64) float64 { return float64(cents) / 100 }

type CardCandidate struct {
	ID                   string
	Eligible             bool
	Lane                 Tier // Empty means unassigned.
	BudgetCents          int64
	AvailableAmountCents int64
}

type RankedCardCandidate struct {
	CardCandidate
	LaneRank              int
	FundingShortfallCents int64
}

func RankMembershipCardCandidates(candidates []CardCandidate, targetTier Tier) ([]RankedCardCandidate, error) {
	if !IsMembershipTier(targetTier) {
		return nil, ErrInvalidTargetTier
	}
	ranked := make([]RankedCardCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if !candidate.Eligible || (candidate.Lane != "" && candidate.Lane != targetTier) {
			continue
		}
		laneRank := 1
		if candidate.Lane == targetTier {
			laneRank = 0
		}
		shortfall := candidate.BudgetCents - candidate.AvailableAmountCents
		if shortfall < 0 {
			shortfall = 0
		}
		ranked = append(ranked, RankedCardCandidate{
			CardCandidate:         candidate,
			LaneRank:              laneRank,
			FundingShortfallCents: shortfall,
		})
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		left, right := ranked[i], ranked[j]
		if left.LaneRank != right.LaneRank {
			return left.LaneRank < right.LaneRank
		}
		if left.FundingShortfallCents != right.FundingShortfallCents {
			return left.FundingShortfallCents < right.FundingShortfallCents
		}
		return left.ID < right.ID
	})
	return ranked, nil
}

const (
	TransactionTypeAuthorization = "Authorization"
	TransactionTypeSettlement    = "Settlement"
	TransactionTypeReversal      = "Reversal"
	TransactionTypeRefund        = "Refund"

	TransactionStatusPending  = "PENDING"
	TransactionStatusDeclined = "DECLINED"
	TransactionStatusComplete = "COMPLETE"
)

type TransactionState struct {
	Type   string
	Status string
}

func SelectCanonicalCardTransactionState(current, candidate TransactionState) TransactionState {
	if candidate.Type == "" {
		return current
	}
	if current.Type == "" {
		return candidate
	}
	typeRank := map[string]int{
		TransactionTypeAuthorization: 1,
		TransactionTypeSettlement:    2,
		TransactionTypeReversal:      3,
		TransactionTypeRefund:        4,
	}
	if typeRank[candidate.Type] > typeRank[current.Type] {
		return candidate
	}
	if typeRank[candidate.Type] < typeRank[current.Type] {
		return current
	}
	statusRank := map[string]int{
		TransactionStatusPending:  1,
		TransactionStatusDeclined: 2,
		TransactionStatusComplete: 3,
	}
	if statusRank[candidate.Status] > statusRank[current.Status] {
		return candidate
	}
	return current
}

type CardTransaction struct {
	AuthID             string
	AuthTime           string
	CreatedAt          string
	AuthAmountCents    *int64
	SettleAmountCents  *int64
	AuthCurrency       string
	SettleCurrency     string
	MerchantNormalized string
	Type               string
	Status             string
}

type PaymentDeltaOptions struct {
	BeforeAuthIDs []string
	Tier          Tier
	MinCents      int64
	MaxCents      int64
	Transactions  []CardTransaction
}

type PaymentMatchOutcome string

const (
	PaymentOutcomeMatched   PaymentMatchOutcome = "matched"
	PaymentOutcomeDeclined  PaymentMatchOutcome = "declined"
	PaymentOutcomeUncertain PaymentMatchOutcome = "uncertain"
)

type PaymentMatchResult struct {
	Outcome     PaymentMatchOutcome
	Transaction *CardTransaction
	Reason      string
	Matches     int
}

func MatchPaymentTransactionDelta(options PaymentDeltaOptions) (PaymentMatchResult, error) {
	if options.MinCents < 0 || options.MaxCents < options.MinCents {
		return PaymentMatchResult{}, ErrInvalidTransactionRange
	}
	minCents, maxCents := options.MinCents, options.MaxCents
	if recognition, ok := RecognitionRangeForTier(options.Tier); ok {
		minCents = min(minCents, recognition.MinCents)
		maxCents = max(maxCents, recognition.MaxCents)
	}

	before := make(map[string]struct{}, len(options.BeforeAuthIDs))
	for _, authID := range options.BeforeAuthIDs {
		before[authID] = struct{}{}
	}
	candidates := make([]CardTransaction, 0)
	for _, transaction := range latestTransactionByAuthID(options.Transactions) {
		if _, existed := before[transaction.AuthID]; existed || !isOpenAITransaction(transaction) {
			continue
		}
		amount, valid := transactionAmount(transaction)
		if valid && amount >= minCents && amount <= maxCents {
			candidates = append(candidates, transaction)
		}
	}

	effective := make([]CardTransaction, 0, len(candidates))
	for _, candidate := range candidates {
		if (candidate.Status == TransactionStatusPending || candidate.Status == TransactionStatusComplete) &&
			candidate.Type != TransactionTypeRefund && candidate.Type != TransactionTypeReversal {
			effective = append(effective, candidate)
		}
	}
	if len(effective) == 1 {
		transaction := effective[0]
		return PaymentMatchResult{Outcome: PaymentOutcomeMatched, Transaction: &transaction}, nil
	}
	if len(effective) > 1 {
		return PaymentMatchResult{Outcome: PaymentOutcomeUncertain, Reason: "MULTIPLE_MATCHES", Matches: len(effective)}, nil
	}

	declined := make([]CardTransaction, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.Status == TransactionStatusDeclined {
			declined = append(declined, candidate)
		}
	}
	if len(declined) == 1 {
		transaction := declined[0]
		return PaymentMatchResult{Outcome: PaymentOutcomeDeclined, Transaction: &transaction}, nil
	}
	reason := "NO_MATCH"
	if len(declined) > 1 {
		reason = "MULTIPLE_DECLINES"
	}
	return PaymentMatchResult{Outcome: PaymentOutcomeUncertain, Reason: reason, Matches: len(declined)}, nil
}

func latestTransactionByAuthID(transactions []CardTransaction) []CardTransaction {
	statusRank := map[string]int{
		TransactionStatusDeclined: 1,
		TransactionStatusPending:  2,
		TransactionStatusComplete: 3,
	}
	latest := make([]CardTransaction, 0, len(transactions))
	indexByID := make(map[string]int, len(transactions))
	for _, transaction := range transactions {
		authID := strings.TrimSpace(transaction.AuthID)
		if authID == "" {
			continue
		}
		transaction.AuthID = authID
		index, exists := indexByID[authID]
		if !exists {
			indexByID[authID] = len(latest)
			latest = append(latest, transaction)
			continue
		}
		if statusRank[transaction.Status] >= statusRank[latest[index].Status] {
			latest[index] = transaction
		}
	}
	return latest
}

func transactionAmount(transaction CardTransaction) (int64, bool) {
	if transaction.SettleAmountCents != nil && *transaction.SettleAmountCents > 0 {
		return *transaction.SettleAmountCents, true
	}
	if transaction.AuthAmountCents != nil {
		return *transaction.AuthAmountCents, true
	}
	return 0, false
}

func transactionCurrency(transaction CardTransaction) string {
	currency := transaction.AuthCurrency
	if transaction.SettleAmountCents != nil && *transaction.SettleAmountCents > 0 {
		currency = transaction.SettleCurrency
	}
	return strings.ToUpper(strings.TrimSpace(currency))
}

func transactionAmountPriority(transaction CardTransaction) int {
	if transaction.SettleAmountCents != nil && *transaction.SettleAmountCents > 0 {
		return 2
	}
	return 1
}

func isOpenAITransaction(transaction CardTransaction) bool {
	return strings.ToUpper(strings.TrimSpace(transaction.MerchantNormalized)) == "OPENAI"
}

type HistoricalFulfillmentState string

const (
	HistoricalStateAvailable          HistoricalFulfillmentState = "AVAILABLE"
	HistoricalStateCapacityFull       HistoricalFulfillmentState = "CAPACITY_FULL"
	HistoricalStateReconciliationHold HistoricalFulfillmentState = "RECONCILIATION_HOLD"
)

type HistoricalFulfillmentResult struct {
	Lane     *Tier
	Consumed int
	State    HistoricalFulfillmentState
	Reason   string
}

type historicalAuthorization struct {
	AuthID               string
	AuthTime             time.Time
	AuthTimeValid        bool
	AmountCents          int64
	AmountValid          bool
	AmountPriority       int
	Currency             string
	AuthorizationPending bool
	SettlementComplete   bool
	RefundSeen           bool
	ReversalSeen         bool
	Tier                 Tier
}

func ClassifyHistoricalCardFulfillments(transactions []CardTransaction, knownLane Tier) HistoricalFulfillmentResult {
	authorizations := foldHistoricalAuthorizations(transactions)
	if !IsMembershipTier(knownLane) {
		knownLane = ""
	}
	for _, authorization := range authorizations {
		if authorization.RefundSeen && authorization.SettlementComplete {
			return historicalHold(0, "REFUNDED_FULFILLMENT")
		}
	}
	if knownLane != TierPlus {
		for _, authorization := range authorizations {
			if authorization.AuthorizationPending && !authorization.SettlementComplete && !authorization.ReversalSeen {
				return historicalHold(0, "PENDING_SETTLEMENT")
			}
		}
	}

	effective := make([]historicalAuthorization, 0, len(authorizations))
	for _, authorization := range authorizations {
		if !authorization.SettlementComplete && !(authorization.AuthorizationPending && !authorization.ReversalSeen) {
			continue
		}
		authorization.Tier = classifyOpenAITier(authorization.AmountCents, authorization.AmountValid)
		if authorization.Tier == "" && knownLane == TierPlus && authorization.AuthorizationPending && !authorization.SettlementComplete &&
			authorization.Currency != "" && authorization.Currency != "USD" {
			authorization.Tier = TierPlus
		}
		effective = append(effective, authorization)
	}
	if len(effective) == 0 {
		return HistoricalFulfillmentResult{Lane: tierPointer(knownLane), State: HistoricalStateAvailable}
	}
	for _, authorization := range effective {
		if authorization.Tier == "" || !authorization.AuthTimeValid {
			return historicalHold(0, "UNCLASSIFIABLE_OPENAI_PAYMENT")
		}
	}

	sort.SliceStable(effective, func(i, j int) bool {
		if !effective[i].AuthTime.Equal(effective[j].AuthTime) {
			return effective[i].AuthTime.Before(effective[j].AuthTime)
		}
		return effective[i].AuthID < effective[j].AuthID
	})
	unpairedPlus := make([]historicalAuthorization, 0)
	finals := make([]historicalAuthorization, 0)
	for _, authorization := range effective {
		if authorization.Tier == TierPlus {
			unpairedPlus = append(unpairedPlus, authorization)
		} else {
			finals = append(finals, authorization)
		}
	}

	pairedTiers := make([]Tier, 0, len(finals))
	for _, final := range finals {
		bestIndex := -1
		bestDelta := time.Duration(math.MaxInt64)
		for index, plus := range unpairedPlus {
			delta := final.AuthTime.Sub(plus.AuthTime)
			if delta >= 0 && delta <= HistoricalUpgradePairWindow && delta < bestDelta {
				bestIndex = index
				bestDelta = delta
			}
		}
		if bestIndex < 0 {
			return historicalHold(0, "UPGRADE_PAIR_MISSING")
		}
		pairedTiers = append(pairedTiers, final.Tier)
		unpairedPlus = append(unpairedPlus[:bestIndex], unpairedPlus[bestIndex+1:]...)
	}

	if len(pairedTiers) > 0 && len(unpairedPlus) > 0 {
		return historicalHold(0, "MIXED_MEMBERSHIP_LANES")
	}
	finalTierSet := make(map[Tier]struct{}, len(pairedTiers))
	for _, tier := range pairedTiers {
		finalTierSet[tier] = struct{}{}
	}
	if len(finalTierSet) > 1 {
		return historicalHold(0, "MIXED_FINAL_TIERS")
	}

	lane := TierPlus
	if len(pairedTiers) > 0 {
		lane = pairedTiers[0]
	}
	if knownLane != "" && lane != knownLane {
		return historicalHold(0, "MIXED_MEMBERSHIP_LANES")
	}
	consumed := len(unpairedPlus)
	if len(pairedTiers) > 0 {
		consumed = len(pairedTiers)
	}
	capacity, _ := CapacityForTier(lane)
	if consumed > capacity {
		return historicalHold(consumed, "CAPACITY_EXCEEDED")
	}
	state := HistoricalStateAvailable
	if consumed == capacity {
		state = HistoricalStateCapacityFull
	}
	return HistoricalFulfillmentResult{Lane: tierPointer(lane), Consumed: consumed, State: state}
}

func foldHistoricalAuthorizations(transactions []CardTransaction) []historicalAuthorization {
	byID := make(map[string]historicalAuthorization, len(transactions))
	for _, event := range transactions {
		authID := strings.TrimSpace(event.AuthID)
		if authID == "" || !isOpenAITransaction(event) {
			continue
		}
		record := byID[authID]
		record.AuthID = authID
		timeValue := event.AuthTime
		if timeValue == "" {
			timeValue = event.CreatedAt
		}
		if parsed, valid := parseProviderTimestamp(timeValue); valid && (!record.AuthTimeValid || parsed.Before(record.AuthTime)) {
			record.AuthTime = parsed.UTC()
			record.AuthTimeValid = true
		}
		amount, amountValid := transactionAmount(event)
		priority := transactionAmountPriority(event)
		if amountValid && amount > 0 && priority >= record.AmountPriority {
			record.AmountCents = amount
			record.AmountValid = true
			record.AmountPriority = priority
			record.Currency = transactionCurrency(event)
		}
		if event.Type == TransactionTypeAuthorization && event.Status == TransactionStatusPending {
			record.AuthorizationPending = true
		}
		if event.Type == TransactionTypeSettlement && event.Status == TransactionStatusComplete {
			record.SettlementComplete = true
		}
		if event.Type == TransactionTypeRefund && (event.Status == TransactionStatusPending || event.Status == TransactionStatusComplete) {
			record.RefundSeen = true
		}
		if event.Type == TransactionTypeReversal && event.Status == TransactionStatusComplete {
			record.ReversalSeen = true
		}
		byID[authID] = record
	}
	authorizations := make([]historicalAuthorization, 0, len(byID))
	for _, authorization := range byID {
		authorizations = append(authorizations, authorization)
	}
	return authorizations
}

func classifyOpenAITier(amountCents int64, valid bool) Tier {
	if !valid {
		return ""
	}
	for _, tier := range MembershipTiers() {
		recognition, _ := RecognitionRangeForTier(tier)
		if amountCents >= recognition.MinCents && amountCents <= recognition.MaxCents {
			return tier
		}
	}
	return ""
}

func historicalHold(consumed int, reason string) HistoricalFulfillmentResult {
	return HistoricalFulfillmentResult{
		Consumed: consumed,
		State:    HistoricalStateReconciliationHold,
		Reason:   reason,
	}
}

func tierPointer(tier Tier) *Tier {
	if tier == "" {
		return nil
	}
	copy := tier
	return &copy
}

func utcNow(now time.Time) time.Time {
	if now.IsZero() {
		now = time.Now()
	}
	return now.UTC()
}

func parseProviderTimestamp(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{
		time.RFC3339Nano,
		"2006-01-02 15:04:05Z07:00",
		"2006-01-02",
	} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed.UTC(), true
		}
	}
	providerLocation := time.FixedZone("UTC+8", 8*60*60)
	for _, layout := range []string{
		"2006-01-02 15:04:05.999999999",
		"2006-01-02T15:04:05.999999999",
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05",
	} {
		if parsed, err := time.ParseInLocation(layout, value, providerLocation); err == nil {
			return parsed.UTC(), true
		}
	}
	return time.Time{}, false
}

package domain

import (
	"errors"
	"math"
	"reflect"
	"testing"
	"time"
)

func TestMembershipTierCapacity(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		tier     Tier
		capacity int
		valid    bool
	}{
		{name: "plus", tier: TierPlus, capacity: 5, valid: true},
		{name: "x5", tier: TierX5, capacity: 2, valid: true},
		{name: "x20", tier: TierX20, capacity: 1, valid: true},
		{name: "free is not a fulfillment tier", tier: TierFree},
		{name: "unknown", tier: "future"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			capacity, valid := CapacityForTier(test.tier)
			if capacity != test.capacity || valid != test.valid {
				t.Fatalf("CapacityForTier(%q) = (%d, %v), want (%d, %v)", test.tier, capacity, valid, test.capacity, test.valid)
			}
			if IsMembershipTier(test.tier) != test.valid {
				t.Fatalf("IsMembershipTier(%q) = %v, want %v", test.tier, IsMembershipTier(test.tier), test.valid)
			}
		})
	}
	if got := MembershipTiers(); !reflect.DeepEqual(got, []Tier{TierPlus, TierX5, TierX20}) {
		t.Fatalf("MembershipTiers() = %v", got)
	}
}

func TestNormalizeMembershipEnvelopeAndConfirmation(t *testing.T) {
	t.Parallel()
	now := mustTime(t, "2026-07-16T00:00:00Z")
	observation, err := NormalizeMembershipEnvelope([]byte(`{
		"code": 200,
		"data": {
			"account_type": " Pro ",
			"currency": "php",
			"auto_renew": true,
			"is_overdue": false,
			"is_delinquent": false,
			"expire_time": "2026-08-15T21:25:18+08:00"
		}
	}`), now)
	if err != nil {
		t.Fatal(err)
	}
	if observation.ProviderCode != 200 || observation.ProviderAccountType != "pro" || observation.AccountType != TierX20 {
		t.Fatalf("unexpected normalized account: %+v", observation)
	}
	if observation.Currency != "PHP" || !observation.ExpireTimeValid || !observation.ExpireTimeFuture {
		t.Fatalf("unexpected strict status fields: %+v", observation)
	}
	if got := observation.ExpireTime.Format(time.RFC3339); got != "2026-08-15T13:25:18Z" {
		t.Fatalf("ExpireTime = %s", got)
	}
	if got := observation.ObservedAt.Format(time.RFC3339); got != "2026-07-16T00:00:00Z" {
		t.Fatalf("ObservedAt = %s", got)
	}
	classification, err := ClassifyStartingMembership(observation)
	if err != nil || classification != StartingMembershipSubscribed {
		t.Fatalf("classification = %q, %v", classification, err)
	}
	if !IsStrictMembershipStageConfirmed(observation, TierX20, false) {
		t.Fatal("expected x20 PHP membership to be confirmed")
	}
	if IsStrictMembershipStageConfirmed(observation, TierX20, true) {
		t.Fatal("auto-renew=true must not satisfy renewal-safe confirmation")
	}
	observation.Currency = "php"
	if IsStrictMembershipStageConfirmed(observation, TierX20, false) {
		t.Fatal("confirmation requires normalized exact PHP currency")
	}
	observation.Currency = "PHP"
	autoRenewFalse := false
	observation.AutoRenew = &autoRenewFalse
	if !IsStrictMembershipStageConfirmed(observation, TierX20, true) {
		t.Fatal("auto-renew=false should satisfy renewal-safe confirmation")
	}
}

func TestNormalizeMembershipEnvelopeTreatsNullExpiriesAsFree(t *testing.T) {
	t.Parallel()
	now := mustTime(t, "2026-07-21T06:02:58Z")
	observation, err := NormalizeMembershipEnvelope([]byte(`{
		"code": 200,
		"data": {
			"account_type": "plus",
			"currency": "USD",
			"auto_renew": false,
			"is_overdue": false,
			"is_delinquent": false,
			"expire_time": null,
			"expires_at": null
		}
	}`), now)
	if err != nil {
		t.Fatal(err)
	}
	if observation.ProviderAccountType != "plus" || observation.AccountType != TierFree {
		t.Fatalf("unexpected normalized account: %+v", observation)
	}
	classification, err := ClassifyStartingMembership(observation)
	if err != nil || classification != StartingMembershipFree {
		t.Fatalf("classification = %q, %v", classification, err)
	}
}

func TestNormalizeMembershipEnvelopeTreatsExplicitNoSubscriptionAsFree(t *testing.T) {
	t.Parallel()
	now := mustTime(t, "2026-07-21T06:36:14Z")
	observation, err := NormalizeMembershipEnvelope([]byte(`{
		"code": 200,
		"data": null,
		"message": "您还没有订阅,允许您生成订阅链接"
	}`), now)
	if err != nil {
		t.Fatal(err)
	}
	if observation.ProviderCode != 200 || observation.AccountType != TierFree {
		t.Fatalf("unexpected normalized account: %+v", observation)
	}
	classification, err := ClassifyStartingMembership(observation)
	if err != nil || classification != StartingMembershipFree {
		t.Fatalf("classification = %q, %v", classification, err)
	}
}

func TestNormalizeMembershipEnvelopeUsesExpiresAtFallback(t *testing.T) {
	t.Parallel()
	now := mustTime(t, "2026-07-21T06:02:58Z")
	observation, err := NormalizeMembershipEnvelope([]byte(`{
		"code": 200,
		"data": {
			"account_type": "plus",
			"currency": "USD",
			"auto_renew": true,
			"is_overdue": false,
			"is_delinquent": false,
			"expire_time": null,
			"expires_at": "2026-08-21 14:17:11"
		}
	}`), now)
	if err != nil {
		t.Fatal(err)
	}
	if observation.AccountType != TierPlus || !observation.ExpireTimeFuture {
		t.Fatalf("unexpected normalized account: %+v", observation)
	}
	classification, err := ClassifyStartingMembership(observation)
	if err != nil || classification != StartingMembershipSubscribed {
		t.Fatalf("classification = %q, %v", classification, err)
	}
}

func TestNormalizeMembershipEnvelopeStrictErrors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		payload string
	}{
		{name: "root array", payload: `[]`},
		{name: "business code", payload: `{"code":0,"data":{}}`},
		{name: "data null", payload: `{"code":200,"data":null}`},
		{name: "unknown plan", payload: validEnvelopeWith(`"account_type":"future"`)},
		{name: "account type number", payload: validEnvelopeWith(`"account_type":20`)},
		{name: "currency number", payload: validEnvelopeWith(`"currency":1`)},
		{name: "auto renew string", payload: validEnvelopeWith(`"auto_renew":"false"`)},
		{name: "overdue missing", payload: `{"code":200,"data":{"account_type":"free","is_delinquent":false}}`},
		{name: "delinquent null", payload: validEnvelopeWith(`"is_delinquent":null`)},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			_, err := NormalizeMembershipEnvelope([]byte(test.payload), mustTime(t, "2026-07-16T00:00:00Z"))
			if err == nil {
				t.Fatal("expected strict contract error")
			}
			if ErrorCode(err) != ErrorCodeMembershipContractUnknown {
				t.Fatalf("error code = %q, error = %v", ErrorCode(err), err)
			}
			var coded *CodedError
			if !errors.As(err, &coded) || !coded.Retryable || coded.RetryScope != "global" {
				t.Fatalf("unexpected retry contract: %#v", err)
			}
		})
	}
	if _, err := ClassifyStartingMembership(nil); ErrorCode(err) != ErrorCodeMembershipContractUnknown {
		t.Fatalf("nil classification error = %v", err)
	}
}

func TestNormalizeMembershipEnvelopeClassifiesRejectedSession(t *testing.T) {
	t.Parallel()
	_, err := NormalizeMembershipEnvelope(
		[]byte(`{"code":401,"message":"token error","data":null}`),
		mustTime(t, "2026-07-16T00:00:00Z"),
	)
	if ErrorCode(err) != ErrorCodeSessionInvalid {
		t.Fatalf("error code = %q, error = %v", ErrorCode(err), err)
	}
	var coded *CodedError
	if !errors.As(err, &coded) || coded.Retryable || coded.RetryScope != "order" {
		t.Fatalf("unexpected rejected-session contract: %#v", err)
	}
}

func TestClassifyStartingMembership(t *testing.T) {
	t.Parallel()
	autoRenewFalse := false
	cases := []struct {
		name string
		in   MembershipObservation
		want StartingMembershipClassification
	}{
		{name: "clean free", in: MembershipObservation{AccountType: TierFree, AutoRenew: &autoRenewFalse}, want: StartingMembershipFree},
		{name: "free with unknown renewal is unknown", in: MembershipObservation{AccountType: TierFree}, want: StartingMembershipUnknown},
		{name: "overdue free is unknown", in: MembershipObservation{AccountType: TierFree, IsOverdue: true}, want: StartingMembershipUnknown},
		{name: "paid delinquent", in: MembershipObservation{AccountType: TierX5, Currency: "PHP", ExpireTimeFuture: true, IsDelinquent: true}, want: StartingMembershipDelinquent},
		{name: "paid active", in: MembershipObservation{AccountType: TierPlus, Currency: "USD", ExpireTimeFuture: true}, want: StartingMembershipSubscribed},
		{name: "paid without currency", in: MembershipObservation{AccountType: TierPlus, ExpireTimeFuture: true}, want: StartingMembershipUnknown},
		{name: "unknown tier", in: MembershipObservation{AccountType: "future"}, want: StartingMembershipUnknown},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			got, err := ClassifyStartingMembership(&test.in)
			if err != nil || got != test.want {
				t.Fatalf("got (%q, %v), want %q", got, err, test.want)
			}
		})
	}
}

func TestCalculateMembershipBudget(t *testing.T) {
	t.Parallel()
	signals := []PriceSignal{
		{Tier: TierPlus, Found: true, AmountUSD: 16.24, Time: "2026-07-16 09:43:25"},
		{Tier: TierX5, Found: true, AmountUSD: 99, Time: "2026-07-15 20:11:03"},
		{Tier: TierX20, Found: true, AmountUSD: 150, Time: "2026-07-16 03:02:55"},
	}
	now := mustTime(t, "2026-07-16T12:00:00+08:00")
	cases := []struct {
		name       string
		tier       Tier
		wantStages []int64
		wantTotal  int64
	}{
		{name: "plus has one allowance", tier: TierPlus, wantStages: []int64{1644}, wantTotal: 1644},
		{name: "x5 has one allowance per stage", tier: TierX5, wantStages: []int64{1644, 9920}, wantTotal: 11564},
		{name: "x20 has one allowance per stage", tier: TierX20, wantStages: []int64{1644, 15020}, wantTotal: 16664},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			budget, err := CalculateMembershipBudget(signals, test.tier, now)
			if err != nil {
				t.Fatal(err)
			}
			gotStages := make([]int64, len(budget.Stages))
			for index, stage := range budget.Stages {
				gotStages[index] = stage.BudgetCents
				if stage.AllowanceCents != 20 || stage.ProviderTime.Location() != time.UTC {
					t.Fatalf("stage did not preserve allowance/UTC: %+v", stage)
				}
			}
			if !reflect.DeepEqual(gotStages, test.wantStages) || budget.TotalCents != test.wantTotal {
				t.Fatalf("stages/total = %v/%d, want %v/%d", gotStages, budget.TotalCents, test.wantStages, test.wantTotal)
			}
		})
	}
	if got, err := CentsFromUSD(16.245); err != nil || got != 1625 {
		t.Fatalf("two-decimal rounding = %d, %v", got, err)
	}
}

func TestCalculateMembershipBudgetRejectsUnavailableSignals(t *testing.T) {
	t.Parallel()
	base := []PriceSignal{
		{Tier: TierPlus, Found: true, AmountUSD: 16.24, Time: "2026-07-16 09:43:25"},
		{Tier: TierX5, Found: true, AmountUSD: 99, Time: "2026-07-15 20:11:03"},
	}
	cases := []struct {
		name    string
		signals []PriceSignal
		now     string
	}{
		{name: "stale", signals: base, now: "2026-07-20T12:00:00+08:00"},
		{name: "missing target", signals: base[:1], now: "2026-07-16T12:00:00+08:00"},
		{name: "not found", signals: replaceSignal(base, TierX5, func(signal *PriceSignal) { signal.Found = false }), now: "2026-07-16T12:00:00+08:00"},
		{name: "non-finite", signals: replaceSignal(base, TierX5, func(signal *PriceSignal) { signal.AmountUSD = math.NaN() }), now: "2026-07-16T12:00:00+08:00"},
		{name: "too far in future", signals: replaceSignal(base, TierPlus, func(signal *PriceSignal) { signal.Time = "2026-07-16 12:05:01" }), now: "2026-07-16T12:00:00+08:00"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			_, err := CalculateMembershipBudget(test.signals, TierX5, mustTime(t, test.now))
			if ErrorCode(err) != ErrorCodeCardPriceUnavailable {
				t.Fatalf("error = %v, code = %q", err, ErrorCode(err))
			}
			var coded *CodedError
			if !errors.As(err, &coded) || !coded.Retryable || coded.RetryScope != "order" {
				t.Fatalf("unexpected retry contract: %#v", err)
			}
		})
	}
	if _, err := CalculateMembershipBudget(base, TierFree, mustTime(t, "2026-07-16T12:00:00+08:00")); !errors.Is(err, ErrInvalidTargetTier) {
		t.Fatalf("invalid target error = %v", err)
	}
}

func TestCalculateMembershipBudgetFreshnessBoundaries(t *testing.T) {
	t.Parallel()
	now := mustTime(t, "2026-07-20T12:00:00+08:00")
	cases := []struct {
		name       string
		signalTime string
	}{
		{name: "exactly 72 hours old", signalTime: "2026-07-17 12:00:00"},
		{name: "exactly five minutes ahead", signalTime: "2026-07-20 12:05:00"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			budget, err := CalculateMembershipBudget([]PriceSignal{{
				Tier: TierPlus, Found: true, AmountUSD: 16.24, Time: test.signalTime,
			}}, TierPlus, now)
			if err != nil || budget.TotalCents != 1644 {
				t.Fatalf("budget = %+v, error = %v", budget, err)
			}
		})
	}
}

func TestCalculateMembershipBudgetCorrectsProviderLocalClockMislabelledAsUTC(t *testing.T) {
	t.Parallel()
	now := mustTime(t, "2026-07-26T09:42:16Z")
	budget, err := CalculateMembershipBudget([]PriceSignal{{
		Tier: TierPlus, Found: true, AmountUSD: 16.05, Time: "2026-07-26T17:40:04Z",
	}}, TierPlus, now)
	if err != nil {
		t.Fatal(err)
	}
	if budget.TotalCents != 1625 || len(budget.Stages) != 1 ||
		budget.Stages[0].ProviderTime.Format(time.RFC3339) != "2026-07-26T09:40:04Z" {
		t.Fatalf("unexpected corrected budget: %+v", budget)
	}
}

func TestRankMembershipCardCandidates(t *testing.T) {
	t.Parallel()
	candidates := []CardCandidate{
		{ID: "unassigned", Eligible: true, BudgetCents: 2000, AvailableAmountCents: 2000},
		{ID: "same-expensive", Eligible: true, Lane: TierPlus, BudgetCents: 2000, AvailableAmountCents: 500},
		{ID: "same-cheap-z", Eligible: true, Lane: TierPlus, BudgetCents: 2000, AvailableAmountCents: 1000},
		{ID: "same-cheap-a", Eligible: true, Lane: TierPlus, BudgetCents: 2000, AvailableAmountCents: 1000},
		{ID: "wrong", Eligible: true, Lane: TierX5, BudgetCents: 2000, AvailableAmountCents: 2000},
		{ID: "ineligible", Lane: TierPlus, BudgetCents: 2000, AvailableAmountCents: 2000},
	}
	ranked, err := RankMembershipCardCandidates(candidates, TierPlus)
	if err != nil {
		t.Fatal(err)
	}
	gotIDs := make([]string, len(ranked))
	gotShortfalls := make([]int64, len(ranked))
	for index, candidate := range ranked {
		gotIDs[index] = candidate.ID
		gotShortfalls[index] = candidate.FundingShortfallCents
	}
	if want := []string{"same-cheap-a", "same-cheap-z", "same-expensive", "unassigned"}; !reflect.DeepEqual(gotIDs, want) {
		t.Fatalf("ranked IDs = %v, want %v", gotIDs, want)
	}
	if want := []int64{1000, 1000, 1500, 0}; !reflect.DeepEqual(gotShortfalls, want) {
		t.Fatalf("shortfalls = %v, want %v", gotShortfalls, want)
	}
	if _, err := RankMembershipCardCandidates(candidates, TierFree); !errors.Is(err, ErrInvalidTargetTier) {
		t.Fatalf("invalid target error = %v", err)
	}
}

func TestSelectCanonicalCardTransactionState(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		current   TransactionState
		candidate TransactionState
		want      TransactionState
	}{
		{name: "empty candidate", current: TransactionState{Type: TransactionTypeAuthorization, Status: TransactionStatusPending}, want: TransactionState{Type: TransactionTypeAuthorization, Status: TransactionStatusPending}},
		{name: "first candidate", candidate: TransactionState{Type: TransactionTypeAuthorization, Status: TransactionStatusPending}, want: TransactionState{Type: TransactionTypeAuthorization, Status: TransactionStatusPending}},
		{name: "settlement beats authorization", current: TransactionState{Type: TransactionTypeAuthorization, Status: TransactionStatusComplete}, candidate: TransactionState{Type: TransactionTypeSettlement, Status: TransactionStatusPending}, want: TransactionState{Type: TransactionTypeSettlement, Status: TransactionStatusPending}},
		{name: "refund beats settlement", current: TransactionState{Type: TransactionTypeSettlement, Status: TransactionStatusComplete}, candidate: TransactionState{Type: TransactionTypeRefund, Status: TransactionStatusPending}, want: TransactionState{Type: TransactionTypeRefund, Status: TransactionStatusPending}},
		{name: "complete beats pending for same type", current: TransactionState{Type: TransactionTypeSettlement, Status: TransactionStatusPending}, candidate: TransactionState{Type: TransactionTypeSettlement, Status: TransactionStatusComplete}, want: TransactionState{Type: TransactionTypeSettlement, Status: TransactionStatusComplete}},
		{name: "same rank retains current", current: TransactionState{Type: TransactionTypeSettlement, Status: TransactionStatusComplete}, candidate: TransactionState{Type: TransactionTypeSettlement, Status: TransactionStatusComplete}, want: TransactionState{Type: TransactionTypeSettlement, Status: TransactionStatusComplete}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := SelectCanonicalCardTransactionState(test.current, test.candidate); got != test.want {
				t.Fatalf("got %+v, want %+v", got, test.want)
			}
		})
	}
}

func TestMatchPaymentTransactionDelta(t *testing.T) {
	t.Parallel()
	base := CardTransaction{
		AuthAmountCents:    cents(1624),
		SettleAmountCents:  cents(0),
		MerchantNormalized: "OPENAI",
		Type:               TransactionTypeAuthorization,
	}
	cases := []struct {
		name            string
		options         PaymentDeltaOptions
		wantOutcome     PaymentMatchOutcome
		wantReason      string
		wantMatches     int
		wantTransaction string
	}{
		{
			name: "exactly one new authorization settles",
			options: PaymentDeltaOptions{BeforeAuthIDs: []string{"old"}, MinCents: 1500, MaxCents: 2000, Transactions: []CardTransaction{
				withTransaction(base, "old", TransactionStatusComplete),
				withTransaction(base, " new ", TransactionStatusPending),
				withSettlement(withTransaction(base, "new", TransactionStatusComplete), 1624),
			}},
			wantOutcome: PaymentOutcomeMatched, wantTransaction: "new",
		},
		{
			name: "multiple matches are uncertain",
			options: PaymentDeltaOptions{MinCents: 1500, MaxCents: 2000, Transactions: []CardTransaction{
				withTransaction(base, "new-1", TransactionStatusPending),
				withTransaction(base, "new-2", TransactionStatusPending),
			}},
			wantOutcome: PaymentOutcomeUncertain, wantReason: "MULTIPLE_MATCHES", wantMatches: 2,
		},
		{
			name: "one decline",
			options: PaymentDeltaOptions{MinCents: 1500, MaxCents: 2000, Transactions: []CardTransaction{
				withTransaction(base, "declined", TransactionStatusDeclined),
			}},
			wantOutcome: PaymentOutcomeDeclined, wantTransaction: "declined",
		},
		{
			name: "x20 recognition includes historical PH settlement",
			options: PaymentDeltaOptions{Tier: TierX20, MinCents: 14000, MaxCents: 16000, Transactions: []CardTransaction{
				withSettlement(withTransaction(base, "x20-ph", TransactionStatusComplete), 13006),
			}},
			wantOutcome: PaymentOutcomeMatched, wantTransaction: "x20-ph",
		},
		{
			name: "latest higher-ranked status wins",
			options: PaymentDeltaOptions{MinCents: 1500, MaxCents: 2000, Transactions: []CardTransaction{
				withTransaction(base, "same", TransactionStatusPending),
				withTransaction(base, "same", TransactionStatusDeclined),
			}},
			wantOutcome: PaymentOutcomeMatched, wantTransaction: "same",
		},
		{
			name: "multiple declines",
			options: PaymentDeltaOptions{MinCents: 1500, MaxCents: 2000, Transactions: []CardTransaction{
				withTransaction(base, "declined-1", TransactionStatusDeclined),
				withTransaction(base, "declined-2", TransactionStatusDeclined),
			}},
			wantOutcome: PaymentOutcomeUncertain, wantReason: "MULTIPLE_DECLINES", wantMatches: 2,
		},
		{
			name: "refund is not effective",
			options: PaymentDeltaOptions{MinCents: 1500, MaxCents: 2000, Transactions: []CardTransaction{
				func() CardTransaction {
					transaction := withTransaction(base, "refund", TransactionStatusComplete)
					transaction.Type = TransactionTypeRefund
					return transaction
				}(),
			}},
			wantOutcome: PaymentOutcomeUncertain, wantReason: "NO_MATCH",
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			got, err := MatchPaymentTransactionDelta(test.options)
			if err != nil {
				t.Fatal(err)
			}
			if got.Outcome != test.wantOutcome || got.Reason != test.wantReason || got.Matches != test.wantMatches {
				t.Fatalf("result = %+v", got)
			}
			if test.wantTransaction == "" {
				if got.Transaction != nil {
					t.Fatalf("unexpected transaction: %+v", got.Transaction)
				}
			} else if got.Transaction == nil || got.Transaction.AuthID != test.wantTransaction {
				t.Fatalf("transaction = %+v, want %q", got.Transaction, test.wantTransaction)
			}
		})
	}
	if _, err := MatchPaymentTransactionDelta(PaymentDeltaOptions{MinCents: 2000, MaxCents: 1000}); !errors.Is(err, ErrInvalidTransactionRange) {
		t.Fatalf("invalid range error = %v", err)
	}
}

func TestClassifyHistoricalCardFulfillments(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		transactions []CardTransaction
		knownLane    Tier
		wantLane     Tier
		wantCount    int
		wantState    HistoricalFulfillmentState
		wantReason   string
	}{
		{name: "empty inventory", wantState: HistoricalStateAvailable},
		{
			name: "two staged x5 upgrades fill capacity",
			transactions: []CardTransaction{
				settled("plus-1", "2026-07-10T00:00:00Z", 1624),
				settled("x5-1", "2026-07-10T01:00:00Z", 9900),
				settled("plus-2", "2026-07-11T00:00:00Z", 1624),
				settled("x5-2", "2026-07-11T01:30:00Z", 9900),
			},
			wantLane: TierX5, wantCount: 2, wantState: HistoricalStateCapacityFull,
		},
		{
			name: "historical PH x20 is recognized",
			transactions: []CardTransaction{
				settled("plus-ph", "2026-07-10T03:36:14+08:00", 1609),
				settled("x20-ph", "2026-07-10T03:38:39+08:00", 13006),
			},
			wantLane: TierX20, wantCount: 1, wantState: HistoricalStateCapacityFull,
		},
		{
			name: "unclassifiable payment",
			transactions: []CardTransaction{
				settled("plus", "2026-07-10T00:00:00Z", 1624),
				settled("unknown", "2026-07-10T01:00:00Z", 11000),
			},
			wantState: HistoricalStateReconciliationHold, wantReason: "UNCLASSIFIABLE_OPENAI_PAYMENT",
		},
		{
			name:         "upgrade pair missing",
			transactions: []CardTransaction{settled("x20", "2026-07-10T01:00:00Z", 15000)},
			wantState:    HistoricalStateReconciliationHold, wantReason: "UPGRADE_PAIR_MISSING",
		},
		{
			name: "extra plus mixes lanes",
			transactions: []CardTransaction{
				settled("plus-1", "2026-07-10T00:00:00Z", 1624),
				settled("x5-1", "2026-07-10T01:00:00Z", 9900),
				settled("plus-extra", "2026-07-11T00:00:00Z", 1624),
			},
			wantState: HistoricalStateReconciliationHold, wantReason: "MIXED_MEMBERSHIP_LANES",
		},
		{
			name: "mixed final tiers hold",
			transactions: []CardTransaction{
				settled("plus-x5", "2026-07-10T00:00:00Z", 1624),
				settled("x5", "2026-07-10T01:00:00Z", 9900),
				settled("plus-x20", "2026-07-10T02:00:00Z", 1624),
				settled("x20", "2026-07-10T03:00:00Z", 15000),
			},
			wantState: HistoricalStateReconciliationHold, wantReason: "MIXED_FINAL_TIERS",
		},
		{
			name: "known lane disagreement holds",
			transactions: []CardTransaction{
				settled("plus", "2026-07-10T00:00:00Z", 1624),
				settled("x5", "2026-07-10T01:00:00Z", 9900),
			},
			knownLane: TierX20,
			wantState: HistoricalStateReconciliationHold, wantReason: "MIXED_MEMBERSHIP_LANES",
		},
		{
			name: "refunded settlement holds",
			transactions: []CardTransaction{
				settled("paid", "2026-07-10T00:00:00Z", 1624),
				transactionEvent("paid", "2026-07-10T00:00:00Z", 1624, TransactionTypeRefund, TransactionStatusComplete),
			},
			wantState: HistoricalStateReconciliationHold, wantReason: "REFUNDED_FULFILLMENT",
		},
		{
			name: "reversed pending authorization is available",
			transactions: []CardTransaction{
				transactionEvent("reversed", "2026-07-10T00:00:00Z", 1624, TransactionTypeAuthorization, TransactionStatusPending),
				transactionEvent("reversed", "2026-07-10T00:00:00Z", 1624, TransactionTypeReversal, TransactionStatusComplete),
			},
			wantState: HistoricalStateAvailable,
		},
		{
			name:         "known plus accepts pending",
			transactions: []CardTransaction{transactionEvent("plus-pending", "2026-07-12T00:00:00Z", 1624, TransactionTypeAuthorization, TransactionStatusPending)},
			knownLane:    TierPlus, wantLane: TierPlus, wantCount: 1, wantState: HistoricalStateAvailable,
		},
		{
			name: "known plus accepts pending PHP authorization",
			transactions: []CardTransaction{func() CardTransaction {
				transaction := transactionEvent("plus-pending-php", "2026-07-12T00:00:00Z", 98214, TransactionTypeAuthorization, TransactionStatusPending)
				transaction.AuthCurrency = "PHP"
				transaction.SettleAmountCents = cents(0)
				transaction.SettleCurrency = "USD"
				return transaction
			}()},
			knownLane: TierPlus, wantLane: TierPlus, wantCount: 1, wantState: HistoricalStateAvailable,
		},
		{
			name: "settlement amount wins despite reverse event order",
			transactions: []CardTransaction{
				func() CardTransaction {
					transaction := settled("plus-reverse-order", "2026-07-12T00:00:00Z", 98214)
					transaction.AuthCurrency = "PHP"
					transaction.SettleAmountCents = cents(1609)
					transaction.SettleCurrency = "USD"
					return transaction
				}(),
				func() CardTransaction {
					transaction := transactionEvent("plus-reverse-order", "2026-07-12T00:00:00Z", 98214, TransactionTypeAuthorization, TransactionStatusPending)
					transaction.AuthCurrency = "PHP"
					transaction.SettleAmountCents = cents(0)
					return transaction
				}(),
			},
			wantLane: TierPlus, wantCount: 1, wantState: HistoricalStateAvailable,
		},
		{
			name:         "unknown pending lane holds",
			transactions: []CardTransaction{transactionEvent("unknown", "2026-07-12T00:00:00Z", 1624, TransactionTypeAuthorization, TransactionStatusPending)},
			wantState:    HistoricalStateReconciliationHold, wantReason: "PENDING_SETTLEMENT",
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			got := ClassifyHistoricalCardFulfillments(test.transactions, test.knownLane)
			gotLane := Tier("")
			if got.Lane != nil {
				gotLane = *got.Lane
			}
			if gotLane != test.wantLane || got.Consumed != test.wantCount || got.State != test.wantState || got.Reason != test.wantReason {
				t.Fatalf("got %+v (lane %q), want lane=%q count=%d state=%q reason=%q", got, gotLane, test.wantLane, test.wantCount, test.wantState, test.wantReason)
			}
		})
	}
}

func TestHistoricalCapacityExceeded(t *testing.T) {
	t.Parallel()
	transactions := make([]CardTransaction, 6)
	for index := range transactions {
		transactions[index] = transactionEvent(
			"plus-over-capacity-"+string(rune('0'+index)),
			time.Date(2026, 7, 10+index, 0, 0, 0, 0, time.UTC).Format(time.RFC3339),
			1624,
			TransactionTypeAuthorization,
			TransactionStatusPending,
		)
	}
	got := ClassifyHistoricalCardFulfillments(transactions, TierPlus)
	if got.State != HistoricalStateReconciliationHold || got.Reason != "CAPACITY_EXCEEDED" || got.Consumed != 6 {
		t.Fatalf("result = %+v", got)
	}
}

func validEnvelopeWith(replacement string) string {
	fields := map[string]string{
		"account_type":  `"free"`,
		"currency":      `null`,
		"auto_renew":    `null`,
		"is_overdue":    `false`,
		"is_delinquent": `false`,
		"expire_time":   `null`,
	}
	for key := range fields {
		prefix := `"` + key + `":`
		if len(replacement) >= len(prefix) && replacement[:len(prefix)] == prefix {
			fields[key] = replacement[len(prefix):]
		}
	}
	return `{"code":200,"data":{"account_type":` + fields["account_type"] +
		`,"currency":` + fields["currency"] +
		`,"auto_renew":` + fields["auto_renew"] +
		`,"is_overdue":` + fields["is_overdue"] +
		`,"is_delinquent":` + fields["is_delinquent"] +
		`,"expire_time":` + fields["expire_time"] + `}}`
}

func replaceSignal(signals []PriceSignal, tier Tier, change func(*PriceSignal)) []PriceSignal {
	copy := append([]PriceSignal(nil), signals...)
	for index := range copy {
		if copy[index].Tier == tier {
			change(&copy[index])
		}
	}
	return copy
}

func withTransaction(base CardTransaction, authID, status string) CardTransaction {
	base.AuthID = authID
	base.Status = status
	return base
}

func withSettlement(transaction CardTransaction, amount int64) CardTransaction {
	transaction.Type = TransactionTypeSettlement
	transaction.SettleAmountCents = cents(amount)
	return transaction
}

func settled(authID, authTime string, amount int64) CardTransaction {
	return transactionEvent(authID, authTime, amount, TransactionTypeSettlement, TransactionStatusComplete)
}

func transactionEvent(authID, authTime string, amount int64, transactionType, status string) CardTransaction {
	return CardTransaction{
		AuthID:             authID,
		AuthTime:           authTime,
		AuthAmountCents:    cents(amount),
		SettleAmountCents:  cents(amount),
		MerchantNormalized: "OPENAI",
		Type:               transactionType,
		Status:             status,
	}
}

func cents(value int64) *int64 { return &value }

func mustTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatalf("parse time %q: %v", value, err)
	}
	return parsed
}

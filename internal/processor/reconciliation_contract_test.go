package processor

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"kwmembership/internal/checkout"
	"kwmembership/internal/domain"
	"kwmembership/internal/provider"
	"kwmembership/internal/secure"
	"kwmembership/internal/store"
)

// These are local state-machine contracts. The observations and card events are
// synthetic; no test connects to an account, card platform or payment service.
func rcContractProcessor(t *testing.T, id string) (*Processor, *store.Store, time.Time) {
	t.Helper()
	repository, err := store.Open(filepath.Join(t.TempDir(), "reconciliation.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { repository.Close() })
	rcTestSchema(t, context.Background(), repository)
	now := time.Date(2026, 7, 16, 0, 0, 0, 0, time.UTC)
	rcTestSeedStage(t, context.Background(), repository, id, "plus", "plus", now)
	return &Processor{store: repository, now: func() time.Time { return now }}, repository, now
}

func rcContractObservation(now time.Time, paid bool) *domain.MembershipObservation {
	if !paid {
		return &domain.MembershipObservation{ProviderCode: 200, AccountType: domain.TierFree, ObservedAt: now}
	}
	autoRenew, expires := true, now.Add(30*24*time.Hour)
	return &domain.MembershipObservation{
		ProviderCode: 200, AccountType: domain.TierPlus, Currency: "PHP", AutoRenew: &autoRenew,
		ExpireTime: &expires, ExpireTimeValid: true, ExpireTimeFuture: true, ObservedAt: now,
	}
}

func rcContractAuthorization(now time.Time, amount float64) provider.Transaction {
	return provider.Transaction{
		AuthID: "new-payment", AuthTime: store.ISO(now), AuthAmount: amount, AuthCurrency: "USD",
		Type: domain.TransactionTypeAuthorization, Status: domain.TransactionStatusPending, MerchantNormalized: "OPENAI",
	}
}

func rcContractState(t *testing.T, repository *store.Store, id, want string) {
	t.Helper()
	var got string
	if err := repository.DB().QueryRow(`SELECT state FROM membership_fulfillments WHERE id=?`, id).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("fulfillment state=%s want=%s", got, want)
	}
}

func TestReconciliationDelayedEntitlementUsesDurablePaymentAfterRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p, repository, now := rcContractProcessor(t, "delayed-entitlement")
	first := now.Add(time.Minute)
	event := rcContractAuthorization(first, 20)
	if err := p.rcApplyPaymentEvidence(ctx, "delayed-entitlement", "plus", []provider.Transaction{event}, rcContractObservation(first, false), first); err != nil {
		t.Fatal(err)
	}
	rcContractState(t, repository, "delayed-entitlement", "PLUS_RECONCILING")
	// A fresh Processor has no in-memory payment evidence. The card platform's
	// next successful read may omit an earlier event while entitlement catches up.
	restarted := &Processor{store: repository}
	later := now.Add(2 * time.Minute)
	if err := restarted.rcApplyPaymentEvidence(ctx, "delayed-entitlement", "plus", nil, rcContractObservation(later, true), later); err != nil {
		t.Fatal(err)
	}
	rcContractState(t, repository, "delayed-entitlement", "FINAL_TIER_CONFIRMED")
	var authID, settlement string
	if err := repository.DB().QueryRow(`SELECT matched_auth_id,settlement_state FROM membership_payment_stages WHERE fulfillment_id='delayed-entitlement'`).Scan(&authID, &settlement); err != nil {
		t.Fatal(err)
	}
	if authID != event.AuthID || settlement != domain.TransactionStatusPending {
		t.Fatalf("payment proof was lost or authorization was called settlement: auth=%s settlement=%s", authID, settlement)
	}
	observation := rcContractObservation(later, true)
	*observation.AutoRenew = false
	if err := restarted.rcApplyRenewalObservation(ctx, "delayed-entitlement", observation, later); err != nil {
		t.Fatal(err)
	}
	rcContractState(t, repository, "delayed-entitlement", "COMPLETED")
	if due, found, err := restarted.rcDueFulfillment(ctx, later); err != nil || !found || due.ID != "delayed-entitlement" {
		t.Fatalf("completed entitlement must still reconcile pending settlement: found=%v err=%v", found, err)
	}
	event.Type, event.Status = domain.TransactionTypeSettlement, domain.TransactionStatusComplete
	event.SettleAmount, event.SettleCurrency = 20, "USD"
	if reviewed, err := restarted.rcApplySettlementEvidence(ctx, "delayed-entitlement", "plus", []provider.Transaction{event}, later); err != nil || reviewed {
		t.Fatalf("normal settlement requires no intervention: reviewed=%v err=%v", reviewed, err)
	}
	if _, found, err := restarted.rcDueFulfillment(ctx, later); err != nil || found {
		t.Fatalf("settled completed fulfillment remained due: found=%v err=%v", found, err)
	}
}

func TestReconciliationKnownPendingCannotBecomeNoPaymentOnEmptyPage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p, repository, now := rcContractProcessor(t, "pending-small")
	first := now.Add(time.Minute)
	// Even an authorization below the subscription price prevents an assertion
	// that no pending authorization or effective OpenAI transaction exists.
	event := rcContractAuthorization(first, 1)
	if err := p.rcApplyPaymentEvidence(ctx, "pending-small", "plus", []provider.Transaction{event}, rcContractObservation(first, false), first); err != nil {
		t.Fatal(err)
	}
	later := now.Add(24 * time.Hour)
	if err := (&Processor{store: repository}).rcApplyPaymentEvidence(ctx, "pending-small", "plus", nil, rcContractObservation(later, false), later); err != nil {
		t.Fatal(err)
	}
	var checks, assertedNoPayment, assertedNoPending int
	if err := repository.DB().QueryRow(`SELECT COUNT(*),COALESCE(SUM(no_effective_transaction),0),COALESCE(SUM(no_pending_authorization),0)
		FROM membership_no_payment_checks WHERE fulfillment_id='pending-small'`).Scan(&checks, &assertedNoPayment, &assertedNoPending); err != nil {
		t.Fatal(err)
	}
	if checks != 3 || assertedNoPayment != 0 || assertedNoPending != 0 {
		t.Fatalf("empty page erased durable payment evidence: checks=%d noPayment=%d noPending=%d", checks, assertedNoPayment, assertedNoPending)
	}
	rcContractState(t, repository, "pending-small", "PAYMENT_OUTCOME_UNCERTAIN")
}

func TestReconciliationNeverUsesAnotherCardOrPreSubmitHistory(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p, repository, now := rcContractProcessor(t, "isolated")
	rcTestSeedStage(t, ctx, repository, "other-platform-card", "plus", "plus", now)
	for _, record := range []struct {
		cardID string
		at     time.Time
	}{
		{"card-isolated", now.Add(-time.Hour)},
		{"card-other-platform-card", now.Add(time.Minute)},
	} {
		if err := repository.WithImmediate(ctx, func(tx *sql.Tx) error {
			_, err := p.rcPersistManagedTransactions(ctx, tx, record.cardID, []provider.Transaction{rcContractAuthorization(record.at, 20)}, record.at)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	later := now.Add(2 * time.Minute)
	if err := p.rcApplyPaymentEvidence(ctx, "isolated", "plus", nil, rcContractObservation(later, true), later); err != nil {
		t.Fatal(err)
	}
	rcContractState(t, repository, "isolated", "PAYMENT_OUTCOME_UNCERTAIN")
	var matched sql.NullString
	if err := repository.DB().QueryRow(`SELECT matched_auth_id FROM membership_payment_stages WHERE fulfillment_id='isolated'`).Scan(&matched); err != nil || matched.Valid {
		t.Fatalf("foreign or pre-submit evidence matched: matched=%v err=%v", matched, err)
	}
}

func TestReconciliationRefundOrReversalCannotBeResurrectedByStalePage(t *testing.T) {
	t.Parallel()
	for _, terminalType := range []string{domain.TransactionTypeRefund, domain.TransactionTypeReversal} {
		t.Run(terminalType, func(t *testing.T) {
			ctx := context.Background()
			p, repository, now := rcContractProcessor(t, "reversed")
			first := now.Add(time.Minute)
			event := rcContractAuthorization(first, 20)
			if err := p.rcApplyPaymentEvidence(ctx, "reversed", "plus", []provider.Transaction{event}, rcContractObservation(first, false), first); err != nil {
				t.Fatal(err)
			}
			terminal := event
			terminal.Type, terminal.Status = terminalType, domain.TransactionStatusComplete
			if err := p.rcApplyPaymentEvidence(ctx, "reversed", "plus", []provider.Transaction{terminal}, rcContractObservation(first, false), first); err != nil {
				t.Fatal(err)
			}
			later := now.Add(2 * time.Minute)
			for _, stale := range [][]provider.Transaction{nil, {event}} {
				if err := (&Processor{store: repository}).rcApplyPaymentEvidence(ctx, "reversed", "plus", stale, rcContractObservation(later, true), later); err != nil {
					t.Fatal(err)
				}
				rcContractState(t, repository, "reversed", "PAYMENT_OUTCOME_UNCERTAIN")
			}
		})
	}
}

func TestReconciliationCurrencyCannotBeBorrowedFromEarlierAmount(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p, repository, now := rcContractProcessor(t, "unknown-currency")
	first := now.Add(time.Minute)
	event := rcContractAuthorization(first, 1)
	if err := p.rcApplyPaymentEvidence(ctx, "unknown-currency", "plus", []provider.Transaction{event}, rcContractObservation(first, false), first); err != nil {
		t.Fatal(err)
	}
	event.AuthAmount, event.AuthCurrency = 20, ""
	later := now.Add(2 * time.Minute)
	if err := p.rcApplyPaymentEvidence(ctx, "unknown-currency", "plus", []provider.Transaction{event}, rcContractObservation(later, true), later); err != nil {
		t.Fatal(err)
	}
	rcContractState(t, repository, "unknown-currency", "PAYMENT_OUTCOME_UNCERTAIN")
}

func TestReconciliationEntitlementAloneWaitsForDelayedCardTransaction(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p, repository, now := rcContractProcessor(t, "delayed-card")
	first := now.Add(time.Minute)
	if err := p.rcApplyPaymentEvidence(ctx, "delayed-card", "plus", nil, rcContractObservation(first, true), first); err != nil {
		t.Fatal(err)
	}
	rcContractState(t, repository, "delayed-card", "PAYMENT_OUTCOME_UNCERTAIN")
	if processed, err := p.tickCheckout(ctx); err != nil || processed {
		t.Fatalf("missing card evidence restarted checkout: processed=%v err=%v", processed, err)
	}
	later := now.Add(6 * time.Minute)
	if err := p.rcApplyPaymentEvidence(ctx, "delayed-card", "plus", []provider.Transaction{rcContractAuthorization(later, 20)}, rcContractObservation(later, true), later); err != nil {
		t.Fatal(err)
	}
	rcContractState(t, repository, "delayed-card", "FINAL_TIER_CONFIRMED")
}

func TestReconciliationCompletedAuthorizationIsNotSettledPayment(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p, repository, now := rcContractProcessor(t, "authorization-complete")
	event := rcContractAuthorization(now, 20)
	event.Status = domain.TransactionStatusComplete
	if err := p.rcApplyPaymentEvidence(ctx, "authorization-complete", "plus", []provider.Transaction{event}, rcContractObservation(now, true), now); err != nil {
		t.Fatal(err)
	}
	observation := rcContractObservation(now, true)
	*observation.AutoRenew = false
	if err := p.rcApplyRenewalObservation(ctx, "authorization-complete", observation, now); err != nil {
		t.Fatal(err)
	}
	if reviewed, err := p.rcApplySettlementEvidence(ctx, "authorization-complete", "plus", []provider.Transaction{event}, now); err != nil || reviewed {
		t.Fatalf("unchanged authorization should wait for settlement: reviewed=%v err=%v", reviewed, err)
	}
	var settlement string
	if err := repository.DB().QueryRow(`SELECT settlement_state FROM membership_payment_stages WHERE fulfillment_id='authorization-complete'`).Scan(&settlement); err != nil {
		t.Fatal(err)
	}
	if settlement != domain.TransactionStatusPending {
		t.Fatalf("completed authorization was mislabeled settled: settlement=%s", settlement)
	}
}

func TestReconciliationSettlementNeedsAmountAndCurrencyEvidence(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, currency string
		amount         float64
	}{
		{"missing currency", "", 20},
		{"different currency", "EGP", 20},
		{"different amount", "USD", 90},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			p, repository, now := rcContractProcessor(t, "changed-settlement")
			event := rcContractAuthorization(now, 20)
			if err := p.rcApplyPaymentEvidence(ctx, "changed-settlement", "plus", []provider.Transaction{event}, rcContractObservation(now, true), now); err != nil {
				t.Fatal(err)
			}
			observation := rcContractObservation(now, true)
			*observation.AutoRenew = false
			if err := p.rcApplyRenewalObservation(ctx, "changed-settlement", observation, now); err != nil {
				t.Fatal(err)
			}
			event.Type, event.Status = domain.TransactionTypeSettlement, domain.TransactionStatusComplete
			event.SettleAmount, event.SettleCurrency = tc.amount, tc.currency
			if reviewed, err := p.rcApplySettlementEvidence(ctx, "changed-settlement", "plus", []provider.Transaction{event}, now); err != nil || !reviewed {
				t.Fatalf("changed settlement was not held for review: reviewed=%v err=%v", reviewed, err)
			}
			rcContractState(t, repository, "changed-settlement", "PAYMENT_OUTCOME_UNCERTAIN")
		})
	}
}

func TestReconciliationCompleteSettlementCannotBorrowAuthorizationAmount(t *testing.T) {
	t.Parallel()
	for _, amount := range []float64{0, -1} {
		for _, refresh := range []bool{false, true} {
			t.Run(fmt.Sprintf("settle_%g_refresh_%v", amount, refresh), func(t *testing.T) {
				ctx := context.Background()
				p, repository, now := rcContractProcessor(t, "settlement-amount")
				event := rcContractAuthorization(now, 20)
				if refresh {
					if err := p.rcApplyPaymentEvidence(ctx, "settlement-amount", "plus", []provider.Transaction{event}, rcContractObservation(now, true), now); err != nil {
						t.Fatal(err)
					}
				}
				event.Type, event.Status = domain.TransactionTypeSettlement, domain.TransactionStatusComplete
				event.SettleAmount, event.SettleCurrency = amount, ""
				var applyErr error
				if refresh {
					_, applyErr = p.rcApplySettlementEvidence(ctx, "settlement-amount", "plus", []provider.Transaction{event}, now)
				} else {
					applyErr = p.rcApplyPaymentEvidence(ctx, "settlement-amount", "plus", []provider.Transaction{event}, rcContractObservation(now, true), now)
				}
				if applyErr != nil && !(amount < 0 && !refresh && errorCode(applyErr) == "CARD_TRANSACTION_LIST_INVALID") {
					t.Fatalf("unexpected failure instead of an evidence hold: %v", applyErr)
				}
				var settlement sql.NullString
				if err := repository.DB().QueryRow(`SELECT settlement_state FROM membership_payment_stages WHERE fulfillment_id='settlement-amount'`).Scan(&settlement); err != nil {
					t.Fatal(err)
				}
				if settlement.String == domain.TransactionStatusComplete {
					t.Fatalf("missing/nonpositive settlement amount borrowed the authorization: amount=%g refresh=%v", amount, refresh)
				}
			})
		}
	}
}

func TestReconciliationSamePageCannotBorrowCurrencyFromAnotherAmount(t *testing.T) {
	t.Parallel()
	for _, settled := range []bool{false, true} {
		t.Run(fmt.Sprintf("settlement_%v", settled), func(t *testing.T) {
			ctx := context.Background()
			p, repository, now := rcContractProcessor(t, "same-page-currency")
			small, unknown := rcContractAuthorization(now, 1), rcContractAuthorization(now, 20)
			unknown.AuthCurrency = ""
			if settled {
				small.Type, unknown.Type = domain.TransactionTypeSettlement, domain.TransactionTypeSettlement
				small.Status, unknown.Status = domain.TransactionStatusComplete, domain.TransactionStatusComplete
				small.SettleAmount, small.SettleCurrency = 1, "USD"
				unknown.SettleAmount, unknown.SettleCurrency = 20, ""
			}
			if err := p.rcApplyPaymentEvidence(ctx, "same-page-currency", "plus", []provider.Transaction{small, unknown}, rcContractObservation(now, true), now); err != nil {
				t.Fatal(err)
			}
			rcContractState(t, repository, "same-page-currency", "PAYMENT_OUTCOME_UNCERTAIN")
		})
	}
}

func TestReconciliationSubmissionAcknowledgementAndTimeoutNeverResubmit(t *testing.T) {
	t.Parallel()
	for _, outcome := range []string{"acknowledged", "submit timeout", "executor lost"} {
		t.Run(outcome, func(t *testing.T) {
			ctx := context.Background()
			p, repository, now := rcContractProcessor(t, "submit-result")
			if _, err := repository.DB().Exec(`UPDATE membership_fulfillments SET state='PLUS_SUBMIT_PERMITTED' WHERE id='submit-result'`); err != nil {
				t.Fatal(err)
			}
			execution := checkoutExecution{
				Fulfillment: Fulfillment{ID: "submit-result"}, Stage: checkoutStage{StageKey: "plus"}, AttemptNo: 1,
			}
			action := checkout.Action{Kind: checkout.ActionSubmit, Stage: "plus"}
			permit := checkout.Permit{ID: "permit-submit-result"}
			wantState := "PLUS_SUBMIT_PERMITTED"
			switch outcome {
			case "acknowledged":
				result, err := (&checkoutGuard{processor: p, execution: execution}).AfterAction(ctx, action, permit)
				if err != nil || result.Continue {
					t.Fatalf("submission acknowledgement authorized another action: continue=%v err=%v", result.Continue, err)
				}
			case "submit timeout":
				cause := &checkout.Error{ErrorCode: "PAYMENT_SUBMIT_TIMEOUT", Uncertain: true, Action: &action, Permit: &permit}
				if err := p.checkoutMarkUncertain(ctx, execution, cause, now); err != nil {
					t.Fatal(err)
				}
				wantState = "PAYMENT_OUTCOME_UNCERTAIN"
			case "executor lost":
				if err := p.checkoutRecordExecutionFailure(ctx, execution, context.DeadlineExceeded, now); !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("lost executor outcome was not preserved: %v", err)
				}
				wantState = "EXECUTOR_OUTCOME_UNCERTAIN"
			}
			rcContractState(t, repository, "submit-result", wantState)
			for _, restartAt := range []time.Time{now.Add(time.Minute), now.Add(24 * time.Hour)} {
				restarted := &Processor{store: repository, now: func() time.Time { return restartAt },
					executor: checkoutExecutorFunc(func(context.Context, checkout.Request) (checkout.Result, error) {
						t.Fatal("an acknowledged or unknown submission was executed again")
						return checkout.Result{}, nil
					})}
				if processed, err := restarted.tickCheckout(ctx); err != nil || processed {
					t.Fatalf("unsafe checkout retry: processed=%v err=%v", processed, err)
				}
			}
			var permits, snapshots int
			if err := repository.DB().QueryRow(`SELECT COUNT(*) FROM membership_action_permits WHERE fulfillment_id='submit-result'`).Scan(&permits); err != nil {
				t.Fatal(err)
			}
			if err := repository.DB().QueryRow(`SELECT COUNT(*) FROM membership_action_auth_snapshots WHERE permit_id='permit-submit-result' AND auth_id='old-auth'`).Scan(&snapshots); err != nil {
				t.Fatal(err)
			}
			var stageState string
			if err := repository.DB().QueryRow(`SELECT state FROM membership_payment_stages WHERE fulfillment_id='submit-result'`).Scan(&stageState); err != nil {
				t.Fatal(err)
			}
			if permits != 1 || snapshots != 1 || stageState == "confirmed" {
				t.Fatalf("submission result altered the durable boundary or pretended success: permits=%d snapshots=%d stage=%s", permits, snapshots, stageState)
			}
		})
	}
}

func TestReconciliationWorkerTakeoverRejectsStalePaymentProjection(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	stale, repository, now := rcContractProcessor(t, "takeover")
	if err := repository.EnsureLeaseTable(ctx, now); err != nil {
		t.Fatal(err)
	}
	lease, err := repository.AcquireLease(ctx, "go", "synthetic-old-worker", "test", now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	stale.lease, stale.requireLeaseFence = lease, true
	if _, err := repository.AcquireLease(ctx, "go", "synthetic-new-worker", "test", now, time.Minute); !errors.Is(err, store.ErrLeaseHeld) {
		t.Fatalf("two live workers acquired the same processor lease: %v", err)
	}
	later := now.Add(2 * time.Minute)
	newLease, err := repository.AcquireLease(ctx, "go", "synthetic-new-worker", "test", later, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	stale.now = func() time.Time { return later }
	events, observation := []provider.Transaction{rcContractAuthorization(later, 20)}, rcContractObservation(later, true)
	if err := stale.rcApplyPaymentEvidence(ctx, "takeover", "plus", events, observation, later); !errors.Is(err, store.ErrLeaseLost) {
		t.Fatalf("stale worker persisted payment evidence after takeover: %v", err)
	}
	var observations int
	if err := repository.DB().QueryRow(`SELECT COUNT(*) FROM membership_observations`).Scan(&observations); err != nil || observations != 0 {
		t.Fatalf("stale observation persisted: count=%d err=%v", observations, err)
	}
	replacement := &Processor{store: repository, lease: newLease, requireLeaseFence: true, now: func() time.Time { return later }}
	if err := replacement.rcApplyPaymentEvidence(ctx, "takeover", "plus", events, observation, later); err != nil {
		t.Fatal(err)
	}
	rcContractState(t, repository, "takeover", "FINAL_TIER_CONFIRMED")
}

func TestReconciliationCancellationTimeoutReobservesBeforeAnyRetry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p, repository, now := rcContractProcessor(t, "renewal-timeout")
	event := rcContractAuthorization(now, 20)
	if err := p.rcApplyPaymentEvidence(ctx, "renewal-timeout", "plus", []provider.Transaction{event}, rcContractObservation(now, true), now); err != nil {
		t.Fatal(err)
	}
	if err := repository.EnsureLeaseTable(ctx, now); err != nil {
		t.Fatal(err)
	}
	lease, err := repository.AcquireLease(ctx, "go", "synthetic-renewal-worker", "test", now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	crypt, err := secure.NewDecrypter("synthetic-renewal-contract-secret")
	if err != nil {
		t.Fatal(err)
	}
	session, err := crypt.Encrypt(`{"user":{"email":"buyer@example.com"},"token":"synthetic-session"}`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.DB().Exec(`INSERT INTO redeem_orders VALUES ('order-renewal-timeout',?)`, session); err != nil {
		t.Fatal(err)
	}
	currentTime, renewed := now, true
	observeCalls, cancelCalls := 0, 0
	p.lease, p.decrypter = lease, crypt
	p.now = func() time.Time { return currentTime }
	p.config.MaintenancePath = filepath.Join(t.TempDir(), "maintenance")
	p.accounts = &accountServiceStub{
		observe: func(context.Context, json.RawMessage) (AccountObservation, error) {
			observeCalls++
			observation := accountTestObservation(currentTime)
			observation.AutoRenew = accountBool(renewed)
			return observation, nil
		},
		cancel: func(context.Context, json.RawMessage) error {
			cancelCalls++
			renewed = false // The remote effect may happen despite a lost response.
			return context.DeadlineExceeded
		},
	}
	if processed, err := p.tickReconciliation(ctx); !processed || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancellation timeout was not retained: processed=%v err=%v", processed, err)
	}
	rcContractState(t, repository, "renewal-timeout", "FINAL_TIER_CONFIRMED")
	currentTime = now.Add(rcFailureRetry)
	if processed, err := p.tickReconciliation(ctx); !processed || err != nil {
		t.Fatalf("post-timeout observation failed: processed=%v err=%v", processed, err)
	}
	rcContractState(t, repository, "renewal-timeout", "COMPLETED")
	if observeCalls != 2 || cancelCalls != 1 {
		t.Fatalf("cancellation was blindly retried instead of observed: observe=%d cancel=%d", observeCalls, cancelCalls)
	}
}

func TestReconciliationSettlementUsesBoundConfigurationAndLegacyFallback(t *testing.T) {
	t.Parallel()
	for _, configurationKey := range []string{"", "cards-a", "cards-b"} {
		t.Run("configuration_"+configurationKey, func(t *testing.T) {
			ctx := context.Background()
			p, repository, now := rcContractProcessor(t, "settlement-route")
			crypt, err := secure.NewDecrypter("synthetic-settlement-route-secret")
			if err != nil {
				t.Fatal(err)
			}
			p.decrypter = crypt
			wantHost, wantKey := "spacexcard.com", "synthetic-legacy"
			if configurationKey == "" {
				encrypted, err := crypt.Encrypt(wantKey)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := repository.DB().Exec(`INSERT INTO membership_fulfillment_settings
					(id,spacexcard_app_id,spacexcard_app_secret_encrypted) VALUES ('default','synthetic-app',?)`, encrypted); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := repository.DB().Exec(`ALTER TABLE managed_cards ADD COLUMN provider_key TEXT;
					CREATE TABLE membership_card_platforms (key TEXT PRIMARY KEY,kind TEXT,base_url TEXT,credential_encrypted TEXT)`); err != nil {
					t.Fatal(err)
				}
				for _, key := range []string{"cards-a", "cards-b"} {
					encrypted, err := crypt.Encrypt(`{"appSecret":"synthetic-` + key + `"}`)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := repository.DB().Exec(`INSERT INTO membership_card_platforms VALUES (?,?,?,?)`,
						key, provider.CardPlatformSpaceX, "https://"+key+".example/openapi/v1", encrypted); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := repository.DB().Exec(`UPDATE managed_cards SET provider_key=? WHERE id='card-settlement-route'`, configurationKey); err != nil {
					t.Fatal(err)
				}
				wantHost, wantKey = configurationKey+".example", "synthetic-"+configurationKey
			}
			event := rcContractAuthorization(now, 20)
			if err := p.rcApplyPaymentEvidence(ctx, "settlement-route", "plus", []provider.Transaction{event}, rcContractObservation(now, true), now); err != nil {
				t.Fatal(err)
			}
			observation := rcContractObservation(now, true)
			*observation.AutoRenew = false
			if err := p.rcApplyRenewalObservation(ctx, "settlement-route", observation, now); err != nil {
				t.Fatal(err)
			}
			if err := repository.EnsureLeaseTable(ctx, now); err != nil {
				t.Fatal(err)
			}
			p.lease, err = repository.AcquireLease(ctx, "go", "synthetic-settlement-worker", "test", now, time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			p.config.MaintenancePath = filepath.Join(t.TempDir(), "maintenance")
			reads := 0
			p.httpClient = &http.Client{Transport: rcTestRoundTripper(func(request *http.Request) (*http.Response, error) {
				reads++
				if request.Method != http.MethodGet || request.URL.Host != wantHost || request.URL.Path != "/openapi/v1/cards/1001/transactions" || request.Header.Get("X-API-Key") != wantKey {
					t.Fatal("settlement evidence was read from an unbound card account or a mutation was attempted")
				}
				return eligibilityHTTPResponse(http.StatusOK, `{"code":0,"data":[{"auth_id":"new-payment","auth_time":"2026-07-16T00:00:00Z", "auth_amount":20,"auth_currency":"USD","settle_amount":20,"settle_currency":"USD","status":"COMPLETE","type":"Settlement","merchant_name":"OPENAI"}]}`), nil
			})}
			if processed, err := p.tickReconciliation(ctx); !processed || err != nil {
				t.Fatalf("completed subscription did not reconcile its pending settlement: processed=%v err=%v", processed, err)
			}
			var settlement string
			if err := repository.DB().QueryRow(`SELECT settlement_state FROM membership_payment_stages WHERE fulfillment_id='settlement-route'`).Scan(&settlement); err != nil {
				t.Fatal(err)
			}
			if reads != 1 || settlement != domain.TransactionStatusComplete {
				t.Fatalf("bound settlement was not confirmed: reads=%d settlement=%s", reads, settlement)
			}
		})
	}
}

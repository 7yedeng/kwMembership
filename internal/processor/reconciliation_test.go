package processor

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kwmembership/internal/config"
	"kwmembership/internal/domain"
	"kwmembership/internal/provider"
	"kwmembership/internal/secure"
	"kwmembership/internal/store"
)

func TestReconciliationPaymentAndRenewalProjection(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repository, err := store.Open(filepath.Join(t.TempDir(), "reconciliation.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	rcTestSchema(t, ctx, repository)
	p := &Processor{store: repository}
	now := time.Date(2026, 7, 16, 0, 0, 0, 0, time.UTC)
	rcTestSeedStage(t, ctx, repository, "mf-plus", "plus", "plus", now)

	autoRenew := true
	expires := now.Add(31 * 24 * time.Hour)
	plus := &domain.MembershipObservation{
		ProviderCode: 200, AccountType: domain.TierPlus, Currency: "PHP", AutoRenew: &autoRenew,
		ExpireTime: &expires, ExpireTimeValid: true, ExpireTimeFuture: true, ObservedAt: now,
	}
	events := []provider.Transaction{
		{AuthID: "old-auth", AuthTime: store.ISO(now), AuthAmount: 20, AuthCurrency: "USD", SettleAmount: 20,
			SettleCurrency: "USD", Type: domain.TransactionTypeSettlement, Status: domain.TransactionStatusComplete, MerchantNormalized: "OPENAI"},
		{AuthID: "new-auth", AuthTime: store.ISO(now), AuthAmount: 20, AuthCurrency: "USD", SettleAmount: 20,
			SettleCurrency: "USD", Type: domain.TransactionTypeSettlement, Status: domain.TransactionStatusComplete, MerchantNormalized: "OPENAI"},
	}
	if err := p.rcApplyPaymentEvidence(ctx, "mf-plus", "plus", events, plus, now); err != nil {
		t.Fatal(err)
	}
	var fulfillmentState, stageState, matchedID, permitState, reservationState string
	if err := repository.DB().QueryRowContext(ctx, `SELECT state FROM membership_fulfillments WHERE id='mf-plus'`).Scan(&fulfillmentState); err != nil {
		t.Fatal(err)
	}
	if err := repository.DB().QueryRowContext(ctx, `SELECT state,matched_auth_id FROM membership_payment_stages WHERE id='stage-mf-plus'`).Scan(&stageState, &matchedID); err != nil {
		t.Fatal(err)
	}
	if err := repository.DB().QueryRowContext(ctx, `SELECT state FROM membership_action_permits WHERE id='permit-mf-plus'`).Scan(&permitState); err != nil {
		t.Fatal(err)
	}
	if err := repository.DB().QueryRowContext(ctx, `SELECT state FROM card_capacity_reservations WHERE fulfillment_id='mf-plus'`).Scan(&reservationState); err != nil {
		t.Fatal(err)
	}
	if fulfillmentState != "FINAL_TIER_CONFIRMED" || stageState != "confirmed" || matchedID != "new-auth" ||
		permitState != "reported" || reservationState != "consumed" {
		t.Fatalf("unexpected projection: fulfillment=%s stage=%s match=%s permit=%s reservation=%s",
			fulfillmentState, stageState, matchedID, permitState, reservationState)
	}

	// A later stale authorization-only page cannot erase the settlement proof.
	err = repository.WithImmediate(ctx, func(tx *sql.Tx) error {
		_, err := p.rcPersistManagedTransactions(ctx, tx, "card-mf-plus", []provider.Transaction{{
			AuthID: "new-auth", AuthTime: store.ISO(now), AuthAmount: 20, AuthCurrency: "USD",
			Type: domain.TransactionTypeAuthorization, Status: domain.TransactionStatusPending, MerchantNormalized: "OPENAI",
		}}, now.Add(time.Minute))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	var transactionType, transactionState string
	var authorizationSeen, settlementSeen int
	if err := repository.DB().QueryRowContext(ctx, `SELECT type,status,authorization_seen,settlement_seen
    FROM managed_card_transactions WHERE card_id='card-mf-plus' AND auth_id='new-auth'`).
		Scan(&transactionType, &transactionState, &authorizationSeen, &settlementSeen); err != nil {
		t.Fatal(err)
	}
	if transactionType != domain.TransactionTypeSettlement || transactionState != domain.TransactionStatusComplete ||
		authorizationSeen != 1 || settlementSeen != 1 {
		t.Fatalf("transaction evidence regressed: type=%s status=%s auth=%d settlement=%d",
			transactionType, transactionState, authorizationSeen, settlementSeen)
	}

	plus.ObservedAt = now.Add(time.Minute)
	if err := p.rcApplyRenewalObservation(ctx, "mf-plus", plus, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := repository.DB().QueryRowContext(ctx, `SELECT state FROM membership_fulfillments WHERE id='mf-plus'`).Scan(&fulfillmentState); err != nil {
		t.Fatal(err)
	}
	if fulfillmentState != "RENEWAL_CANCELLING" {
		t.Fatalf("enabled renewal must not complete fulfillment, got %s", fulfillmentState)
	}

	autoRenew = false
	plus.ObservedAt = now.Add(2 * time.Minute)
	if err := p.rcApplyRenewalObservation(ctx, "mf-plus", plus, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := repository.DB().QueryRowContext(ctx, `SELECT state FROM membership_fulfillments WHERE id='mf-plus'`).Scan(&fulfillmentState); err != nil {
		t.Fatal(err)
	}
	if fulfillmentState != "COMPLETED" {
		t.Fatalf("completion requires the false renewal observation, got %s", fulfillmentState)
	}
}

func TestReconciliationPartialDeclineAndNoPaymentCheckpoints(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repository, err := store.Open(filepath.Join(t.TempDir(), "partial.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	rcTestSchema(t, ctx, repository)
	p := &Processor{store: repository}
	now := time.Date(2026, 7, 16, 0, 0, 0, 0, time.UTC)

	rcTestSeedStage(t, ctx, repository, "mf-upgrade", "x5", "upgrade", now)
	autoRenew := true
	expires := now.Add(31 * 24 * time.Hour)
	plus := &domain.MembershipObservation{
		ProviderCode: 200, AccountType: domain.TierPlus, Currency: "PHP", AutoRenew: &autoRenew,
		ExpireTime: &expires, ExpireTimeValid: true, ExpireTimeFuture: true, ObservedAt: now,
	}
	decline := []provider.Transaction{{
		AuthID: "upgrade-decline", AuthTime: store.ISO(now), AuthAmount: 95, AuthCurrency: "USD",
		Type: domain.TransactionTypeAuthorization, Status: domain.TransactionStatusDeclined, MerchantNormalized: "OPENAI",
	}}
	if err := p.rcApplyPaymentEvidence(ctx, "mf-upgrade", "upgrade", decline, plus, now); err != nil {
		t.Fatal(err)
	}
	var state, reservation string
	if err := repository.DB().QueryRowContext(ctx, `SELECT state FROM membership_fulfillments WHERE id='mf-upgrade'`).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if err := repository.DB().QueryRowContext(ctx, `SELECT state FROM card_capacity_reservations WHERE fulfillment_id='mf-upgrade'`).Scan(&reservation); err != nil {
		t.Fatal(err)
	}
	if state != "PARTIALLY_FULFILLED" || reservation != "retained_partial" {
		t.Fatalf("upgrade decline lost paid Plus value: state=%s reservation=%s", state, reservation)
	}

	rcTestSeedStage(t, ctx, repository, "mf-no-payment", "plus", "plus", now)
	checkAt := now.Add(24 * time.Hour)
	free := &domain.MembershipObservation{
		ProviderCode: 200, AccountType: domain.TierFree, ObservedAt: checkAt,
	}
	terminalDecline := []provider.Transaction{{
		AuthID: "small-declined-auth", AuthTime: store.ISO(checkAt), AuthAmount: 1, AuthCurrency: "USD",
		Type: domain.TransactionTypeAuthorization, Status: domain.TransactionStatusDeclined, MerchantNormalized: "OPENAI",
	}}
	if err := p.rcApplyPaymentEvidence(ctx, "mf-no-payment", "plus", terminalDecline, free, checkAt); err != nil {
		t.Fatal(err)
	}
	var checks, passed int
	if err := repository.DB().QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(
      membership_unchanged*no_effective_transaction*no_pending_authorization),0)
    FROM membership_no_payment_checks WHERE fulfillment_id='mf-no-payment'`).Scan(&checks, &passed); err != nil {
		t.Fatal(err)
	}
	if checks != 3 || passed != 3 {
		t.Fatalf("terminal declined authorization must not remain pending, checks=%d passed=%d", checks, passed)
	}
}

func TestReconciliationPlusDeclineReleasesReservationWithRevisionAndCapacity(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repository, err := store.Open(filepath.Join(t.TempDir(), "plus-decline.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	rcTestSchema(t, ctx, repository)
	now := time.Date(2026, 7, 16, 0, 0, 0, 0, time.UTC)
	rcTestSeedStage(t, ctx, repository, "mf-plus-decline", "plus", "plus", now)
	if _, err := repository.DB().ExecContext(ctx, `UPDATE managed_cards
    SET capacity_state='CAPACITY_FULL' WHERE id='card-mf-plus-decline'`); err != nil {
		t.Fatal(err)
	}
	rcTestApplyPlusDecline(t, ctx, repository, "mf-plus-decline", now)

	var state, reservationState, releasedAt string
	var stateRevision, releaseEvidenceRevision int64
	if err := repository.DB().QueryRowContext(ctx, `SELECT state,state_revision
    FROM membership_fulfillments WHERE id='mf-plus-decline'`).Scan(&state, &stateRevision); err != nil {
		t.Fatal(err)
	}
	if err := repository.DB().QueryRowContext(ctx, `SELECT state,released_at,release_evidence_revision
    FROM card_capacity_reservations WHERE fulfillment_id='mf-plus-decline'`).
		Scan(&reservationState, &releasedAt, &releaseEvidenceRevision); err != nil {
		t.Fatal(err)
	}
	var lane sql.NullString
	var capacityState string
	if err := repository.DB().QueryRowContext(ctx, `SELECT lane,capacity_state
    FROM managed_cards WHERE id='card-mf-plus-decline'`).Scan(&lane, &capacityState); err != nil {
		t.Fatal(err)
	}
	if state != "PAYMENT_DECLINED" || reservationState != "released" || releasedAt != store.ISO(now) ||
		releaseEvidenceRevision != stateRevision || stateRevision != 1 || lane.Valid || capacityState != "AVAILABLE" {
		t.Fatalf("unsafe decline release: state=%s revision=%d reservation=%s released=%s evidence=%d lane=%v capacity=%s",
			state, stateRevision, reservationState, releasedAt, releaseEvidenceRevision, lane, capacityState)
	}
}

func TestReconciliationPlusDeclineKeepsNonProvisionalLane(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		block func(context.Context, *store.Store, string, time.Time) error
	}{
		{
			name: "consumed slots",
			block: func(ctx context.Context, repository *store.Store, id string, _ time.Time) error {
				_, err := repository.DB().ExecContext(ctx, `UPDATE managed_cards SET consumed_slots=1 WHERE id=?`, "card-"+id)
				return err
			},
		},
		{
			name: "another active reservation",
			block: func(ctx context.Context, repository *store.Store, id string, now time.Time) error {
				_, err := repository.DB().ExecContext(ctx, `INSERT INTO card_capacity_reservations
          (id,fulfillment_id,card_id,target_lane,slot_index,state,reserved_at)
          VALUES (?,?,?,?,2,'reserved',?)`, "other-reservation-"+id, "other-fulfillment-"+id, "card-"+id, "plus", store.ISO(now))
				return err
			},
		},
		{
			name: "effective OpenAI transaction",
			block: func(ctx context.Context, repository *store.Store, id string, now time.Time) error {
				at := store.ISO(now)
				_, err := repository.DB().ExecContext(ctx, `INSERT INTO managed_card_transactions
          (card_id,auth_id,auth_time,auth_amount,auth_currency,settle_amount,settle_currency,
           type,status,merchant_normalized,authorization_seen,settlement_seen,refund_seen,reversal_seen,
           first_seen_at,last_seen_at)
          VALUES (?,?,?,?,?,0,NULL,'Authorization','PENDING','OPENAI',1,0,0,0,?,?)`,
					"card-"+id, "effective-"+id, at, 1, "USD", at, at)
				return err
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			repository, err := store.Open(filepath.Join(t.TempDir(), "lane.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer repository.Close()
			rcTestSchema(t, ctx, repository)
			now := time.Date(2026, 7, 16, 0, 0, 0, 0, time.UTC)
			id := "mf-lane-blocked"
			rcTestSeedStage(t, ctx, repository, id, "plus", "plus", now)
			if err := test.block(ctx, repository, id, now); err != nil {
				t.Fatal(err)
			}
			rcTestApplyPlusDecline(t, ctx, repository, id, now)

			var reservationState, lane, capacityState string
			if err := repository.DB().QueryRowContext(ctx, `SELECT state
          FROM card_capacity_reservations WHERE fulfillment_id=?`, id).Scan(&reservationState); err != nil {
				t.Fatal(err)
			}
			if err := repository.DB().QueryRowContext(ctx, `SELECT lane,capacity_state
          FROM managed_cards WHERE id=?`, "card-"+id).Scan(&lane, &capacityState); err != nil {
				t.Fatal(err)
			}
			if reservationState != "released" || lane != "plus" || capacityState != "AVAILABLE" {
				t.Fatalf("capacity guard failed: reservation=%s lane=%s capacity=%s", reservationState, lane, capacityState)
			}
		})
	}
}

func TestReconciliationUnexpectedPreauthPersistsEvidenceWithoutPaymentSideEffects(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repository, err := store.Open(filepath.Join(t.TempDir(), "preauth.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	rcTestSchema(t, ctx, repository)
	now := time.Date(2026, 7, 16, 0, 0, 0, 0, time.UTC)
	rcTestSeedStage(t, ctx, repository, "mf-preauth", "plus", "plus", now)
	if _, err := repository.DB().ExecContext(ctx, `UPDATE membership_fulfillments
    SET state='UNEXPECTED_PREAUTH',failure_code='UNEXPECTED_PREAUTH',state_revision=1
    WHERE id='mf-preauth'`); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.DB().ExecContext(ctx, `UPDATE membership_action_permits
    SET action_type='progression',state='blocked',outcome_code='UNEXPECTED_PREAUTH'
    WHERE id='permit-mf-preauth'`); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.DB().ExecContext(ctx, `INSERT INTO fulfillment_interventions
    (id,fulfillment_id,state,state_revision,reason_code,created_at)
    VALUES ('fi-preauth','mf-preauth','UNEXPECTED_PREAUTH',1,'UNEXPECTED_PREAUTH',?)`, store.ISO(now)); err != nil {
		t.Fatal(err)
	}
	p := &Processor{store: repository}
	if due, found, err := p.rcDueFulfillment(ctx, now); err != nil || !found || due.ID != "mf-preauth" {
		t.Fatalf("existing intervention suppressed first preauth evidence read: due=%s found=%v err=%v", due.ID, found, err)
	}
	free := &domain.MembershipObservation{ProviderCode: 200, AccountType: domain.TierFree, ObservedAt: now}
	events := []provider.Transaction{{
		AuthID: "small-preauth", AuthTime: store.ISO(now), AuthAmount: 1, AuthCurrency: "USD",
		Type: domain.TransactionTypeAuthorization, Status: domain.TransactionStatusDeclined, MerchantNormalized: "OPENAI",
	}}
	if err := p.rcApplyProgressionEvidence(ctx, "mf-preauth", "UNEXPECTED_PREAUTH", "plus", true, events, free, now); err != nil {
		t.Fatal(err)
	}
	var state, failureCode, stageState, reservationState string
	var retryAt sql.NullString
	if err := repository.DB().QueryRowContext(ctx, `SELECT state,failure_code,retry_at
    FROM membership_fulfillments WHERE id='mf-preauth'`).Scan(&state, &failureCode, &retryAt); err != nil {
		t.Fatal(err)
	}
	if err := repository.DB().QueryRowContext(ctx, `SELECT state FROM membership_payment_stages
    WHERE fulfillment_id='mf-preauth'`).Scan(&stageState); err != nil {
		t.Fatal(err)
	}
	if err := repository.DB().QueryRowContext(ctx, `SELECT state FROM card_capacity_reservations
    WHERE fulfillment_id='mf-preauth'`).Scan(&reservationState); err != nil {
		t.Fatal(err)
	}
	if state != "UNEXPECTED_PREAUTH" || failureCode != "UNEXPECTED_PREAUTH_NO_PAYMENT_EVIDENCE" ||
		retryAt.Valid || stageState != "reconciling" || reservationState != "reserved" {
		t.Fatalf("preauth reconciliation escaped its safety hold: state=%s failure=%s retry=%v stage=%s reservation=%s",
			state, failureCode, retryAt, stageState, reservationState)
	}
	if _, found, err := p.rcDueFulfillment(ctx, now); err != nil || found {
		t.Fatalf("terminal preauth evidence should wait for admin/adapter review: found=%v err=%v", found, err)
	}
}

func TestReconciliationExceptionalStateScheduling(t *testing.T) {
	t.Parallel()
	t.Run("lost challenge waits for context sanitation", func(t *testing.T) {
		ctx := context.Background()
		repository, err := store.Open(filepath.Join(t.TempDir(), "context-lost.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer repository.Close()
		rcTestSchema(t, ctx, repository)
		now := time.Date(2026, 7, 16, 0, 0, 0, 0, time.UTC)
		rcTestSeedStage(t, ctx, repository, "mf-context-lost", "plus", "plus", now)
		if _, err := repository.DB().ExecContext(ctx, `UPDATE membership_fulfillments
      SET state='ACTION_REQUIRED_CONTEXT_LOST',browser_lease_epoch=7,state_revision=1
      WHERE id='mf-context-lost'`); err != nil {
			t.Fatal(err)
		}
		p := &Processor{store: repository}
		if _, found, err := p.rcDueFulfillment(ctx, now); err != nil || found {
			t.Fatalf("lost challenge reconciled before sanitation: found=%v err=%v", found, err)
		}
		if _, err := repository.DB().ExecContext(ctx, `UPDATE membership_fulfillments
      SET browser_lease_epoch=NULL,state_revision=2 WHERE id='mf-context-lost'`); err != nil {
			t.Fatal(err)
		}
		if due, found, err := p.rcDueFulfillment(ctx, now); err != nil || !found || due.ID != "mf-context-lost" {
			t.Fatalf("sanitized lost challenge was not due: due=%s found=%v err=%v", due.ID, found, err)
		}
	})

	for _, state := range []string{"UPGRADE_CHECKOUT_UNAVAILABLE", "CHECKOUT_UI_UNSUPPORTED"} {
		state := state
		t.Run(state+" stays manual and visible", func(t *testing.T) {
			ctx := context.Background()
			repository, err := store.Open(filepath.Join(t.TempDir(), "manual.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer repository.Close()
			rcTestSchema(t, ctx, repository)
			now := time.Date(2026, 7, 16, 0, 0, 0, 0, time.UTC)
			rcTestSeedStage(t, ctx, repository, "mf-manual", "x5", "upgrade", now)
			reason := state + "_REVIEW_REQUIRED"
			if _, err := repository.DB().ExecContext(ctx, `UPDATE membership_fulfillments
        SET state=?,failure_code=?,state_revision=1 WHERE id='mf-manual'`, state, reason); err != nil {
				t.Fatal(err)
			}
			p := &Processor{store: repository, now: func() time.Time { return now }}
			processed, err := p.tickReconciliation(ctx)
			if err != nil || !processed {
				t.Fatalf("manual state was not surfaced: processed=%v err=%v", processed, err)
			}
			var gotState, gotReason string
			if err := repository.DB().QueryRowContext(ctx, `SELECT f.state,i.reason_code
          FROM membership_fulfillments f JOIN fulfillment_interventions i ON i.fulfillment_id=f.id
          WHERE f.id='mf-manual'`).Scan(&gotState, &gotReason); err != nil {
				t.Fatal(err)
			}
			if gotState != state || gotReason != reason {
				t.Fatalf("manual state changed or hidden: state=%s reason=%s", gotState, gotReason)
			}
			if processed, err := p.tickReconciliation(ctx); err != nil || processed {
				t.Fatalf("manual intervention should be one-shot without pending settlement: processed=%v err=%v", processed, err)
			}
		})
	}
}

func TestReconciliationLeaseLossAfterProviderReadWritesNoEvidence(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repository, err := store.Open(filepath.Join(t.TempDir(), "lease-loss.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	rcTestSchema(t, ctx, repository)
	now := time.Date(2026, 7, 16, 0, 0, 0, 0, time.UTC)
	if err := repository.EnsureLeaseTable(ctx, now); err != nil {
		t.Fatal(err)
	}
	lease, err := repository.AcquireLease(ctx, "go", "rc-test-token", "test", now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	decrypter, err := secure.NewDecrypter("reconciliation-test-secret")
	if err != nil {
		t.Fatal(err)
	}
	credential, err := decrypter.Encrypt("openapi-secret")
	if err != nil {
		t.Fatal(err)
	}
	session, err := decrypter.Encrypt(`{"token":"session"}`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.DB().ExecContext(ctx, `INSERT INTO membership_fulfillment_settings
    (id,spacexcard_app_id,spacexcard_app_secret_encrypted) VALUES ('default','app',?)`, credential); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.DB().ExecContext(ctx, `INSERT INTO redeem_orders (id,session_payload) VALUES (?,?)`,
		"order-mf-lease-loss", session); err != nil {
		t.Fatal(err)
	}
	rcTestSeedStage(t, ctx, repository, "mf-lease-loss", "plus", "plus", now)

	client := &http.Client{Transport: rcTestRoundTripper(func(*http.Request) (*http.Response, error) {
		if _, err := repository.DB().ExecContext(ctx, `UPDATE membership_processor_lease
      SET status='stopped' WHERE id='default'`); err != nil {
			return nil, err
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"code":0,"data":[]}`)),
		}, nil
	})}
	p := &Processor{
		config:     config.Config{MaintenancePath: filepath.Join(t.TempDir(), "standby")},
		store:      repository,
		lease:      lease,
		decrypter:  decrypter,
		httpClient: client,
		membership: provider.NewMembershipClient(client),
		now:        func() time.Time { return now },
	}
	processed, err := p.tickReconciliation(ctx)
	if !processed || !errors.Is(err, store.ErrLeaseLost) {
		t.Fatalf("expected lease loss after provider read, processed=%v err=%v", processed, err)
	}
	var state string
	var failureCode, retryAt sql.NullString
	if err := repository.DB().QueryRowContext(ctx, `SELECT state,failure_code,retry_at
    FROM membership_fulfillments WHERE id='mf-lease-loss'`).Scan(&state, &failureCode, &retryAt); err != nil {
		t.Fatal(err)
	}
	var transactions, observations int
	if err := repository.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM managed_card_transactions`).Scan(&transactions); err != nil {
		t.Fatal(err)
	}
	if err := repository.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM membership_observations`).Scan(&observations); err != nil {
		t.Fatal(err)
	}
	if state != "PLUS_RECONCILING" || failureCode.Valid || retryAt.Valid || transactions != 0 || observations != 0 {
		t.Fatalf("lost lease wrote reconciliation state: state=%s failure=%v retry=%v tx=%d observations=%d",
			state, failureCode, retryAt, transactions, observations)
	}
}

func TestReconciliationTickPersistsRetryAndReturnsCodedFailure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repository, err := store.Open(filepath.Join(t.TempDir(), "coded-failure.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	rcTestSchema(t, ctx, repository)
	now := time.Date(2026, 7, 16, 0, 0, 0, 0, time.UTC)
	rcTestSeedStage(t, ctx, repository, "mf-coded-failure", "plus", "plus", now)
	if _, err := repository.DB().ExecContext(ctx, `UPDATE membership_payment_stages
    SET price_signal_min=NULL WHERE fulfillment_id='mf-coded-failure'`); err != nil {
		t.Fatal(err)
	}
	p := &Processor{store: repository, now: func() time.Time { return now }}
	processed, err := p.tickReconciliation(ctx)
	if !processed || errorCode(err) != "PAYMENT_STAGE_EVIDENCE_INCOMPLETE" {
		t.Fatalf("coded reconciliation failure was swallowed: processed=%v err=%v code=%s",
			processed, err, errorCode(err))
	}
	var failureCode, retryAt string
	if err := repository.DB().QueryRowContext(ctx, `SELECT failure_code,retry_at
    FROM membership_fulfillments WHERE id='mf-coded-failure'`).Scan(&failureCode, &retryAt); err != nil {
		t.Fatal(err)
	}
	if failureCode != "PAYMENT_STAGE_EVIDENCE_INCOMPLETE" || retryAt != store.ISO(now.Add(rcFailureRetry)) {
		t.Fatalf("retry projection missing: failure=%s retry=%s", failureCode, retryAt)
	}
}

type rcTestRoundTripper func(*http.Request) (*http.Response, error)

func (roundTrip rcTestRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

func rcTestSchema(t *testing.T, ctx context.Context, repository *store.Store) {
	t.Helper()
	statements := []string{
		`CREATE TABLE membership_fulfillments (
      id TEXT PRIMARY KEY,order_id TEXT NOT NULL,order_no TEXT NOT NULL,target_tier TEXT NOT NULL,
      state TEXT NOT NULL,current_stage TEXT,run_mode TEXT,account_lock_key TEXT,state_revision INTEGER NOT NULL DEFAULT 0,
      retry_at TEXT,money_boundary_at TEXT,browser_lease_epoch INTEGER,card_reservation_id TEXT,failure_code TEXT,created_at TEXT NOT NULL,
      updated_at TEXT NOT NULL,completed_at TEXT)`,
		`CREATE TABLE membership_payment_stages (
      id TEXT PRIMARY KEY,fulfillment_id TEXT NOT NULL,stage_key TEXT NOT NULL,expected_tier TEXT NOT NULL,
      state TEXT NOT NULL,attempt_no INTEGER,card_id TEXT,price_signal_min REAL,price_signal_max REAL,submit_permitted_at TEXT,
      matched_auth_id TEXT,settlement_state TEXT,membership_observation_id TEXT,confirmed_at TEXT,
      created_at TEXT NOT NULL,updated_at TEXT NOT NULL)`,
		`CREATE TABLE membership_action_permits (
      id TEXT PRIMARY KEY,fulfillment_id TEXT NOT NULL,stage_key TEXT NOT NULL,action_type TEXT NOT NULL,
      attempt_no INTEGER NOT NULL,sequence_no INTEGER NOT NULL,state TEXT NOT NULL,reported_at TEXT,outcome_code TEXT)`,
		`CREATE TABLE membership_action_auth_snapshots (
      permit_id TEXT NOT NULL,card_id TEXT NOT NULL,auth_id TEXT NOT NULL,snapshotted_at TEXT NOT NULL,
      PRIMARY KEY(permit_id,auth_id))`,
		`CREATE TABLE managed_cards (
      id TEXT PRIMARY KEY,upstream_card_id INTEGER NOT NULL,vm_card_id TEXT NOT NULL,product_code TEXT NOT NULL,
      upstream_status TEXT NOT NULL,cached_available_amount REAL NOT NULL DEFAULT 0,lane TEXT,
      consumed_slots INTEGER NOT NULL DEFAULT 0,capacity_state TEXT NOT NULL DEFAULT 'AVAILABLE',
      reconciliation_state TEXT NOT NULL DEFAULT 'PENDING',reconciliation_reason TEXT,
      last_balance_sync_at TEXT,last_transaction_sync_at TEXT,created_at TEXT NOT NULL,updated_at TEXT NOT NULL)`,
		`CREATE TABLE managed_card_transactions (
      card_id TEXT NOT NULL,auth_id TEXT NOT NULL,auth_time TEXT,auth_amount REAL NOT NULL DEFAULT 0,
      auth_currency TEXT,settle_amount REAL NOT NULL DEFAULT 0,settle_currency TEXT,type TEXT NOT NULL,status TEXT NOT NULL,
      merchant_normalized TEXT NOT NULL,authorization_seen INTEGER NOT NULL DEFAULT 0,
      settlement_seen INTEGER NOT NULL DEFAULT 0,refund_seen INTEGER NOT NULL DEFAULT 0,
      reversal_seen INTEGER NOT NULL DEFAULT 0,decline_reason_code TEXT,first_seen_at TEXT NOT NULL,last_seen_at TEXT NOT NULL,
      PRIMARY KEY(card_id,auth_id))`,
		`CREATE TABLE membership_observations (
      id TEXT PRIMARY KEY,fulfillment_id TEXT NOT NULL,stage_key TEXT,purpose TEXT NOT NULL,provider_code INTEGER NOT NULL,
      account_type TEXT NOT NULL,currency TEXT,auto_renew INTEGER,is_overdue INTEGER NOT NULL,is_delinquent INTEGER NOT NULL,
      expire_time TEXT,observed_at TEXT NOT NULL)`,
		`CREATE TABLE card_capacity_reservations (
      id TEXT PRIMARY KEY,fulfillment_id TEXT NOT NULL UNIQUE,card_id TEXT,planned_product_code TEXT,
      target_lane TEXT NOT NULL,slot_index INTEGER,state TEXT NOT NULL,reserved_at TEXT NOT NULL,
      consumed_at TEXT,released_at TEXT,release_evidence_revision INTEGER)`,
		`CREATE TABLE fulfillment_interventions (
      id TEXT PRIMARY KEY,fulfillment_id TEXT NOT NULL,state TEXT NOT NULL,state_revision INTEGER NOT NULL,
      reason_code TEXT NOT NULL,acknowledged_at TEXT,acknowledged_by TEXT,feishu_status TEXT,feishu_sent_at TEXT,
      created_at TEXT NOT NULL,UNIQUE(fulfillment_id,state,state_revision))`,
		`CREATE TABLE membership_no_payment_checks (
      id TEXT PRIMARY KEY,fulfillment_id TEXT NOT NULL,stage_key TEXT NOT NULL,checkpoint TEXT NOT NULL,
      membership_unchanged INTEGER NOT NULL,no_effective_transaction INTEGER NOT NULL,
      no_pending_authorization INTEGER NOT NULL,observed_at TEXT NOT NULL,
      UNIQUE(fulfillment_id,stage_key,checkpoint))`,
		`CREATE TABLE membership_fulfillment_attempts (
      id TEXT PRIMARY KEY,fulfillment_id TEXT NOT NULL,ended_at TEXT,outcome_code TEXT)`,
		`CREATE TABLE membership_fulfillment_settings (
      id TEXT PRIMARY KEY,spacexcard_app_id TEXT,spacexcard_app_secret_encrypted TEXT)`,
		`CREATE TABLE redeem_orders (id TEXT PRIMARY KEY,session_payload TEXT NOT NULL)`,
	}
	for _, statement := range statements {
		if _, err := repository.DB().ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
}

func rcTestApplyPlusDecline(t *testing.T, ctx context.Context, repository *store.Store, id string, now time.Time) {
	t.Helper()
	free := &domain.MembershipObservation{
		ProviderCode: 200,
		AccountType:  domain.TierFree,
		ObservedAt:   now,
	}
	decline := []provider.Transaction{{
		AuthID:             "decline-" + id,
		AuthTime:           store.ISO(now),
		AuthAmount:         20,
		AuthCurrency:       "USD",
		Type:               domain.TransactionTypeAuthorization,
		Status:             domain.TransactionStatusDeclined,
		MerchantNormalized: "OPENAI",
	}}
	p := &Processor{store: repository}
	if err := p.rcApplyPaymentEvidence(ctx, id, "plus", decline, free, now); err != nil {
		t.Fatal(err)
	}
}

func rcTestSeedStage(t *testing.T, ctx context.Context, repository *store.Store, id, tier, key string, now time.Time) {
	t.Helper()
	at := store.ISO(now)
	state := "PLUS_RECONCILING"
	expectedTier := tier
	if key == "upgrade" {
		state = "UPGRADE_RECONCILING"
	} else {
		expectedTier = "plus"
	}
	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO membership_fulfillments
      (id,order_id,order_no,target_tier,state,current_stage,money_boundary_at,card_reservation_id,created_at,updated_at)
      VALUES (?,?,?,?,?,?,?,?,?,?)`, []any{id, "order-" + id, "ORDER-" + id, tier, state, key, at, "reservation-" + id, at, at}},
		{`INSERT INTO managed_cards
      (id,upstream_card_id,vm_card_id,product_code,upstream_status,cached_available_amount,lane,
       consumed_slots,capacity_state,reconciliation_state,created_at,updated_at)
      VALUES (?,?,?,?,?,0,?,0,'AVAILABLE','READY',?,?)`,
			[]any{"card-" + id, 1001, "vm-" + id, "product-test", "ACTIVE", tier, at, at}},
		{`INSERT INTO card_capacity_reservations
      (id,fulfillment_id,card_id,target_lane,slot_index,state,reserved_at) VALUES (?,?,?,?,1,'reserved',?)`,
			[]any{"reservation-" + id, id, "card-" + id, tier, at}},
		{`INSERT INTO membership_payment_stages
	  (id,fulfillment_id,stage_key,expected_tier,state,attempt_no,card_id,price_signal_min,price_signal_max,
	   submit_permitted_at,created_at,updated_at) VALUES (?,?,?,?,?,1,?,?,?,?,?,?)`,
			[]any{"stage-" + id, id, key, expectedTier, "reconciling", "card-" + id, 18, 25, at, at, at}},
		{`INSERT INTO membership_action_permits
	  (id,fulfillment_id,stage_key,action_type,attempt_no,sequence_no,state) VALUES (?,?,?,'submit',1,1,'activated')`,
			[]any{"permit-" + id, id, key}},
		{`INSERT INTO membership_action_auth_snapshots
      (permit_id,card_id,auth_id,snapshotted_at) VALUES (?,?,?,?)`,
			[]any{"permit-" + id, "card-" + id, "old-auth", at}},
	}
	for _, statement := range statements {
		if _, err := repository.DB().ExecContext(ctx, statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
}

package processor

import (
	"net/http"
	"testing"
	"time"

	"kwmembership/internal/provider"
	"kwmembership/internal/store"
)

// A 2xx-only upstream idempotency cache cannot justify replaying an unknown
// write. Exercise the actual recovery entry with durable intents and the
// configured Zovo adapter, not merely its capability method.
func TestZovoUnknownFundingRecoveryNeverReplaysOpenOrRecharge(t *testing.T) {
	for _, operation := range []string{"open", "recharge"} {
		for _, state := range []string{"outcome_unknown", "submitted"} {
			t.Run(operation+"/"+state, func(t *testing.T) {
				calls := 0
				fixture := newPaymentRecoveryFixture(t, operation, state, func(_ *store.Store, _ time.Time, _ *http.Request) (*http.Response, error) {
					calls++
					return paymentProviderResponse(200, `{"code":0}`), nil
				})
				for _, statement := range []string{
					`ALTER TABLE funding_intents ADD COLUMN provider_key TEXT NOT NULL DEFAULT 'zovo-primary'`,
					`ALTER TABLE managed_cards ADD COLUMN provider_key TEXT NOT NULL DEFAULT 'zovo-primary'`,
					`ALTER TABLE card_capacity_reservations ADD COLUMN provider_key TEXT NOT NULL DEFAULT 'zovo-primary'`,
					`CREATE TABLE membership_card_platforms (key TEXT PRIMARY KEY,kind TEXT,base_url TEXT,credential_encrypted TEXT)`,
				} {
					if _, err := fixture.repository.DB().Exec(statement); err != nil {
						t.Fatal(err)
					}
				}
				credential, err := fixture.processor.decrypter.Encrypt(`{"appSecret":"synthetic-zovo-credential"}`)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := fixture.repository.DB().Exec(`INSERT INTO membership_card_platforms VALUES ('zovo-primary',?,?,?)`,
					provider.CardPlatformZovo, provider.ZovoCardSandboxBaseURL, credential); err != nil {
					t.Fatal(err)
				}
				err = fixture.processor.RecoverFundingOutcome(fixture.ctx, "mf-recovery", FundingRecoveryOptions{Enabled: true, AllowOrphanedSubmitted: true})
				if errorCode(err) != "FUNDING_RECOVERY_UNSUPPORTED" || calls != 0 {
					t.Fatalf("recovery escaped policy: code=%s calls=%d err=%v", errorCode(err), calls, err)
				}
				var intentState, fulfillmentState, storedKey, storedFingerprint string
				if err := fixture.repository.DB().QueryRow(`SELECT state,idempotency_key,request_fingerprint FROM funding_intents WHERE id='intent-recovery'`).Scan(&intentState, &storedKey, &storedFingerprint); err != nil {
					t.Fatal(err)
				}
				if err := fixture.repository.DB().QueryRow(`SELECT state FROM membership_fulfillments WHERE id='mf-recovery'`).Scan(&fulfillmentState); err != nil {
					t.Fatal(err)
				}
				if intentState != state || fulfillmentState != "FUNDING_OUTCOME_UNKNOWN" || storedKey != fixture.idempotencyKey || storedFingerprint != paymentFingerprint(fixture.canonical) {
					t.Fatal("refused recovery altered durable evidence")
				}
			})
		}
	}
}

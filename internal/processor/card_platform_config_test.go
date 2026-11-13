package processor

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"

	"kwmembership/internal/provider"
	"kwmembership/internal/secure"
	"kwmembership/internal/store"
)

func TestSpaceXCardPlatformUsesConfiguredBaseURL(t *testing.T) {
	for _, tc := range []struct{ name, baseURL, wantHost, wantCode string }{
		{"configured", "https://cards.example/openapi/v1", "cards.example", ""},
		{"legacy default", "", "spacexcard.com", ""},
		{"reject insecure", "http://cards.example", "", "SPACEXCARD_CONFIGURATION_INVALID"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repository, err := store.Open(filepath.Join(t.TempDir(), "platform.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer repository.Close()
			decrypter, err := secure.NewDecrypter("synthetic-platform-test-secret")
			if err != nil {
				t.Fatal(err)
			}
			credential, err := decrypter.Encrypt(`{"appId":"test-app","appSecret":"test-key"}`)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := repository.DB().Exec(`CREATE TABLE membership_card_platforms (
			  key TEXT PRIMARY KEY,kind TEXT,base_url TEXT,credential_encrypted TEXT
			); INSERT INTO membership_card_platforms VALUES ('spacexcard','spacexcard',?,?)`, tc.baseURL, credential); err != nil {
				t.Fatal(err)
			}
			calls := 0
			p := &Processor{store: repository, decrypter: decrypter, httpClient: &http.Client{Transport: eligibilityRoundTripper(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.Host != tc.wantHost || r.URL.Scheme != "https" || r.URL.Path != "/openapi/v1/balance" ||
					r.Header.Get("X-API-Key") != "test-key" || r.Header.Get("X-App-Id") != "test-app" {
					t.Error("platform configuration was not used for the authenticated request")
				}
				return eligibilityHTTPResponse(http.StatusOK, `{"code":0,"data":{"balance":20,"currency":"USD"}}`), nil
			})}}
			platform, err := p.cardPlatform(context.Background(), provider.CardPlatformSpaceX)
			if tc.wantCode != "" {
				if errorCode(err) != tc.wantCode || calls != 0 {
					t.Fatalf("invalid endpoint was not blocked before request: err=%v calls=%d", err, calls)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			balance, err := platform.GetBalance(context.Background())
			if err != nil || balance.Balance != 20 || calls != 1 {
				t.Fatalf("configured platform request: balance=%v calls=%d err=%v", balance, calls, err)
			}
		})
	}
}

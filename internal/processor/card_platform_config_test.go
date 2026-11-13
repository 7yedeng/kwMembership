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

func TestCardPlatformSameKindConfigurationKeysRemainIsolated(t *testing.T) {
	repository, err := store.Open(filepath.Join(t.TempDir(), "platforms.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	decrypter, err := secure.NewDecrypter("synthetic-platform-isolation-secret")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.DB().Exec(`CREATE TABLE membership_card_platforms (
		key TEXT PRIMARY KEY,kind TEXT,base_url TEXT,credential_encrypted TEXT
	)`); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"cards-a", "cards-b"} {
		credential, err := decrypter.Encrypt(`{"appSecret":"synthetic-` + key + `"}`)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := repository.DB().Exec(`INSERT INTO membership_card_platforms VALUES (?,?,?,?)`,
			key, provider.CardPlatformSpaceX, "https://"+key+".example", credential); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string]int{}
	p := &Processor{store: repository, decrypter: decrypter, httpClient: &http.Client{Transport: eligibilityRoundTripper(func(r *http.Request) (*http.Response, error) {
		key := r.Header.Get("X-API-Key")
		if (r.URL.Host != "cards-a.example" || key != "synthetic-cards-a") &&
			(r.URL.Host != "cards-b.example" || key != "synthetic-cards-b") {
			t.Fatal("configured accounts shared credentials or endpoint")
		}
		seen[r.URL.Host]++
		return eligibilityHTTPResponse(http.StatusOK, `{"code":0,"data":{"balance":20,"currency":"USD"}}`), nil
	})}}
	for _, key := range []string{"cards-a", "cards-b"} {
		platform, err := p.cardPlatform(context.Background(), key)
		if err != nil {
			t.Fatal(err)
		}
		if platform.Key() != key {
			t.Fatal("funding/inventory key was replaced with the shared adapter kind")
		}
		configured, ok := platform.(provider.ConfiguredCardPlatform)
		if !ok || configured.Kind() != provider.CardPlatformSpaceX {
			t.Fatal("adapter kind is missing")
		}
		if _, err := platform.GetBalance(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if seen["cards-a.example"] != 1 || seen["cards-b.example"] != 1 {
		t.Fatal("platform read isolation failed")
	}
}

func TestCardPlatformUnknownKindAndInvalidCredentialsFailWithoutRequests(t *testing.T) {
	for _, tc := range []struct{ name, kind, credential, code string }{
		{"unknown kind", "unknown", `{}`, "CARD_PLATFORM_KIND_UNSUPPORTED"},
		{"invalid credential", provider.CardPlatformSpaceX, `{"appSecret":123}`, "CARD_PLATFORM_NOT_CONFIGURED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repository, err := store.Open(filepath.Join(t.TempDir(), "platforms.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer repository.Close()
			decrypter, err := secure.NewDecrypter("synthetic-platform-error-secret")
			if err != nil {
				t.Fatal(err)
			}
			encrypted, err := decrypter.Encrypt(tc.credential)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := repository.DB().Exec(`CREATE TABLE membership_card_platforms (
				key TEXT PRIMARY KEY,kind TEXT,base_url TEXT,credential_encrypted TEXT
			); INSERT INTO membership_card_platforms VALUES ('cards',?,'https://cards.example',?)`, tc.kind, encrypted); err != nil {
				t.Fatal(err)
			}
			p := &Processor{store: repository, decrypter: decrypter, httpClient: &http.Client{Transport: eligibilityRoundTripper(func(*http.Request) (*http.Response, error) {
				t.Fatal("invalid platform performed network I/O")
				return nil, nil
			})}}
			_, err = p.cardPlatform(context.Background(), "cards")
			if errorCode(err) != tc.code {
				t.Fatalf("error code=%s want=%s", errorCode(err), tc.code)
			}
		})
	}
}

func TestCardPlatformLegacySettingsRemainAvailable(t *testing.T) {
	repository, err := store.Open(filepath.Join(t.TempDir(), "legacy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	decrypter, err := secure.NewDecrypter("synthetic-platform-legacy-secret")
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := decrypter.Encrypt("synthetic-legacy-api-key")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.DB().Exec(`CREATE TABLE membership_fulfillment_settings (
		id TEXT PRIMARY KEY,spacexcard_app_id TEXT,spacexcard_app_secret_encrypted TEXT
	); INSERT INTO membership_fulfillment_settings VALUES ('default','legacy-app',?)`, encrypted); err != nil {
		t.Fatal(err)
	}
	requests := 0
	p := &Processor{store: repository, decrypter: decrypter, httpClient: &http.Client{Transport: eligibilityRoundTripper(func(r *http.Request) (*http.Response, error) {
		requests++
		if r.URL.Host != "spacexcard.com" || r.Header.Get("X-App-Id") != "legacy-app" || r.Header.Get("X-API-Key") != "synthetic-legacy-api-key" {
			t.Fatal("legacy settings were not preserved")
		}
		return eligibilityHTTPResponse(http.StatusOK, `{"code":0,"data":{"balance":20,"currency":"USD"}}`), nil
	})}}
	platform, err := p.cardPlatform(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if platform.Key() != provider.CardPlatformSpaceX || requests != 0 {
		t.Fatal("legacy construction changed")
	}
	if _, err := platform.GetBalance(context.Background()); err != nil || requests != 1 {
		t.Fatalf("legacy read failed: %v", err)
	}
}

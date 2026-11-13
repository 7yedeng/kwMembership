package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type factoryRoundTripper func(*http.Request) (*http.Response, error)

func (f factoryRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func factoryResponse(body string) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}

func TestCardPlatformFactorySelectsAdaptersWithoutNetwork(t *testing.T) {
	calls := 0
	client := &http.Client{Transport: factoryRoundTripper(func(request *http.Request) (*http.Response, error) {
		calls++
		switch request.URL.Host + request.URL.Path {
		case "spacex.example/openapi/v1/balance":
			if request.Header.Get("X-API-Key") != "synthetic-space-secret" || request.Header.Get("X-App-Id") != "synthetic-app" {
				t.Fatal("wrong SpaceX credentials")
			}
			return factoryResponse(`{"code":0,"data":{"balance":50,"currency":"USD"}}`), nil
		case "efun.example/account/balance":
			if request.Header.Get("X-API-Key") != "synthetic-efun-secret" || request.Header.Get("X-App-Id") != "" {
				t.Fatal("wrong Efun credentials")
			}
			return factoryResponse(`{"success":true,"data":{"balance":"730.00","currency":"CNY"}}`), nil
		case "efun.example/card-types":
			return factoryResponse(efunCatalogFixture), nil
		default:
			t.Fatal("unexpected request")
			return nil, errors.New("unexpected request")
		}
	})}
	space, err := NewCardPlatform(client, CardPlatformConfig{
		Key: "cards-primary", Kind: CardPlatformSpaceX, BaseURL: "https://spacex.example",
		CredentialJSON: json.RawMessage(`{"appId":"synthetic-app","appSecret":"synthetic-space-secret"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	efun, err := NewCardPlatform(client, CardPlatformConfig{
		Key: "cards-secondary", Kind: CardPlatformEfun, BaseURL: "https://efun.example",
		CredentialJSON: json.RawMessage(`{"apiKey":"synthetic-efun-secret"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatal("constructors must not contact card platforms")
	}
	if space.Key() != "cards-primary" || space.Kind() != CardPlatformSpaceX ||
		efun.Key() != "cards-secondary" || efun.Kind() != CardPlatformEfun {
		t.Fatal("configuration identity was confused with adapter kind")
	}
	if CardPlatformKind(space) != CardPlatformSpaceX || CardPlatformKind(efun) != CardPlatformEfun ||
		CardPlatformKind(&registeredFactoryAdapter{kind: CardPlatformSpaceX}) != CardPlatformSpaceX {
		t.Fatal("adapter error classification does not preserve the provider kind")
	}
	if space.Capabilities() != (CardPlatformCapabilities{Freeze: true, OpenCard: true, Recharge: true, RechargeIdempotent: true}) ||
		efun.Capabilities() != (CardPlatformCapabilities{OpenCard: true, Recharge: true, FundingReplayWindow: 5 * time.Minute}) {
		t.Fatal("configuration wrapper changed funding capability contracts")
	}
	spaceBalance, err := space.GetBalance(context.Background())
	if err != nil || spaceBalance.Balance != 50 || spaceBalance.Currency != "USD" {
		t.Fatalf("SpaceX normalized read failed: %v", err)
	}
	efunBalance, err := efun.GetBalance(context.Background())
	if err != nil || efunBalance.Balance != 100 || efunBalance.Currency != "USD" || calls != 3 {
		t.Fatalf("Efun normalized read failed: %v", err)
	}
}

func TestCardPlatformFactoryKeepsSameKindAccountsSeparate(t *testing.T) {
	seen := map[string]int{}
	client := &http.Client{Transport: factoryRoundTripper(func(request *http.Request) (*http.Response, error) {
		expected := "synthetic-" + strings.TrimSuffix(request.URL.Hostname(), ".example")
		if request.Header.Get("X-API-Key") != expected {
			t.Fatal("a card account received another account's credential")
		}
		seen[request.URL.Hostname()]++
		return factoryResponse(`{"code":0,"data":{"balance":10,"currency":"USD"}}`), nil
	})}
	registry := NewCardPlatformRegistry()
	for _, key := range []string{"account-a", "account-b", "account-a"} {
		credential := json.RawMessage(`{"appSecret":"synthetic-` + key + `"}`)
		original := string(credential)
		platform, err := registry.Create(client, CardPlatformConfig{
			Key: key, Kind: CardPlatformSpaceX, BaseURL: "https://" + key + ".example", CredentialJSON: credential,
		})
		if err != nil || platform.Key() != key || platform.Kind() != CardPlatformSpaceX {
			t.Fatalf("configuration binding failed: %v", err)
		}
		if string(credential) != original {
			t.Fatal("factory mutated caller credential storage")
		}
		if _, err := platform.GetBalance(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if seen["account-a.example"] != 2 || seen["account-b.example"] != 1 {
		t.Fatal("configured endpoint isolation failed")
	}
}

type registeredFactoryAdapter struct {
	CardPlatform
	kind string
}

func (a *registeredFactoryAdapter) Key() string { return a.kind }

func TestCardPlatformRegistryRejectsReplacementAndSupportsNewAdapter(t *testing.T) {
	registry := NewCardPlatformRegistry()
	factory := func(_ *http.Client, _ CardPlatformConfig) (CardPlatform, error) {
		return &registeredFactoryAdapter{kind: "examplecard"}, nil
	}
	requireProviderErrorCode(t, registry.Register(CardPlatformSpaceX, factory), "CARD_PLATFORM_KIND_DUPLICATE")
	requireProviderErrorCode(t, registry.Register("bad kind", factory), "CARD_PLATFORM_REGISTRATION_INVALID")
	requireProviderErrorCode(t, registry.Register("examplecard", nil), "CARD_PLATFORM_REGISTRATION_INVALID")
	if err := registry.Register("examplecard", factory); err != nil {
		t.Fatal(err)
	}
	requireProviderErrorCode(t, registry.Register("examplecard", factory), "CARD_PLATFORM_KIND_DUPLICATE")
	platform, err := registry.Create(&http.Client{}, CardPlatformConfig{
		Key: "tenant-cards", Kind: "examplecard", CredentialJSON: json.RawMessage(`{}`),
	})
	if err != nil || platform.Key() != "tenant-cards" || platform.Kind() != "examplecard" {
		t.Fatalf("registered adapter is not available: %v", err)
	}
}

func TestCardPlatformFactoryRejectsMalformedConfiguration(t *testing.T) {
	valid := CardPlatformConfig{Key: "cards", Kind: CardPlatformSpaceX, CredentialJSON: json.RawMessage(`{"appSecret":"synthetic-secret"}`)}
	for _, tc := range []struct {
		name string
		edit func(*CardPlatformConfig)
		code string
	}{
		{"unknown kind", func(c *CardPlatformConfig) { c.Kind = "unknown" }, "CARD_PLATFORM_KIND_UNSUPPORTED"},
		{"invalid key", func(c *CardPlatformConfig) { c.Key = "cards\nother" }, "CARD_PLATFORM_CONFIGURATION_INVALID"},
		{"invalid kind", func(c *CardPlatformConfig) { c.Kind = "SpaceX Card" }, "CARD_PLATFORM_CONFIGURATION_INVALID"},
		{"empty credential", func(c *CardPlatformConfig) { c.CredentialJSON = nil }, "CARD_PLATFORM_NOT_CONFIGURED"},
		{"large credential", func(c *CardPlatformConfig) { c.CredentialJSON = make([]byte, (64<<10)+1) }, "CARD_PLATFORM_NOT_CONFIGURED"},
		{"invalid JSON", func(c *CardPlatformConfig) { c.CredentialJSON = json.RawMessage(`secret-not-json`) }, "CARD_PLATFORM_NOT_CONFIGURED"},
		{"wrong credential shape", func(c *CardPlatformConfig) { c.CredentialJSON = json.RawMessage(`{"appSecret":123}`) }, "CARD_PLATFORM_NOT_CONFIGURED"},
		{"null credential", func(c *CardPlatformConfig) { c.CredentialJSON = json.RawMessage(`null`) }, "CARD_PLATFORM_NOT_CONFIGURED"},
		{"missing Efun key", func(c *CardPlatformConfig) { c.Kind = CardPlatformEfun }, "CARD_PLATFORM_NOT_CONFIGURED"},
		{"invalid SpaceX endpoint", func(c *CardPlatformConfig) { c.BaseURL = "http://cards.example" }, "SPACEXCARD_CONFIGURATION_INVALID"},
		{"invalid Efun endpoint", func(c *CardPlatformConfig) {
			c.Kind = CardPlatformEfun
			c.CredentialJSON = json.RawMessage(`{"apiKey":"synthetic-secret"}`)
		}, "EFUNCARD_CONFIGURATION_INVALID"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := valid
			tc.edit(&config)
			_, err := NewCardPlatform(&http.Client{}, config)
			requireProviderErrorCode(t, err, tc.code)
			var typed *Error
			if !errors.As(err, &typed) || !typed.KnownNoWrite || typed.Retryable {
				t.Fatal("invalid construction must not be retried as a write")
			}
		})
	}
	_, err := NewCardPlatform(nil, valid)
	requireProviderErrorCode(t, err, "CARD_PLATFORM_CONFIGURATION_INVALID")
}

func TestCardPlatformFactoryNeverForwardsConstructorSecrets(t *testing.T) {
	secret := "synthetic-credential-do-not-log"
	for _, tc := range []struct {
		name string
		fn   CardPlatformFactory
		code string
	}{
		{"raw error", func(_ *http.Client, _ CardPlatformConfig) (CardPlatform, error) { return nil, errors.New(secret) }, "CARD_PLATFORM_FACTORY_INVALID"},
		{"coded error", func(_ *http.Client, _ CardPlatformConfig) (CardPlatform, error) {
			return nil, &Error{ErrorCode: "CARD_PLATFORM_NOT_CONFIGURED", Message: secret}
		}, "CARD_PLATFORM_NOT_CONFIGURED"},
		{"nil result", func(_ *http.Client, _ CardPlatformConfig) (CardPlatform, error) { return nil, nil }, "CARD_PLATFORM_FACTORY_INVALID"},
		{"typed nil", func(_ *http.Client, _ CardPlatformConfig) (CardPlatform, error) {
			var p *registeredFactoryAdapter
			return p, nil
		}, "CARD_PLATFORM_FACTORY_INVALID"},
		{"wrong kind", func(_ *http.Client, _ CardPlatformConfig) (CardPlatform, error) {
			return &registeredFactoryAdapter{kind: CardPlatformSpaceX}, nil
		}, "CARD_PLATFORM_FACTORY_INVALID"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			registry := NewCardPlatformRegistry()
			if err := registry.Register("testcard", tc.fn); err != nil {
				t.Fatal(err)
			}
			_, err := registry.Create(&http.Client{}, CardPlatformConfig{Key: "cards", Kind: "testcard", CredentialJSON: json.RawMessage(`{}`)})
			requireProviderErrorCode(t, err, tc.code)
			if strings.Contains(fmt.Sprintf("%+v", err), secret) {
				t.Fatal("constructor error disclosed credential")
			}
		})
	}
	config := CardPlatformConfig{Key: "cards", Kind: CardPlatformSpaceX, BaseURL: "https://" + secret + ".example", CredentialJSON: json.RawMessage(`{"appSecret":"` + secret + `"}`)}
	encoded, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	for _, output := range []string{string(encoded), fmt.Sprint(config), fmt.Sprintf("%+v", config), fmt.Sprintf("%#v", config)} {
		if strings.Contains(output, secret) {
			t.Fatal("configuration diagnostic disclosed credentials or endpoint")
		}
	}
}

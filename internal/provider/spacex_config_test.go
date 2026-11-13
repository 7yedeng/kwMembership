package provider

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestSpaceXBaseURLConfiguration(t *testing.T) {
	valid := []struct{ input, want string }{
		{"", SpaceXCardBaseURL},
		{"https://zovocard.com", "https://zovocard.com/openapi/v1"},
		{" https://ZOVOCARD.com/ ", "https://zovocard.com/openapi/v1"},
		{"https://sandbox.zovocard.com/openapi/v1/", "https://sandbox.zovocard.com/openapi/v1"},
		{"https://cards.example:8443/openapi/v1", "https://cards.example:8443/openapi/v1"},
		{"https://[::1]:8443", "https://[::1]:8443/openapi/v1"},
	}
	for _, tc := range valid {
		t.Run(tc.input, func(t *testing.T) {
			client, err := NewSpaceXClientWithBaseURL(&http.Client{}, tc.input, "app", "test-secret")
			if err != nil {
				t.Fatal(err)
			}
			if client.baseURL != tc.want {
				t.Fatalf("base URL = %s, want %s", client.baseURL, tc.want)
			}
		})
	}
	invalid := []string{
		"http://cards.example", "//cards.example", "https:///openapi/v1", "https://",
		"https://user:secret-marker@cards.example", "https://cards.example?api_key=secret-marker",
		"https://cards.example?", "https://cards.example/#secret-marker", "https://cards.example/#",
		"https://cards.example/other", "https://cards.example/openapi/v1/../v2", "https://cards.example/%6fpenapi/v1",
		"https://cards.example:", "https://cards.example:0", "https://cards.example:65536", "https://cards.example:nope",
	}
	for _, input := range invalid {
		t.Run(input, func(t *testing.T) {
			client, err := NewSpaceXClientWithBaseURL(&http.Client{}, input, "app", "test-secret")
			var failure *Error
			if client != nil || !errors.As(err, &failure) || failure.ErrorCode != "SPACEXCARD_CONFIGURATION_INVALID" {
				t.Fatalf("invalid URL accepted: client=%v err=%v", client, err)
			}
			if strings.Contains(err.Error(), "secret-marker") || strings.Contains(err.Error(), "test-secret") {
				t.Fatal("configuration error disclosed sensitive input")
			}
		})
	}
}

func TestSpaceXRefusesRedirectsEvenWithCallerDefaultPolicy(t *testing.T) {
	for _, status := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var targetCalls, sourceCalls atomic.Int32
			target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				targetCalls.Add(1)
				w.WriteHeader(http.StatusOK)
			}))
			defer target.Close()
			source := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				sourceCalls.Add(1)
				if r.URL.Path != "/openapi/v1/balance" || r.Header.Get("X-API-Key") != "dummy-key" {
					t.Error("configured source did not receive the expected request")
				}
				http.Redirect(w, r, target.URL+"/capture", status)
			}))
			defer source.Close()
			httpClient := source.Client()
			client, err := NewSpaceXClientWithBaseURL(httpClient, source.URL, "dummy-app", "dummy-key")
			if err != nil {
				t.Fatal(err)
			}
			if httpClient.CheckRedirect != nil {
				t.Fatal("provider mutated caller's shared HTTP client")
			}
			_, err = client.GetBalance(context.Background())
			var failure *Error
			if !errors.As(err, &failure) || failure.ErrorCode != "SPACEXCARD_REDIRECT_REFUSED" || failure.KnownNoWrite {
				t.Fatalf("redirect did not fail conservatively: %v", err)
			}
			if sourceCalls.Load() != 1 || targetCalls.Load() != 0 {
				t.Fatalf("API credentials followed redirect: source=%d target=%d", sourceCalls.Load(), targetCalls.Load())
			}
		})
	}
}

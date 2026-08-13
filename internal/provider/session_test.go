package provider

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestBrowserSessionFromJSONChunksSessionTokenWithoutNetwork(t *testing.T) {
	now := time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC)
	token := strings.Repeat("a", 4400)
	raw, _ := json.Marshal(map[string]any{
		"sessionToken": token,
		"expires":      "2026-10-24T09:43:50.305Z",
		"user":         map[string]any{"email": " Buyer@Example.com "},
	})
	session, err := BrowserSessionFromJSON(raw, now)
	if err != nil {
		t.Fatal(err)
	}
	if session.Email != "buyer@example.com" || len(session.Cookies) != 2 {
		t.Fatalf("session identity/cookies = %q/%d", session.Email, len(session.Cookies))
	}
	if session.Cookies[0].Name != cookieBaseName+".0" || len(session.Cookies[0].Value) != 3936 ||
		session.Cookies[1].Name != cookieBaseName+".1" || len(session.Cookies[1].Value) != 464 {
		t.Fatalf("unexpected Cookie chunks: %s/%d %s/%d",
			session.Cookies[0].Name, len(session.Cookies[0].Value), session.Cookies[1].Name, len(session.Cookies[1].Value))
	}
	if session.Cookies[0].Domain != ".chatgpt.com" || session.Cookies[0].Path != "/" ||
		!session.Cookies[0].Secure || !session.Cookies[0].HTTPOnly || session.Cookies[0].SameSite != "lax" ||
		session.Cookies[0].Expiration == nil {
		t.Fatalf("unsafe Cookie attributes: %+v", session.Cookies[0])
	}
}

func TestBrowserSessionFromJSONRejectsMissingAndExpiredSessionToken(t *testing.T) {
	now := time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC)
	for name, raw := range map[string]json.RawMessage{
		"missing": json.RawMessage(`{"user":{"email":"buyer@example.com"},"expires":"2026-10-24T09:43:50Z"}`),
		"expired": json.RawMessage(`{"user":{"email":"buyer@example.com"},"sessionToken":"opaque","expires":"2026-07-27T09:43:50Z"}`),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := BrowserSessionFromJSON(raw, now); err == nil {
				t.Fatal("invalid Session unexpectedly accepted")
			}
		})
	}
}

package provider

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

const (
	cookieBaseName         = "__Secure-next-auth.session-token"
	sessionCookieChunkSize = 3936
	maxSessionPayloadBytes = 256 << 10
	maxSessionTokenBytes   = 64 << 10
)

// BrowserSession is the only checkout-facing representation of an order's
// protected ChatGPT Session. It keeps Session parsing and Cookie chunking
// behind one interface so processors never handle token fields directly.
type BrowserSession struct {
	Email   string
	Cookies []SessionCookie
}

func BrowserSessionFromJSON(raw json.RawMessage, now time.Time) (BrowserSession, error) {
	if len(raw) == 0 || len(raw) > maxSessionPayloadBytes || !json.Valid(raw) || raw[0] != '{' {
		return BrowserSession{}, fail("SESSION_INVALID", "session is invalid", false)
	}
	var payload struct {
		SessionToken string `json:"sessionToken"`
		Expires      string `json:"expires"`
		Email        string `json:"email"`
		User         struct {
			Email string `json:"email"`
		} `json:"user"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return BrowserSession{}, fail("SESSION_INVALID", "session is invalid", false)
	}
	email := normalizeProviderEmail(payload.User.Email)
	if email == "" {
		email = normalizeProviderEmail(payload.Email)
	}
	if email == "" {
		return BrowserSession{}, fail("EXPECTED_IDENTITY_MISSING", "session identity is missing", false)
	}
	token := payload.SessionToken
	if token == "" || len(token) > maxSessionTokenBytes || strings.TrimSpace(token) != token || strings.ContainsAny(token, "\r\n\x00") {
		return BrowserSession{}, fail("SESSION_COOKIE_MISSING", "session cookie token is missing", false)
	}

	var expiration *float64
	if strings.TrimSpace(payload.Expires) != "" {
		expiresAt, err := time.Parse(time.RFC3339Nano, payload.Expires)
		if err != nil {
			return BrowserSession{}, fail("SESSION_INVALID", "session expiry is invalid", false)
		}
		if !expiresAt.After(now) {
			return BrowserSession{}, fail("CHATGPT_SESSION_UNAUTHORIZED", "session has expired", false)
		}
		value := float64(expiresAt.UnixNano()) / float64(time.Second)
		expiration = &value
	}

	count := (len(token) + sessionCookieChunkSize - 1) / sessionCookieChunkSize
	cookies := make([]SessionCookie, 0, count)
	for index, offset := 0, 0; offset < len(token); index, offset = index+1, offset+sessionCookieChunkSize {
		end := offset + sessionCookieChunkSize
		if end > len(token) {
			end = len(token)
		}
		name := cookieBaseName
		if count > 1 {
			name += "." + strconv.Itoa(index)
		}
		cookies = append(cookies, SessionCookie{
			Name: name, Value: token[offset:end], Domain: ".chatgpt.com", Path: "/",
			SameSite: "lax", Secure: true, HTTPOnly: true, Expiration: expiration,
		})
	}
	return BrowserSession{Email: email, Cookies: cookies}, nil
}

package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

const (
	GPTCheckoutURL = "https://spacexcard.com/api/v1/gpt/checkout"
	GPTCookieURL   = "https://spacexcard.com/api/v1/gpt/session-to-cookie"
	maxGPTResponse = 256 << 10
)

type GPTClient struct{ http *http.Client }

type SessionCookie struct {
	Name       string
	Value      string
	Domain     string
	Path       string
	SameSite   string
	Secure     bool
	HTTPOnly   bool
	Expiration *float64
}

type CheckoutSession struct {
	URL     string
	Cookies []SessionCookie
}

func NewGPTClient(client *http.Client) *GPTClient { return &GPTClient{http: client} }

func (c *GPTClient) PrepareCheckout(ctx context.Context, session json.RawMessage, token string) (CheckoutSession, error) {
	checkoutURL, err := c.CheckoutURL(ctx, session, token)
	if err != nil {
		return CheckoutSession{}, err
	}
	cookies, err := c.SessionCookies(ctx, session, token)
	if err != nil {
		return CheckoutSession{}, err
	}
	return CheckoutSession{URL: checkoutURL, Cookies: cookies}, nil
}

func (c *GPTClient) CheckoutURL(ctx context.Context, session json.RawMessage, token string) (string, error) {
	if !json.Valid(session) || len(session) == 0 || session[0] != '{' {
		return "", fail("SESSION_INVALID", "session is invalid", false)
	}
	token = strings.TrimSpace(token)
	if token == "" || len(token) > 8192 {
		return "", fail("CHECKOUT_BROKER_NOT_CONFIGURED", "checkout broker token is not configured", false)
	}
	compact := string(session)
	checkoutBody, _ := json.Marshal(map[string]string{
		"token_input": compact, "plan_name": "plus", "country": "PH", "currency": "PHP",
	})
	checkoutRaw, err := c.post(ctx, GPTCheckoutURL, token, checkoutBody)
	if err != nil {
		return "", err
	}
	var envelope map[string]json.RawMessage
	if json.Unmarshal(checkoutRaw, &envelope) != nil || envelope == nil {
		return "", fail("CHECKOUT_BROKER_RESPONSE_INVALID", "checkout broker response is invalid", true)
	}
	var code int
	codeRaw, ok := envelope["code"]
	if !ok || json.Unmarshal(codeRaw, &code) != nil {
		return "", fail("CHECKOUT_BROKER_CODE_INVALID", "checkout broker response code is invalid", true)
	}
	if !gptSuccessCode(code) {
		return "", fail("CHECKOUT_BROKER_BUSINESS_REJECTED", "checkout broker rejected the request", false)
	}
	var data map[string]json.RawMessage
	dataRaw, ok := envelope["data"]
	if !ok || json.Unmarshal(dataRaw, &data) != nil || data == nil {
		return "", fail("CHECKOUT_BROKER_LINK_MISSING", "checkout broker link is missing", true)
	}
	var link string
	linkRaw, ok := data["link"]
	if !ok || json.Unmarshal(linkRaw, &link) != nil || strings.TrimSpace(link) == "" {
		return "", fail("CHECKOUT_BROKER_LINK_MISSING", "checkout broker link is missing", true)
	}
	validated, err := validateCheckoutURL(link)
	if err != nil {
		return "", fail("CHECKOUT_BROKER_LINK_INVALID", "checkout broker link is outside the allowlist", true)
	}
	return validated, nil
}

func (c *GPTClient) SessionCookies(ctx context.Context, session json.RawMessage, token string) ([]SessionCookie, error) {
	if !json.Valid(session) || len(session) == 0 || session[0] != '{' {
		return nil, fail("SESSION_INVALID", "session is invalid", false)
	}
	token = strings.TrimSpace(token)
	if token == "" || len(token) > 8192 {
		return nil, fail("CHECKOUT_BROKER_NOT_CONFIGURED", "checkout broker token is not configured", false)
	}
	var identity struct {
		User struct {
			Email string `json:"email"`
		} `json:"user"`
		Email string `json:"email"`
	}
	if json.Unmarshal(session, &identity) != nil {
		return nil, fail("SESSION_INVALID", "session is invalid", false)
	}
	expectedEmail := normalizeProviderEmail(identity.User.Email)
	if expectedEmail == "" {
		expectedEmail = normalizeProviderEmail(identity.Email)
	}
	if expectedEmail == "" {
		return nil, fail("EXPECTED_IDENTITY_MISSING", "session identity is missing", false)
	}
	compact := string(session)
	cookieBody, _ := json.Marshal(map[string]string{"token_input": compact})
	cookieRaw, err := c.post(ctx, GPTCookieURL, token, cookieBody)
	if err != nil {
		return nil, err
	}
	cookies, err := normalizeCookieEnvelope(cookieRaw, expectedEmail)
	if err != nil {
		return nil, err
	}
	return cookies, nil
}

func (c *GPTClient) post(ctx context.Context, endpoint, token string, body []byte) ([]byte, error) {
	request, err := newRequest(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		return nil, wrap("CHECKOUT_BROKER_UNAVAILABLE", "checkout broker network failure", err, true)
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, fail("CHECKOUT_BROKER_AUTH_FAILED", "checkout broker authentication failed", false)
	case http.StatusTooManyRequests:
		return nil, fail("CHECKOUT_BROKER_RATE_LIMITED", "checkout broker rate limited", true)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fail("CHECKOUT_BROKER_UNAVAILABLE", "checkout broker unavailable", true)
	}
	return readLimited(response, maxGPTResponse, "CHECKOUT_BROKER_RESPONSE_TOO_LARGE")
}

func validateCheckoutURL(value string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Fragment != "" {
		return "", fail("CHECKOUT_BROKER_CONTRACT_DRIFT", "checkout URL is invalid", true)
	}
	allowed := parsed.Host == "chatgpt.com" && (parsed.Path == "/checkout" || parsed.Path == "/checkout/" ||
		regexp.MustCompile(`^/checkout/[A-Za-z0-9_-]+/?$`).MatchString(parsed.Path) ||
		regexp.MustCompile(`^/checkout/openai_llc/(?:oaics_|cs_)[A-Za-z0-9_-]+/?$`).MatchString(parsed.Path))
	allowed = allowed || parsed.Host == "pay.openai.com" && (regexp.MustCompile(`^/checkout/[A-Za-z0-9_-]+/?$`).MatchString(parsed.Path) || regexp.MustCompile(`^/(?:c/)?pay/[A-Za-z0-9_-]+/?$`).MatchString(parsed.Path))
	if !allowed {
		return "", fail("CHECKOUT_BROKER_CONTRACT_DRIFT", "checkout URL is outside the allowlist", true)
	}
	return parsed.String(), nil
}

func normalizeCookieEnvelope(raw []byte, expectedEmail string) ([]SessionCookie, error) {
	var envelope struct {
		Code *int `json:"code"`
		Data struct {
			Cookies []struct {
				Domain         string   `json:"domain"`
				HostOnly       *bool    `json:"hostOnly"`
				HTTPOnly       *bool    `json:"httpOnly"`
				Name           string   `json:"name"`
				Path           string   `json:"path"`
				SameSite       string   `json:"sameSite"`
				Secure         *bool    `json:"secure"`
				Session        *bool    `json:"session"`
				StoreID        any      `json:"storeId"`
				Value          string   `json:"value"`
				ExpirationDate *float64 `json:"expirationDate"`
			} `json:"cookies"`
			Count *int   `json:"count"`
			Email string `json:"email"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &envelope) != nil || envelope.Code == nil || !gptSuccessCode(*envelope.Code) ||
		envelope.Data.Count == nil || *envelope.Data.Count != len(envelope.Data.Cookies) || len(envelope.Data.Cookies) == 0 {
		return nil, fail("CONVERTER_CONTRACT_DRIFT", "session cookie response contract drift", true)
	}
	if normalizeProviderEmail(envelope.Data.Email) != expectedEmail {
		return nil, fail("CONVERTER_IDENTITY_MISMATCH", "session cookie identity mismatch", false)
	}
	seen := map[string]bool{}
	result := make([]SessionCookie, 0, len(envelope.Data.Cookies))
	for index, rawCookie := range envelope.Data.Cookies {
		expectedName := cookieBaseName
		if len(envelope.Data.Cookies) > 1 {
			expectedName = fmt.Sprintf("%s.%d", cookieBaseName, index)
		}
		domain := strings.ToLower(strings.TrimSpace(rawCookie.Domain))
		sameSite := strings.ToLower(strings.TrimSpace(rawCookie.SameSite))
		if sameSite == "none" || sameSite == "no_restriction" {
			sameSite = "none"
		}
		if rawCookie.Name != expectedName || domain != ".chatgpt.com" || rawCookie.Path != "/" || rawCookie.Value == "" ||
			rawCookie.HostOnly == nil || *rawCookie.HostOnly || rawCookie.HTTPOnly == nil || !*rawCookie.HTTPOnly ||
			rawCookie.Secure == nil || !*rawCookie.Secure || rawCookie.Session == nil ||
			rawCookie.StoreID != nil || !map[string]bool{"unspecified": true, "none": true, "lax": true, "strict": true}[sameSite] {
			return nil, fail("CONVERTER_CONTRACT_DRIFT", "session cookie response contract drift", true)
		}
		if (*rawCookie.Session && rawCookie.ExpirationDate != nil) || (!*rawCookie.Session && (rawCookie.ExpirationDate == nil || *rawCookie.ExpirationDate <= 0)) {
			return nil, fail("CONVERTER_CONTRACT_DRIFT", "session cookie expiry contract drift", true)
		}
		key := rawCookie.Name + "\x00" + domain + "\x00" + rawCookie.Path
		if seen[key] {
			return nil, fail("COOKIE_PAYLOAD_INVALID", "session cookie is duplicated", false)
		}
		seen[key] = true
		result = append(result, SessionCookie{
			Name: rawCookie.Name, Value: rawCookie.Value, Domain: domain, Path: rawCookie.Path,
			SameSite: sameSite, Secure: true, HTTPOnly: true, Expiration: rawCookie.ExpirationDate,
		})
	}
	return result, nil
}

func gptSuccessCode(code int) bool {
	return code == 0 || code == http.StatusOK
}

func normalizeProviderEmail(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if len(value) > 254 || !regexp.MustCompile(`^[^\s@]+@[^\s@]+\.[^\s@]+$`).MatchString(value) {
		return ""
	}
	return value
}

package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
)

const RenewalCancelURL = "https://spacexcard.com/api/v1/gpt/cancel-renewal"

type RenewalClient struct{ http *http.Client }

func NewRenewalClient(client *http.Client) *RenewalClient { return &RenewalClient{http: client} }

func (c *RenewalClient) Cancel(ctx context.Context, session json.RawMessage, token string) error {
	token = strings.TrimSpace(token)
	if !json.Valid(session) || len(session) == 0 || session[0] != '{' {
		return fail("SESSION_INVALID", "session is invalid", false)
	}
	if token == "" || len(token) > 8192 {
		return fail("RENEWAL_CANCEL_NOT_CONFIGURED", "renewal token is not configured", false)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, session); err != nil {
		return fail("SESSION_INVALID", "session is invalid", false)
	}
	body, _ := json.Marshal(map[string]string{"token_input": compact.String()})
	request, err := newRequest(ctx, http.MethodPost, RenewalCancelURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		var networkError net.Error
		if errors.As(err, &networkError) && networkError.Timeout() {
			return wrap("RENEWAL_CANCEL_TIMEOUT", "renewal cancellation request timed out", err, true)
		}
		return wrap("RENEWAL_CANCEL_FAILED", "renewal cancellation network failure", err, true)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return fail("RENEWAL_CANCEL_AUTH_FAILED", "renewal cancellation authentication failed", true)
	}
	if response.StatusCode == http.StatusTooManyRequests {
		return fail("RENEWAL_CANCEL_RATE_LIMITED", "renewal cancellation rate limited", true)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fail("RENEWAL_CANCEL_FAILED", "renewal cancellation failed", true)
	}
	raw, err := readLimited(response, maxMembershipResponse, "RENEWAL_CANCEL_RESPONSE_TOO_LARGE")
	if err != nil {
		return err
	}
	var envelope struct {
		Code *int `json:"code"`
		Data struct {
			Cancelled *bool `json:"cancelled"`
			WillRenew *bool `json:"will_renew"`
			AutoRenew *bool `json:"auto_renew"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &envelope) != nil || envelope.Code == nil || *envelope.Code != 0 || !((envelope.Data.Cancelled != nil && *envelope.Data.Cancelled) || (envelope.Data.WillRenew != nil && !*envelope.Data.WillRenew) || (envelope.Data.AutoRenew != nil && !*envelope.Data.AutoRenew)) {
		return fail("RENEWAL_CANCEL_RESPONSE_INVALID", "renewal cancellation result is ambiguous", true)
	}
	return nil
}

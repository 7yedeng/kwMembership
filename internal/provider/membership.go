package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
)

const MembershipStateURL = "https://gptserve.freespaces.app/api/subscription/info"
const maxMembershipResponse = 128 << 10

type MembershipClient struct{ http *http.Client }

func NewMembershipClient(client *http.Client) *MembershipClient {
	return &MembershipClient{http: client}
}

func (c *MembershipClient) Fetch(ctx context.Context, session json.RawMessage) ([]byte, error) {
	if !json.Valid(session) || len(session) == 0 || session[0] != '{' {
		return nil, fail("SESSION_INVALID", "session is invalid", false)
	}
	body, err := json.Marshal(struct {
		Token json.RawMessage `json:"token"`
	}{Token: session})
	if err != nil {
		return nil, err
	}
	request, err := newRequest(ctx, http.MethodPost, MembershipStateURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		var networkError net.Error
		if errors.As(err, &networkError) && networkError.Timeout() {
			return nil, wrap("MEMBERSHIP_PROVIDER_TIMEOUT", "membership provider request timed out", err, true)
		}
		return nil, wrap("MEMBERSHIP_PROVIDER_UNAVAILABLE", "membership provider network failure", err, true)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusTooManyRequests {
		return nil, fail("MEMBERSHIP_PROVIDER_RATE_LIMITED", "membership provider rate limited", true)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fail("MEMBERSHIP_PROVIDER_UNAVAILABLE", "membership provider unavailable", true)
	}
	return readLimited(response, maxMembershipResponse, "MEMBERSHIP_PROVIDER_RESPONSE_TOO_LARGE")
}

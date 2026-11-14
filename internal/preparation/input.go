// Package preparation composes the read-only card snapshot and the isolated
// prepayment browser. It has no funding, submission or fulfillment queue API.
package preparation

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"
	"time"
	"unicode"

	"kwmembership/internal/checkout"
	"kwmembership/internal/provider"
	"kwmembership/internal/sessionrestore"
)

const MaxInputBytes = 384 << 10

type Input struct {
	session     json.RawMessage
	target      checkout.QuoteTarget
	budget      checkout.QuoteBudget
	platform    provider.CardPlatformConfig
	cardID      int64
	billing     *provider.BillingAddress
	fingerprint string
}

func (Input) String() string   { return "[protected preparation input]" }
func (Input) GoString() string { return "[protected preparation input]" }

// Clear releases the caller's owned credential buffers after a command. Go
// strings retained transiently by a client cannot be promised to be zeroized.
func (input *Input) Clear() {
	clear(input.session)
	clear(input.platform.CredentialJSON)
	*input = Input{}
}

type inputPayload struct {
	Session json.RawMessage      `json:"session"`
	Target  checkout.QuoteTarget `json:"target"`
	Budget  checkout.QuoteBudget `json:"budget"`
	Card    struct {
		PlatformKey string `json:"platformKey"`
		Kind        string `json:"kind"`
		CardID      int64  `json:"cardId"`
		Credential  struct {
			AppID     string `json:"appId"`
			AppSecret string `json:"appSecret"`
		} `json:"credential"`
	} `json:"card"`
	Billing *struct {
		PlatformKey string                  `json:"platformKey"`
		CardID      int64                   `json:"cardId"`
		Address     provider.BillingAddress `json:"address"`
	} `json:"billing,omitempty"`
}

var platformKeyPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)

func Parse(raw []byte, now time.Time) (Input, error) {
	if len(raw) > MaxInputBytes {
		return Input{}, fail("PREPARATION_INPUT_TOO_LARGE")
	}
	if !uniqueJSONKeys(raw) {
		return Input{}, fail("PREPARATION_INPUT_INVALID")
	}
	var payload inputPayload
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&payload) != nil || decoder.Decode(new(any)) != io.EOF {
		return Input{}, fail("PREPARATION_INPUT_INVALID")
	}
	prepared, err := sessionrestore.Parse(payload.Session, now)
	if err != nil {
		return Input{}, err
	}
	if payload.Target != (checkout.QuoteTarget{Plan: "plus", Country: "PH", Currency: "PHP"}) {
		return Input{}, fail("PREPAYMENT_TARGET_UNSUPPORTED")
	}
	min, minErr := checkout.ParseQuoteMinor(payload.Budget.Min, payload.Target.Currency)
	max, maxErr := checkout.ParseQuoteMinor(payload.Budget.Max, payload.Target.Currency)
	if minErr != nil || maxErr != nil || min <= 0 || max < min {
		return Input{}, fail("QUOTE_BUDGET_INVALID")
	}
	if !platformKeyPattern.MatchString(payload.Card.PlatformKey) || payload.Card.Kind != provider.CardPlatformZovo ||
		payload.Card.CardID <= 0 || !credentialText(payload.Card.Credential.AppID, false, 512) ||
		!credentialText(payload.Card.Credential.AppSecret, true, 8192) {
		return Input{}, fail("PREPARATION_CARD_CONFIGURATION_INVALID")
	}
	var billing *provider.BillingAddress
	if payload.Billing != nil {
		if payload.Billing.PlatformKey != payload.Card.PlatformKey || payload.Billing.CardID != payload.Card.CardID {
			return Input{}, fail("PREPARATION_BILLING_BINDING_MISMATCH")
		}
		address, err := provider.NormalizeBillingAddress(payload.Billing.Address)
		if err != nil {
			return Input{}, fail("PREPARATION_BILLING_INVALID")
		}
		billing = &address
	}
	credential, _ := json.Marshal(payload.Card.Credential)
	return Input{session: bytes.Clone(payload.Session), target: payload.Target, budget: payload.Budget,
		platform: provider.CardPlatformConfig{Key: payload.Card.PlatformKey, Kind: provider.CardPlatformZovo, CredentialJSON: credential},
		cardID:   payload.Card.CardID, billing: billing, fingerprint: sessionrestore.Inspect(prepared).TargetFingerprint}, nil
}

func credentialText(value string, required bool, limit int) bool {
	return (!required || value != "") && len(value) <= limit && strings.TrimSpace(value) == value &&
		strings.IndexFunc(value, unicode.IsControl) < 0
}

// The typed decoder is case-insensitive, so reject duplicate aliases at every
// level, including within the supplied Session. Arrays in Session are allowed.
func uniqueJSONKeys(raw []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var visit func(int) bool
	visit = func(depth int) bool {
		if depth > 32 {
			return false
		}
		token, err := decoder.Token()
		if err != nil {
			return false
		}
		delim, compound := token.(json.Delim)
		if !compound {
			return true
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for decoder.More() {
				key, err := decoder.Token()
				value, ok := key.(string)
				if err != nil || !ok {
					return false
				}
				value = strings.ToLower(value)
				if seen[value] {
					return false
				}
				seen[value] = true
				if !visit(depth + 1) {
					return false
				}
			}
			end, err := decoder.Token()
			return err == nil && end == json.Delim('}')
		case '[':
			for decoder.More() {
				if !visit(depth + 1) {
					return false
				}
			}
			end, err := decoder.Token()
			return err == nil && end == json.Delim(']')
		default:
			return false
		}
	}
	if !visit(0) {
		return false
	}
	_, err := decoder.Token()
	return errors.Is(err, io.EOF)
}

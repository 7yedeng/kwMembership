// Package preflight owns a bounded, non-payment development entry point. It
// does not open the worker store or construct any funding/payment dependency.
package preflight

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/mail"
	"regexp"
	"strings"
	"time"

	"kwmembership/internal/checkout"
	"kwmembership/internal/provider"
)

const MaxInputBytes = 512 << 10
const MaxSessionBytes = 256 << 10

type priceContract struct {
	ID        string  `json:"id"`
	Version   int     `json:"version"`
	Tier      string  `json:"tier"`
	Country   string  `json:"country,omitempty"`
	Currency  string  `json:"currency"`
	MinAmount float64 `json:"minAmount"`
	MaxAmount float64 `json:"maxAmount"`
}

type input struct {
	Session       json.RawMessage `json:"session"`
	ExpectedEmail string          `json:"expectedEmail"`
	TargetTier    string          `json:"targetTier"`
	PriceContract priceContract   `json:"priceContract"`
}

// Prepared keeps the executable request private: callers cannot add payment
// material, a guard, a checkout URL, a workflow binding or another target tier.
type Prepared struct {
	request   checkout.Request
	validated bool
}

type Report struct {
	Operation       string        `json:"operation"`
	Code            string        `json:"code"`
	InputValid      bool          `json:"inputValid"`
	RemoteVerified  bool          `json:"remoteVerified"`
	PreflightPassed bool          `json:"preflightPassed"`
	PaymentAllowed  bool          `json:"paymentAllowed"`
	Page            *PageReport   `json:"page,omitempty"`
	SessionShape    *SessionShape `json:"sessionShape,omitempty"`
}

// PageReport deliberately has no full URL, email, free-form page text, control
// identifier, cookie, raw response or executor error message.
type PageReport struct {
	State           string  `json:"state"`
	Origin          string  `json:"origin"`
	RouteTemplate   string  `json:"routeTemplate"`
	Plan            string  `json:"plan"`
	Country         string  `json:"country"`
	Currency        string  `json:"currency"`
	DisplayedAmount float64 `json:"displayedAmount"`
}

type safeError struct{ code string }

func (e *safeError) Error() string { return e.code }
func (e *safeError) Code() string  { return e.code }

func fail(code string) error { return &safeError{code: code} }

var allowedCodes = map[string]bool{
	"PREFLIGHT_FAILED": true, "PREFLIGHT_ARGUMENTS_INVALID": true,
	"PREFLIGHT_INPUT_INVALID": true, "PREFLIGHT_INPUT_TOO_LARGE": true,
	"PREFLIGHT_TARGET_UNSUPPORTED": true, "PREFLIGHT_PRICE_CONTRACT_INVALID": true,
	"PREFLIGHT_BROWSER_CONFIG_INVALID": true, "PREFLIGHT_BOUNDARY_VIOLATION": true,
	"PREFLIGHT_CANCELLED": true, "PREFLIGHT_TIMEOUT": true,
	"SESSION_INVALID": true, "SESSION_COOKIE_MISSING": true, "EXPECTED_IDENTITY_MISSING": true,
	"CHATGPT_SESSION_IDENTITY_MISMATCH": true, "CHATGPT_SESSION_UNAUTHORIZED": true,
	"CHATGPT_SESSION_REFRESH_FAILED": true, "CHATGPT_ACCOUNT_ALREADY_SUBSCRIBED": true,
	"HEADLESS_BROWSER_UNAVAILABLE": true, "CHECKOUT_REQUEST_INVALID": true,
	"CHECKOUT_ENTRY_UNAVAILABLE": true, "CHECKOUT_API_AUTH_FAILED": true,
	"CHECKOUT_CONTEXT_INVALID": true, "CHECKOUT_API_CONTRACT_DRIFT": true,
	"CHECKOUT_URL_INVALID": true, "CHECKOUT_PAGE_UNAVAILABLE": true,
	"CHECKOUT_PAGE_TIMEOUT": true, "CHECKOUT_UI_UNSUPPORTED": true,
	"SECURITY_CHALLENGE_REQUIRED": true, "SECURITY_CHALLENGE_TIMEOUT": true,
	"COOKIE_PAYLOAD_INVALID":     true,
	"INTERACTIVE_LOGIN_DISABLED": true, "INTERACTIVE_LOGIN_HANDOFF_FAILED": true,
	"INTERACTIVE_LOGIN_IDENTITY_MISMATCH": true, "INTERACTIVE_LOGIN_IDENTITY_UNVERIFIED": true,
	"INTERACTIVE_LOGIN_TIMEOUT": true,
}

// SafeCode never returns an upstream error string, even for an unknown code.
func SafeCode(err error) string {
	if errors.Is(err, context.Canceled) {
		return "PREFLIGHT_CANCELLED"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "PREFLIGHT_TIMEOUT"
	}
	var coded interface{ Code() string }
	if errors.As(err, &coded) && allowedCodes[coded.Code()] {
		return coded.Code()
	}
	return "PREFLIGHT_FAILED"
}

func Failure(operation, code string) Report {
	if operation != "check" && operation != "run" && operation != "inspect-session" && operation != "interactive-preflight" {
		operation = "check"
	}
	if !allowedCodes[code] {
		code = "PREFLIGHT_FAILED"
	}
	return Report{Operation: operation, Code: code}
}

func readBounded(reader io.Reader, limit int64) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, fail("PREFLIGHT_INPUT_INVALID")
	}
	if int64(len(raw)) > limit {
		return nil, fail("PREFLIGHT_INPUT_TOO_LARGE")
	}
	return raw, nil
}

func decodeSingle(raw []byte, output any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(output) != nil {
		return fail("PREFLIGHT_INPUT_INVALID")
	}
	var extra json.RawMessage
	if decoder.Decode(&extra) != io.EOF {
		return fail("PREFLIGHT_INPUT_INVALID")
	}
	return nil
}

var contractIDPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,100}$`)

func normalizeEmail(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	address, err := mail.ParseAddress(value)
	if err != nil || len(value) > 254 || address.Address != value || !strings.Contains(value, "@") {
		return ""
	}
	return value
}

// Decode performs local validation only. A correctly shaped Session is not
// evidence that the remote account is authenticated, free or cancellable.
func Decode(reader io.Reader, now time.Time) (Prepared, error) {
	raw, err := readBounded(reader, MaxInputBytes)
	if err != nil {
		return Prepared{}, err
	}
	var item input
	if err := decodeSingle(raw, &item); err != nil {
		return Prepared{}, err
	}
	request, err := metadataRequest(item.ExpectedEmail, item.TargetTier, item.PriceContract, checkout.ModeSessionPreflight)
	if err != nil {
		return Prepared{}, err
	}
	session := bytes.TrimSpace(item.Session)
	if len(session) > MaxSessionBytes {
		return Prepared{}, fail("PREFLIGHT_INPUT_TOO_LARGE")
	}
	browserSession, err := provider.BrowserSessionFromJSON(session, now)
	if err != nil {
		return Prepared{}, fail(SafeCode(err))
	}
	if browserSession.Email != request.ExpectedEmail {
		return Prepared{}, fail("CHATGPT_SESSION_IDENTITY_MISMATCH")
	}
	request.Cookies = browserSession.Cookies
	return Prepared{validated: true, request: request}, nil
}

func metadataRequest(email, tier string, contract priceContract, mode checkout.Mode) (checkout.Request, error) {
	// The automatic Session path has a reviewed Plus request only. Go may be
	// observed after user navigation; no unverified Go API SKU is constructed.
	if mode != checkout.ModeSessionPreflight && mode != checkout.ModeInteractivePreflight ||
		tier != "plus" && !(mode == checkout.ModeInteractivePreflight && tier == "go") {
		return checkout.Request{}, fail("PREFLIGHT_TARGET_UNSUPPORTED")
	}
	if !contractIDPattern.MatchString(contract.ID) || contract.Version < 1 || contract.Tier != tier ||
		math.IsNaN(contract.MinAmount) || math.IsInf(contract.MinAmount, 0) ||
		math.IsNaN(contract.MaxAmount) || math.IsInf(contract.MaxAmount, 0) ||
		contract.MinAmount <= 0 || contract.MaxAmount < contract.MinAmount {
		return checkout.Request{}, fail("PREFLIGHT_PRICE_CONTRACT_INVALID")
	}
	expected := normalizeEmail(email)
	if expected == "" {
		return checkout.Request{}, fail("EXPECTED_IDENTITY_MISSING")
	}
	request := checkout.Request{
		Mode: mode, Stage: tier, TargetTier: tier, ExpectedEmail: expected,
		PriceContract: checkout.PriceContract{
			ID: contract.ID, Version: contract.Version, Tier: tier, Country: contract.Country, Currency: contract.Currency,
			MinAmount: contract.MinAmount, MaxAmount: contract.MaxAmount,
		},
	}
	if _, _, err := checkout.ExpectedCheckoutMarket(request); err != nil {
		return checkout.Request{}, fail("PREFLIGHT_PRICE_CONTRACT_INVALID")
	}
	return request, nil
}

func Check(prepared Prepared) Report {
	if !prepared.validated {
		return Failure("check", "PREFLIGHT_INPUT_INVALID")
	}
	return Report{Operation: "check", Code: "PREFLIGHT_INPUT_VALID", InputValid: true}
}

// Run may create a remote checkout session. It cannot supply card material or
// a payment guard and never constructs a Processor or a card-platform client.
func Run(ctx context.Context, prepared Prepared, executor checkout.Executor) (Report, error) {
	if prepared.request.Mode != checkout.ModeSessionPreflight {
		return Failure("run", "PREFLIGHT_INPUT_INVALID"), fail("PREFLIGHT_INPUT_INVALID")
	}
	return execute(ctx, prepared, executor, "run")
}

func execute(ctx context.Context, prepared Prepared, executor checkout.Executor, operation string) (Report, error) {
	report := Report{Operation: operation, InputValid: prepared.validated}
	if !prepared.validated || executor == nil {
		return report, fail("PREFLIGHT_INPUT_INVALID")
	}
	expectedCountry, expectedCurrency, err := checkout.ExpectedCheckoutMarket(prepared.request)
	if err != nil {
		return report, fail("PREFLIGHT_PRICE_CONTRACT_INVALID")
	}
	if err := ctx.Err(); err != nil {
		return report, fail(SafeCode(err))
	}
	result, err := executor.Execute(ctx, prepared.request)
	if result.SubmitActivated || result.ProgressionCount != 0 || result.StoppedByGuard {
		return report, fail("PREFLIGHT_BOUNDARY_VIOLATION")
	}
	if err != nil {
		return report, fail(SafeCode(err))
	}
	if result.Challenge || result.Page.StateID == "PAYMENT_ACTION_REQUIRED" {
		return report, fail("SECURITY_CHALLENGE_REQUIRED")
	}
	page, contract := result.Page, prepared.request.PriceContract
	if (page.StateID != "PAYMENT_PROGRESSION_READY" && page.StateID != "PAYMENT_FINAL_READY") ||
		(page.Origin != "https://chatgpt.com" && page.Origin != "https://pay.openai.com") ||
		(page.RouteTemplate != "/checkout" && page.RouteTemplate != "/checkout/{id}" && page.RouteTemplate != "/pay/{id}") ||
		page.Plan != prepared.request.TargetTier || page.Plan != contract.Tier ||
		page.Country != expectedCountry || page.Currency != expectedCurrency || page.DisplayedAmount == nil ||
		math.IsNaN(*page.DisplayedAmount) || math.IsInf(*page.DisplayedAmount, 0) ||
		*page.DisplayedAmount < contract.MinAmount || *page.DisplayedAmount > contract.MaxAmount {
		return report, fail("CHECKOUT_UI_UNSUPPORTED")
	}
	report.Code = "PREFLIGHT_PASSED"
	report.RemoteVerified = true
	report.PreflightPassed = true
	report.Page = &PageReport{
		State: page.StateID, Origin: page.Origin, RouteTemplate: page.RouteTemplate,
		Plan: page.Plan, Country: page.Country, Currency: page.Currency, DisplayedAmount: *page.DisplayedAmount,
	}
	return report, nil
}

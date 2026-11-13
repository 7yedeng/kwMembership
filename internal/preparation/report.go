package preparation

import (
	"context"
	"errors"
	"regexp"

	"kwmembership/internal/checkout"
	"kwmembership/internal/sessionrestore"
)

type Error struct{ code string }

func (err *Error) Error() string { return err.code }
func (err *Error) Code() string  { return err.code }
func fail(code string) error     { return &Error{code: code} }

var preparationCodes = map[string]bool{
	"PREPARATION_FAILED": true, "PREPARATION_ARGUMENTS_INVALID": true,
	"PREPARATION_INPUT_INVALID": true, "PREPARATION_INPUT_TOO_LARGE": true,
	"PREPARATION_CARD_CONFIGURATION_INVALID": true, "PREPARATION_CARD_UNAVAILABLE": true,
	"PREPARATION_CARD_BINDING_MISMATCH": true, "PREPARATION_CARD_INVALID": true,
	"PREPARATION_CARD_EXPIRED": true, "PREPARATION_CARD_BLOCKED": true,
	"PREPARATION_BILLING_BINDING_MISMATCH": true, "PREPARATION_BILLING_INVALID": true,
	"PREPARATION_BILLING_CONFLICT": true, "PREPARATION_BILLING_INCOMPLETE": true,
	"PREPARATION_BALANCE_INSUFFICIENT": true, "PREPARATION_REPORT_INVALID": true,
	"PREPARATION_BROWSER_CONFIG_INVALID": true,
}

func SafeCode(err error) string {
	if errors.Is(err, context.Canceled) {
		return "PREPAYMENT_CANCELLED"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "PREPAYMENT_TIMEOUT"
	}
	var coded interface{ Code() string }
	if errors.As(err, &coded) && preparationCodes[coded.Code()] {
		return coded.Code()
	}
	if code := sessionrestore.SafeCode(err); code != "SESSION_UNEXPECTED" {
		return code
	}
	if code := checkout.SafeQuoteCode(err); code != "QUOTE_FAILED" {
		return code
	}
	if code := checkout.SafePrepaymentCode(err); code != "PREPAYMENT_FAILED" {
		return code
	}
	return "PREPARATION_FAILED"
}

// CardReview contains observation facts, not a payment authorization. Unknown
// balance currency never turns into USD or a PHP affordability conclusion.
type CardReview struct {
	CardBindingVerified       bool   `json:"cardBindingVerified"`
	Last4                     string `json:"last4"`
	AvailableAmount           string `json:"availableAmount"`
	Currency                  string `json:"currency"`
	BalanceCurrencyKnown      bool   `json:"balanceCurrencyKnown"`
	BalanceComparable         bool   `json:"balanceComparable"`
	BalanceSufficient         *bool  `json:"balanceSufficient,omitempty"`
	MerchantRestrictionsKnown bool   `json:"merchantRestrictionsKnown"`
	BillingSource             string `json:"billingSource"`
}

type Report struct {
	checkout.PrepaymentReport
	InputValid          bool        `json:"inputValid"`
	Card                *CardReview `json:"card,omitempty"`
	Unconfirmed         []string    `json:"unconfirmed,omitempty"`
	FundingActionsTaken int         `json:"fundingActionsTaken"`
}

func Failure(code string) Report {
	if !preparationCodes[code] && SafeCode(fail(code)) == "PREPARATION_FAILED" {
		code = "PREPARATION_FAILED"
	}
	return Report{PrepaymentReport: checkout.PrepaymentReport{
		Operation: "prepare-payment", Code: code, Stage: "input", EvidenceSource: "none",
	}}
}

func Check(input Input) Report {
	result := Failure("PREPARATION_FAILED")
	if len(input.session) == 0 || input.cardID <= 0 || input.fingerprint == "" {
		return result
	}
	result.Code, result.Stage, result.InputValid = "PREPARATION_INPUT_CHECKED", "input_validated", true
	result.TargetFingerprint = input.fingerprint
	return result
}

var fingerprintPattern = regexp.MustCompile(`^[a-f0-9]{16}$`)
var progressPhases = map[string]bool{
	"browser_starting": true, "restoring_session": true, "opening_checkout": true,
	"resolving_billing": true, "filling_billing": true, "reviewing_quote": true,
	"resolving_card": true, "filling_fields": true, "checking_final_state": true,
	"review_ready": true, "browser_closed": true,
}

func IsProgressPhase(value string) bool { return progressPhases[value] }

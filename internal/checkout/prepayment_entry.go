package checkout

import (
	_ "embed"
	"encoding/json"
	"strings"

	"kwmembership/internal/sessionrestore"
)

// The local prepayment entry has its own reviewed account adapter. It must not
// alter the legacy worker's checkout script through textual guard insertion.
//
//go:embed prepayment_entry.js
var prepaymentEntryJS string

// CheckoutDiagnostic contains classifications only. Account IDs, identities,
// access tokens, private URLs and response bodies are never part of this type.
type CheckoutDiagnostic struct {
	Phase                  string `json:"phase"`
	Reason                 string `json:"reason"`
	Source                 string `json:"source"`
	SessionHTTPStatus      int    `json:"sessionHttpStatus"`
	AccountHTTPStatus      int    `json:"accountHttpStatus"`
	SubscriptionHTTPStatus int    `json:"subscriptionHttpStatus"`
	CheckoutHTTPStatus     int    `json:"checkoutHttpStatus"`
	AccountIDPresent       bool   `json:"accountIdPresent"`
	AccountBinding         string `json:"accountBinding"`
	Plan                   string `json:"plan"`
	ActiveSubscription     string `json:"activeSubscription"`
	CheckoutRequested      bool   `json:"checkoutRequested"`
}

var checkoutDiagnosticReasons = strings.Fields(`none context_invalid
session_http_rejected session_network_error session_timeout session_response_invalid
identity_changed access_token_missing account_binding_missing account_binding_invalid
account_binding_conflict account_http_rejected account_network_error account_timeout
account_response_invalid account_default_invalid account_not_found account_ambiguous
account_identity_mismatch workspace_unsupported entitlement_invalid plan_unknown
already_subscribed subscription_http_rejected subscription_network_error
subscription_timeout subscription_response_invalid subscription_plan_missing
checkout_http_rejected checkout_unauthorized checkout_network_error checkout_timeout
checkout_response_invalid checkout_created checkout_contract_unrecognized`)

func diagnosticEnum(value, fallback string, allowed ...string) string {
	for _, candidate := range allowed {
		if value == candidate {
			return value
		}
	}
	return fallback
}

func diagnosticHTTPStatus(value int) int {
	if value >= 100 && value <= 599 {
		return value
	}
	return 0
}

// SanitizeCheckoutDiagnostic is also used at the preparation/report boundary,
// so alternate executors cannot introduce arbitrary strings into diagnostics.
func SanitizeCheckoutDiagnostic(value *CheckoutDiagnostic) *CheckoutDiagnostic {
	if value == nil {
		return nil
	}
	return &CheckoutDiagnostic{
		Phase:                  diagnosticEnum(value.Phase, "", "session", "account_lookup", "subscription_lookup", "checkout_create", "complete"),
		Reason:                 diagnosticEnum(value.Reason, "none", checkoutDiagnosticReasons...),
		Source:                 diagnosticEnum(value.Source, "none", "none", "account_entitlement", "subscription"),
		SessionHTTPStatus:      diagnosticHTTPStatus(value.SessionHTTPStatus),
		AccountHTTPStatus:      diagnosticHTTPStatus(value.AccountHTTPStatus),
		SubscriptionHTTPStatus: diagnosticHTTPStatus(value.SubscriptionHTTPStatus),
		CheckoutHTTPStatus:     diagnosticHTTPStatus(value.CheckoutHTTPStatus),
		AccountIDPresent:       value.AccountIDPresent,
		AccountBinding:         diagnosticEnum(value.AccountBinding, "none", "none", "session", "default", "matched"),
		Plan:                   diagnosticEnum(value.Plan, "unknown", "unknown", "free", "paid"),
		ActiveSubscription:     diagnosticEnum(value.ActiveSubscription, "unknown", "unknown", "true", "false"),
		CheckoutRequested:      value.CheckoutRequested,
	}
}

func (value *CheckoutDiagnostic) confirmsFreeCheckout() bool {
	if value == nil || value.Phase != "complete" || value.Reason != "checkout_created" ||
		value.Plan != "free" || (value.ActiveSubscription != "false" && value.ActiveSubscription != "unknown") ||
		!value.AccountIDPresent || !value.CheckoutRequested ||
		value.SessionHTTPStatus < 200 || value.SessionHTTPStatus >= 300 ||
		value.CheckoutHTTPStatus < 200 || value.CheckoutHTTPStatus >= 300 {
		return false
	}
	switch value.Source {
	case "account_entitlement":
		return value.AccountHTTPStatus >= 200 && value.AccountHTTPStatus < 300 &&
			value.SubscriptionHTTPStatus == 0 &&
			(value.AccountBinding == "matched" || value.AccountBinding == "default")
	case "subscription":
		return (value.AccountHTTPStatus == 404 || value.AccountHTTPStatus == 405) &&
			value.SubscriptionHTTPStatus >= 200 && value.SubscriptionHTTPStatus < 300 &&
			value.AccountBinding == "session"
	}
	return false
}

type prepaymentCheckoutEntry struct {
	plusCheckoutEntry
	Diagnostic *CheckoutDiagnostic `json:"diagnostic"`
}

func bindPlusPreparation(identity sessionrestore.Identity) (string, error) {
	mail := normalizeEmail(identity.Email)
	if mail == "" || strings.Count(prepaymentEntryJS, "__PREPAYMENT_IDENTITY__") != 1 {
		return "", prepaymentFail("PREPAYMENT_REQUEST_INVALID")
	}
	bound, err := json.Marshal(struct{ Email, UserID string }{mail, identity.UserID})
	if err != nil {
		return "", prepaymentFail("PREPAYMENT_REQUEST_INVALID")
	}
	return strings.Replace(prepaymentEntryJS, "__PREPAYMENT_IDENTITY__", string(bound), 1), nil
}

type prepaymentDiagnosticReader interface {
	CheckoutDiagnostic() *CheckoutDiagnostic
}

func (driver *chromePrepaymentDriver) CheckoutDiagnostic() *CheckoutDiagnostic {
	return SanitizeCheckoutDiagnostic(driver.entryDiagnostic)
}

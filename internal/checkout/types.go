package checkout

import (
	"context"
	"encoding/json"
	"fmt"
	"net/mail"
	"strings"
	"time"

	"kwmembership/internal/provider"
)

type Mode string

const (
	ModePreflight            Mode = "preflight"
	ModeInteractivePreflight Mode = "interactive-preflight"
	ModeSessionPreflight     Mode = "session-preflight"
	ModeSessionCheckout      Mode = "session-checkout"
	ModeCheckout             Mode = "checkout"
	ModeUpgrade              Mode = "upgrade"
)

type ActionKind string

const (
	ActionProgression ActionKind = "progression"
	ActionSubmit      ActionKind = "submit"
)

type PriceContract struct {
	ID        string
	Version   int
	Tier      string
	Currency  string
	MinAmount float64
	MaxAmount float64
}

type CardMaterial struct {
	Number      string
	CVV         string
	ExpiryMonth string
	ExpiryYear  string
}

type BillingAddress struct {
	Name       string
	Line1      string
	City       string
	State      string
	PostalCode string
	Country    string
}

type Material struct {
	Card    CardMaterial
	Billing BillingAddress
}

type Action struct {
	Kind            ActionKind
	Stage           string
	ControlID       string
	PageFingerprint string
	Page            PageFacts
}

type Permit struct{ ID string }

type ActionOutcome struct{ Continue bool }

type Handoff struct {
	Type string
	Page PageFacts
}

type HandoffHandler func(context.Context, Handoff) error

// ActionGuard is the internal seam that makes every potentially money-bearing
// click cross the durable database and transaction-snapshot fence first.
type ActionGuard interface {
	BeforeAction(context.Context, Action) (Permit, error)
	ActivateAction(context.Context, Action, Permit) error
	AfterAction(context.Context, Action, Permit) (ActionOutcome, error)
}

type ExecutionBinding struct {
	FulfillmentID       string
	FulfillmentRevision int64
	AttemptNo           int64
	PriorityClass       string
	AdapterVersion      string
}

type Request struct {
	Mode          Mode
	Stage         string
	TargetTier    string
	CheckoutURL   string
	PlanURL       string
	Cookies       []provider.SessionCookie
	ExpectedEmail string
	Session       json.RawMessage
	PriceContract PriceContract
	Material      *Material
	Guard         ActionGuard
	OnHandoff     HandoffHandler
	Binding       ExecutionBinding
}

type PageFacts struct {
	StateID         string            `json:"stateId"`
	Origin          string            `json:"origin"`
	RouteTemplate   string            `json:"routeTemplate"`
	Plan            string            `json:"plan"`
	Country         string            `json:"country"`
	Currency        string            `json:"currency"`
	DisplayedAmount *float64          `json:"displayedAmount"`
	StateMarker     string            `json:"stateMarker"`
	Fields          map[string]bool   `json:"fields"`
	Controls        map[string]string `json:"controls"`
	StructuralHash  string            `json:"structuralHash"`
}

type Result struct {
	Page             PageFacts
	SubmitActivated  bool
	Challenge        bool
	ProgressionCount int
	StoppedByGuard   bool
}

type Executor interface {
	Execute(context.Context, Request) (Result, error)
}

type Error struct {
	ErrorCode string
	Message   string
	Cause     error
	Action    *Action
	Permit    *Permit
	Uncertain bool
}

func (e *Error) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%s: %v", e.Message, e.Cause)
	}
	return e.Message
}

func (e *Error) Unwrap() error { return e.Cause }
func (e *Error) Code() string  { return e.ErrorCode }

func failure(code, message string, cause ...error) error {
	item := &Error{ErrorCode: code, Message: message}
	if len(cause) > 0 {
		item.Cause = cause[0]
	}
	return item
}

func uncertain(code, message string, action Action, permit Permit, cause error) error {
	return &Error{ErrorCode: code, Message: message, Cause: cause, Action: &action, Permit: &permit, Uncertain: true}
}

const (
	InteractiveLoginURL = "https://chatgpt.com/"
	PlusPricingURL      = "https://chatgpt.com/#pricing"
	PlanManagementURL   = "https://chatgpt.com/settings/subscription"
	maxTransitions      = 6
	pagePollInterval    = 250 * time.Millisecond
)

func normalizeEmail(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if len(value) == 0 || len(value) > 254 {
		return ""
	}
	address, err := mail.ParseAddress(value)
	if err != nil || !strings.EqualFold(address.Address, value) || !strings.Contains(value, "@") {
		return ""
	}
	return value
}

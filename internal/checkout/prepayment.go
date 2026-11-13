package checkout

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"kwmembership/internal/provider"
	"kwmembership/internal/sessionrestore"
)

// PrepaymentRequest is an acceptance request, not a worker fulfillment or a
// payment permit. No action guard, submit flag or remote debugging endpoint is
// accepted. Material is obtained only after the first quote has been reviewed.
type PrepaymentRequest struct {
	Session         json.RawMessage `json:"-"`
	Expected        QuoteTarget
	Budget          QuoteBudget
	Material        *Material                                               `json:"-"`
	ResolveBilling  func(context.Context) (*BillingAddress, error)          `json:"-"`
	ResolveMaterial func(context.Context, ReviewedQuote) (*Material, error) `json:"-"`
	HoldOpen        time.Duration
	OnProgress      func(string)           `json:"-"`
	OnReady         func(PrepaymentReport) `json:"-"`
}

func (PrepaymentRequest) String() string   { return "[protected prepayment request]" }
func (PrepaymentRequest) GoString() string { return "[protected prepayment request]" }

// PrepaymentReport contains fixed codes, bounded enums and integer money only.
// CountrySource distinguishes the requested checkout market from the actual
// billing country; a visible currency is not used to infer either country.
type PrepaymentReport struct {
	Operation               string         `json:"operation"`
	Code                    string         `json:"code"`
	Stage                   string         `json:"stage"`
	TargetFingerprint       string         `json:"targetFingerprint,omitempty"`
	CookieReadbackVerified  bool           `json:"cookieReadbackVerified"`
	IdentityMatch           bool           `json:"identityMatch"`
	Quote                   *ReviewedQuote `json:"quote,omitempty"`
	EvidenceSource          string         `json:"evidenceSource"`
	CountrySource           string         `json:"countrySource"`
	MissingFacts            []string       `json:"missingFacts,omitempty"`
	BillingVerified         bool           `json:"billingVerified"`
	FieldsVerified          bool           `json:"fieldsVerified"`
	ReadyForConfirmation    bool           `json:"readyForConfirmation"`
	BrowserOpened           bool           `json:"browserOpened"`
	BrowserClosed           bool           `json:"browserClosed"`
	PaymentAllowed          bool           `json:"paymentAllowed"`
	SubmitActivated         bool           `json:"submitActivated"`
	SubmissionBlocked       bool           `json:"submissionBlocked"`
	SubmissionFenceVerified bool           `json:"submissionFenceVerified"`
}

type PrepaymentExecutor interface {
	PreparePrepayment(context.Context, PrepaymentRequest) (PrepaymentReport, error)
}

type prepaymentError struct{ code string }

func (e *prepaymentError) Error() string { return e.code }
func (e *prepaymentError) Code() string  { return e.code }
func prepaymentFail(code string) error   { return &prepaymentError{code: code} }

var prepaymentCodes = map[string]bool{
	"PREPAYMENT_FAILED": true, "PREPAYMENT_REQUEST_INVALID": true,
	"PREPAYMENT_TARGET_UNSUPPORTED": true, "PREPAYMENT_BROWSER_UNAVAILABLE": true,
	"PREPAYMENT_BROWSER_CONFIG_INVALID": true, "PREPAYMENT_CHECKOUT_UNAVAILABLE": true,
	"PREPAYMENT_ELIGIBILITY_UNCONFIRMED": true, "PREPAYMENT_ALREADY_SUBSCRIBED": true,
	"PREPAYMENT_IDENTITY_CHANGED": true, "PREPAYMENT_CHECKOUT_CONTRACT_CHANGED": true,
	"PREPAYMENT_PAGE_UNSUPPORTED": true, "PREPAYMENT_ACTION_REQUIRED": true,
	"PREPAYMENT_QUOTE_INCOMPLETE": true, "PREPAYMENT_QUOTE_CHANGED": true,
	"PREPAYMENT_MATERIAL_UNAVAILABLE": true, "PREPAYMENT_MATERIAL_INVALID": true,
	"PREPAYMENT_BILLING_UNAVAILABLE": true, "PREPAYMENT_BILLING_CHANGED": true,
	"PREPAYMENT_FIELDS_NOT_VERIFIED": true, "PREPAYMENT_PROGRESSION_REQUIRED": true,
	"PREPAYMENT_FINAL_CONTROL_UNCONFIRMED": true, "PREPAYMENT_CANCELLED": true,
	"PREPAYMENT_TIMEOUT": true, "PREPAYMENT_CLEANUP_FAILED": true,
	"PREPAYMENT_SUBMISSION_BLOCKED": true, "PREPAYMENT_FENCE_UNAVAILABLE": true,
}

// SafePrepaymentCode never echoes provider/browser errors or input material.
func SafePrepaymentCode(err error) string {
	if errors.Is(err, context.Canceled) {
		return "PREPAYMENT_CANCELLED"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "PREPAYMENT_TIMEOUT"
	}
	var coded interface{ Code() string }
	if errors.As(err, &coded) {
		if prepaymentCodes[coded.Code()] {
			return coded.Code()
		}
		if code := sessionrestore.SafeCode(err); code != "SESSION_UNEXPECTED" {
			return code
		}
		if code := SafeQuoteCode(err); code != "QUOTE_FAILED" {
			return code
		}
	}
	return "PREPAYMENT_FAILED"
}

func PrepaymentFailure(code string) PrepaymentReport {
	if !prepaymentCodes[code] {
		code = "PREPAYMENT_FAILED"
	}
	return PrepaymentReport{Operation: "prepare-payment", Code: code, Stage: "input", EvidenceSource: "none"}
}

type prepaymentObservation struct {
	Page           inspectedPage
	Evidence       QuoteEvidence
	Missing        []string
	EvidenceSource string
	FinalControl   bool
	FinalEnabled   bool
	Progression    bool
	Challenge      bool
}

// The acceptance driver deliberately has no click, submit, request or arbitrary
// evaluation method. Its orchestration cannot activate a payment control.
type prepaymentDriver interface {
	Restore(context.Context, sessionrestore.Prepared) (sessionrestore.Report, error)
	OpenPlus(context.Context, sessionrestore.Identity) error
	Observe(context.Context) (prepaymentObservation, error)
	FillBilling(context.Context, prepaymentObservation, BillingAddress) error
	Fill(context.Context, prepaymentObservation, Material) error
	Verify(context.Context, prepaymentObservation, Material) error
	SubmissionState() submissionState
	SubmissionSignal() <-chan struct{}
	Close() error
}

type prepaymentFactory func(context.Context) (prepaymentDriver, error)

func validatePrepaymentRequest(request PrepaymentRequest) error {
	if request.Expected != (QuoteTarget{Plan: "plus", Country: "PH", Currency: "PHP"}) {
		return prepaymentFail("PREPAYMENT_TARGET_UNSUPPORTED")
	}
	min, minErr := ParseQuoteMinor(request.Budget.Min, request.Expected.Currency)
	max, maxErr := ParseQuoteMinor(request.Budget.Max, request.Expected.Currency)
	if minErr != nil || maxErr != nil || min <= 0 || max < min ||
		(request.Material == nil) == (request.ResolveMaterial == nil) ||
		request.Material != nil && request.ResolveBilling != nil ||
		request.HoldOpen < 0 || request.HoldOpen > 5*time.Minute {
		return prepaymentFail("PREPAYMENT_REQUEST_INVALID")
	}
	if request.Material != nil {
		return validatePrepaymentMaterial(*request.Material, time.Now())
	}
	return nil
}

func validatePrepaymentMaterial(material Material, now time.Time) error {
	card := material.Card
	month, errMonth := strconv.Atoi(card.ExpiryMonth)
	year, errYear := strconv.Atoi(card.ExpiryYear)
	if len(card.Number) < 12 || len(card.Number) > 19 || !decimal(card.Number) || !luhnValid(card.Number) ||
		(len(card.CVV) != 3 && len(card.CVV) != 4) || !decimal(card.CVV) ||
		len(card.ExpiryMonth) != 2 || len(card.ExpiryYear) != 4 || errMonth != nil || errYear != nil ||
		month < 1 || month > 12 || year < now.Year() || year == now.Year() && month < int(now.Month()) {
		return prepaymentFail("PREPAYMENT_MATERIAL_INVALID")
	}
	return validatePrepaymentBilling(material.Billing)
}

func validatePrepaymentBilling(billing BillingAddress) error {
	if err := provider.ValidateBillingAddress(provider.BillingAddress{
		Name: billing.Name, Line1: billing.Line1, Line2: billing.Line2,
		City: billing.City, State: billing.State,
		PostalCode: billing.PostalCode, Country: billing.Country,
	}); err != nil {
		return prepaymentFail("PREPAYMENT_MATERIAL_INVALID")
	}
	return nil
}

func reviewedPrepaymentQuote(request PrepaymentRequest, observation prepaymentObservation) (ReviewedQuote, error) {
	if observation.Challenge {
		return ReviewedQuote{}, prepaymentFail("PREPAYMENT_ACTION_REQUIRED")
	}
	if len(observation.Missing) > 0 {
		return ReviewedQuote{}, prepaymentFail("PREPAYMENT_QUOTE_INCOMPLETE")
	}
	result := ReviewQuote(QuoteReviewRequest{Expected: request.Expected, Budget: request.Budget, Evidence: observation.Evidence})
	if !result.ReviewReady || result.Quote == nil {
		return ReviewedQuote{}, quoteFail(result.Code)
	}
	return *result.Quote, nil
}

func runPrepayment(parent context.Context, request PrepaymentRequest, timeout time.Duration, factory prepaymentFactory) (report PrepaymentReport, err error) {
	report = PrepaymentReport{Operation: "prepare-payment", Code: "PREPAYMENT_FAILED", Stage: "input", EvidenceSource: "none"}
	defer func() {
		if err != nil {
			report.Code = SafePrepaymentCode(err)
			report.ReadyForConfirmation = false
		}
	}()
	if err = validatePrepaymentRequest(request); err != nil {
		return report, err
	}
	prepared, parseErr := sessionrestore.Parse(request.Session, time.Now())
	if parseErr != nil {
		return report, parseErr
	}
	if factory == nil || timeout < time.Second || timeout > 5*time.Minute {
		return report, prepaymentFail("PREPAYMENT_BROWSER_CONFIG_INVALID")
	}
	ctx, cancel := context.WithTimeout(parent, timeout+request.HoldOpen)
	defer cancel()
	operation, cancelOperation := context.WithTimeout(ctx, timeout)
	defer cancelOperation()
	stage := func(value string) {
		report.Stage = value
		if request.OnProgress != nil {
			request.OnProgress(value)
		}
	}
	stage("browser_starting")
	driver, startErr := factory(ctx)
	if startErr != nil || driver == nil {
		if driver != nil {
			_ = driver.Close()
		}
		if operation.Err() != nil {
			return report, operation.Err()
		}
		return report, prepaymentFail("PREPAYMENT_BROWSER_UNAVAILABLE")
	}
	report.BrowserOpened = true
	defer func() {
		state := driver.SubmissionState()
		report.SubmissionFenceVerified, report.SubmissionBlocked = state.Ready, state.Blocked
		if state.Blocked {
			err = prepaymentFail("PREPAYMENT_SUBMISSION_BLOCKED")
		} else if state.Failed || !state.Ready {
			err = prepaymentFail("PREPAYMENT_FENCE_UNAVAILABLE")
		}
		if closeErr := driver.Close(); closeErr != nil {
			err = prepaymentFail("PREPAYMENT_CLEANUP_FAILED")
			return
		}
		report.BrowserClosed = true
	}()
	initialFence := driver.SubmissionState()
	if initialFence.Blocked {
		return report, prepaymentFail("PREPAYMENT_SUBMISSION_BLOCKED")
	}
	if initialFence.Failed || !initialFence.Ready {
		return report, prepaymentFail("PREPAYMENT_FENCE_UNAVAILABLE")
	}
	// A paused iframe can otherwise leave Navigate waiting for the whole task
	// deadline after its bounded protection/resume setup has already failed.
	// Interrupt the active operation promptly; the deferred fence assessment
	// preserves the specific blocked/unavailable code and owns browser cleanup.
	stopFenceWatch := make(chan struct{})
	defer close(stopFenceWatch)
	go func() {
		select {
		case <-driver.SubmissionSignal():
			cancelOperation()
		case <-stopFenceWatch:
		case <-ctx.Done():
		}
	}()
	stage("restoring_session")
	login, loginErr := driver.Restore(operation, prepared)
	report.CookieReadbackVerified, report.IdentityMatch = login.CookieReadbackVerified, login.IdentityMatch
	report.TargetFingerprint = login.TargetFingerprint
	if loginErr != nil {
		return report, loginErr
	}
	if !login.RemoteVerified || !login.CookieReadbackVerified || !login.IdentityMatch {
		return report, prepaymentFail("PREPAYMENT_IDENTITY_CHANGED")
	}
	stage("opening_checkout")
	if err = driver.OpenPlus(operation, prepared.Identity()); err != nil {
		return report, err
	}
	stage("reviewing_quote")
	first, observeErr := driver.Observe(operation)
	if observeErr != nil {
		return report, observeErr
	}
	if first.Challenge {
		return report, prepaymentFail("PREPAYMENT_ACTION_REQUIRED")
	}
	// Establish a trusted target before sending even billing fields. Country
	// stays the requested market, separate from the issuer's billing country.
	if first.Evidence.Plan != request.Expected.Plan || first.Evidence.Currency != request.Expected.Currency {
		report.MissingFacts = first.Missing
		return report, quoteFail("QUOTE_TARGET_MISMATCH")
	}
	var billing *BillingAddress
	if request.Material != nil {
		copy := request.Material.Billing
		billing = &copy
	} else if request.ResolveBilling != nil {
		stage("resolving_billing")
		billing, err = request.ResolveBilling(operation)
		if err != nil || billing == nil {
			return report, prepaymentFail("PREPAYMENT_BILLING_UNAVAILABLE")
		}
	}
	if billing != nil {
		if err = validatePrepaymentBilling(*billing); err != nil {
			return report, err
		}
		stage("filling_billing")
		if err = driver.FillBilling(operation, first, *billing); err != nil {
			return report, prepaymentFail("PREPAYMENT_FIELDS_NOT_VERIFIED")
		}
		report.BillingVerified = true
		first, observeErr = driver.Observe(operation)
		if observeErr != nil {
			return report, observeErr
		}
		stage("reviewing_quote")
	}
	report.EvidenceSource, report.CountrySource = first.EvidenceSource, "requested_checkout_market"
	report.MissingFacts = first.Missing
	quote, quoteErr := reviewedPrepaymentQuote(request, first)
	if quoteErr != nil {
		return report, quoteErr
	}
	report.Quote = &quote
	stage("resolving_card")
	material := request.Material
	if request.ResolveMaterial != nil {
		material, err = request.ResolveMaterial(operation, quote)
		if err != nil || material == nil {
			return report, prepaymentFail("PREPAYMENT_MATERIAL_UNAVAILABLE")
		}
	}
	if err = validatePrepaymentMaterial(*material, time.Now()); err != nil {
		return report, err
	}
	if billing != nil && *billing != material.Billing {
		return report, prepaymentFail("PREPAYMENT_BILLING_CHANGED")
	}
	stage("filling_fields")
	if err = driver.Fill(operation, first, *material); err != nil {
		return report, prepaymentFail("PREPAYMENT_FIELDS_NOT_VERIFIED")
	}
	stage("checking_final_state")
	final, finalErr := driver.Observe(operation)
	if finalErr != nil {
		return report, finalErr
	}
	report.MissingFacts = final.Missing
	finalQuote, finalQuoteErr := reviewedPrepaymentQuote(request, final)
	if finalQuoteErr != nil {
		return report, finalQuoteErr
	}
	// The quote was reviewed after billing entry. A subsequent card fill must
	// not silently accept a new price, even if that price is inside the budget.
	report.Quote, report.EvidenceSource = &finalQuote, final.EvidenceSource
	if finalQuote != quote {
		return report, prepaymentFail("PREPAYMENT_QUOTE_CHANGED")
	}
	if err = driver.Verify(operation, final, *material); err != nil {
		return report, prepaymentFail("PREPAYMENT_FIELDS_NOT_VERIFIED")
	}
	report.FieldsVerified = true
	if final.Progression {
		return report, prepaymentFail("PREPAYMENT_PROGRESSION_REQUIRED")
	}
	if !final.FinalControl || !final.FinalEnabled {
		return report, prepaymentFail("PREPAYMENT_FINAL_CONTROL_UNCONFIRMED")
	}
	last, lastErr := driver.Observe(operation)
	if lastErr != nil {
		return report, lastErr
	}
	lastQuote, lastQuoteErr := reviewedPrepaymentQuote(request, last)
	if lastQuoteErr != nil || lastQuote != finalQuote || last.Progression || !last.FinalControl || !last.FinalEnabled {
		return report, prepaymentFail("PREPAYMENT_QUOTE_CHANGED")
	}
	// Reading the quote can span a rendering update. Verify again without a
	// setter so a reset field cannot be silently repaired and called accepted.
	if err = driver.Verify(operation, last, *material); err != nil {
		report.FieldsVerified = false
		return report, prepaymentFail("PREPAYMENT_FIELDS_NOT_VERIFIED")
	}
	if operation.Err() != nil {
		return report, operation.Err()
	}
	state := driver.SubmissionState()
	if state.Blocked {
		return report, prepaymentFail("PREPAYMENT_SUBMISSION_BLOCKED")
	}
	if state.Failed || !state.Ready {
		return report, prepaymentFail("PREPAYMENT_FENCE_UNAVAILABLE")
	}
	report.SubmissionFenceVerified = true
	report.Code, report.ReadyForConfirmation = "PREPAYMENT_REVIEW_READY", true
	stage("review_ready")
	if request.OnReady != nil {
		request.OnReady(report)
	}
	// Holding the isolated browser is for observation only. There is no
	// confirmation command in this API and no automatic follow-up submission.
	if request.HoldOpen > 0 {
		timer := time.NewTimer(request.HoldOpen)
		defer timer.Stop()
		select {
		case <-driver.SubmissionSignal():
			if driver.SubmissionState().Blocked {
				return report, prepaymentFail("PREPAYMENT_SUBMISSION_BLOCKED")
			}
			return report, prepaymentFail("PREPAYMENT_FENCE_UNAVAILABLE")
		case <-ctx.Done():
			return report, ctx.Err()
		case <-timer.C:
		}
		held, holdErr := driver.Observe(ctx)
		if holdErr != nil {
			return report, holdErr
		}
		heldQuote, holdQuoteErr := reviewedPrepaymentQuote(request, held)
		if holdQuoteErr != nil || heldQuote != finalQuote || held.Progression || !held.FinalControl || !held.FinalEnabled {
			return report, prepaymentFail("PREPAYMENT_QUOTE_CHANGED")
		}
		if err = driver.Verify(ctx, held, *material); err != nil {
			report.FieldsVerified = false
			return report, prepaymentFail("PREPAYMENT_FIELDS_NOT_VERIFIED")
		}
	}
	return report, nil
}

func missingPrepaymentFacts(evidence QuoteEvidence) []string {
	var missing []string
	if evidence.Plan == "" {
		missing = append(missing, "plan")
	}
	if evidence.Currency == "" {
		missing = append(missing, "currency")
	}
	if evidence.Subtotal == nil {
		missing = append(missing, "subtotal")
	}
	if evidence.Tax == nil {
		missing = append(missing, "tax")
	}
	if evidence.Discount == nil {
		missing = append(missing, "discount")
	}
	if evidence.Total == nil {
		missing = append(missing, "total")
	}
	if evidence.TaxIncluded == nil {
		missing = append(missing, "taxIncluded")
	}
	return missing
}

func sanitizedPrepaymentSource(value string) string {
	if value == "visible_dom" || value == "structured_checkout_session" {
		return value
	}
	return "none"
}

func safePrepaymentFact(value string, allowed ...string) string {
	for _, candidate := range allowed {
		if strings.EqualFold(value, candidate) {
			return candidate
		}
	}
	return ""
}

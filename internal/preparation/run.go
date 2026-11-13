package preparation

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"kwmembership/internal/checkout"
)

type Options struct {
	ChromePath  string
	ShowBrowser bool
	Timeout     time.Duration
	HoldOpen    time.Duration
}

func (o Options) Validate() error {
	if o.Timeout < time.Second || o.Timeout > 5*time.Minute || o.HoldOpen < 0 || o.HoldOpen > 5*time.Minute ||
		o.HoldOpen > 0 && !o.ShowBrowser || strings.TrimSpace(o.ChromePath) == "" {
		return fail("PREPARATION_BROWSER_CONFIG_INVALID")
	}
	return nil
}

type ExecutorFactory func(Options) (checkout.PrepaymentExecutor, error)
type Callbacks struct {
	OnProgress func(string)
	OnReady    func(Report)
}

func NewExecutor(options Options) (checkout.PrepaymentExecutor, error) {
	if options.Validate() != nil {
		return nil, fail("PREPARATION_BROWSER_CONFIG_INVALID")
	}
	path, err := filepath.Abs(options.ChromePath)
	if err != nil {
		return nil, fail("PREPARATION_BROWSER_CONFIG_INVALID")
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil, fail("PREPARATION_BROWSER_CONFIG_INVALID")
	}
	// Explicit empty callbacks suppress chromedp protocol diagnostics which can
	// contain credential values. No proxy override or attach endpoint is exposed.
	return checkout.NewChromeExecutorWithLogging(path, "", options.ShowBrowser, options.Timeout,
		options.Timeout, checkout.ChromeLogging{}), nil
}

func Run(parent context.Context, input Input, options Options, executorFactory ExecutorFactory,
	readerFactory ReaderFactory, now func() time.Time, callbacks Callbacks) (report Report, err error) {
	report = Check(input)
	defer func() {
		if err != nil {
			report.Code = SafeCode(err)
			report.ReadyForConfirmation = false
		}
	}()
	if !report.InputValid || parent == nil || options.Validate() != nil || executorFactory == nil || readerFactory == nil || now == nil {
		return report, fail("PREPARATION_ARGUMENTS_INVALID")
	}
	if parent.Err() != nil {
		return report, parent.Err()
	}
	executor, createErr := executorFactory(options)
	if createErr != nil || executor == nil {
		return report, fail("PREPARATION_BROWSER_CONFIG_INVALID")
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	resolver := &cardResolver{input: input, factory: readerFactory, now: now}
	var readyErr error
	request := checkout.PrepaymentRequest{
		Session: input.session, Expected: input.target, Budget: input.budget,
		ResolveBilling: resolver.billing, ResolveMaterial: resolver.resolve,
		HoldOpen: options.HoldOpen,
		OnProgress: func(stage string) {
			if IsProgressPhase(stage) && callbacks.OnProgress != nil {
				callbacks.OnProgress(stage)
			}
		},
		OnReady: func(observed checkout.PrepaymentReport) {
			review, err := wrapReport(input, resolver, observed, false)
			if err != nil {
				readyErr = err
				cancel()
				return
			}
			if callbacks.OnReady != nil {
				callbacks.OnReady(review)
			}
		},
	}
	observed, executeErr := executor.PreparePrepayment(ctx, request)
	report, err = wrapReport(input, resolver, observed, true)
	if readyErr != nil {
		return report, readyErr
	}
	if parent.Err() != nil {
		return report, parent.Err()
	}
	if _, _, resolveErr := resolver.summary(nil); resolveErr != nil {
		return report, resolveErr
	}
	if executeErr != nil {
		return report, fail(SafeCode(executeErr))
	}
	if err != nil {
		return report, err
	}
	if !report.ReadyForConfirmation {
		return report, fail("PREPARATION_REPORT_INVALID")
	}
	return report, nil
}

func wrapReport(input Input, resolver *cardResolver, observed checkout.PrepaymentReport, final bool) (Report, error) {
	report := Report{InputValid: true, PrepaymentReport: checkout.PrepaymentReport{
		Operation: "prepare-payment", Code: SafeCode(fail(observed.Code)), Stage: "input", EvidenceSource: "none",
		CookieReadbackVerified: observed.CookieReadbackVerified, IdentityMatch: observed.IdentityMatch,
		BillingVerified: observed.BillingVerified, FieldsVerified: observed.FieldsVerified,
		BrowserOpened: observed.BrowserOpened, BrowserClosed: observed.BrowserClosed,
		SubmitActivated:         observed.SubmitActivated,
		SubmissionBlocked:       observed.SubmissionBlocked,
		SubmissionFenceVerified: observed.SubmissionFenceVerified,
	}}
	if IsProgressPhase(observed.Stage) || observed.Stage == "input" {
		report.Stage = observed.Stage
	}
	if fingerprintPattern.MatchString(observed.TargetFingerprint) {
		report.TargetFingerprint = observed.TargetFingerprint
	}
	if observed.EvidenceSource == "visible_dom" || observed.EvidenceSource == "structured_checkout_session" {
		report.EvidenceSource = observed.EvidenceSource
	}
	if observed.CountrySource == "requested_checkout_market" {
		report.CountrySource = observed.CountrySource
	}
	for _, missing := range observed.MissingFacts {
		switch missing {
		case "plan", "country", "currency", "subtotal", "tax", "discount", "total", "taxIncluded":
			report.MissingFacts = append(report.MissingFacts, missing)
		}
	}
	if observed.Quote != nil && quoteMatchesInput(input, *observed.Quote) {
		copy := *observed.Quote
		report.Quote = &copy
	}
	var err error
	report.Card, report.Unconfirmed, err = resolver.summary(report.Quote)
	if err != nil {
		return report, err
	}
	ready := observed.ReadyForConfirmation || observed.Code == "PREPAYMENT_REVIEW_READY"
	if ready {
		if observed.Code != "PREPAYMENT_REVIEW_READY" || !observed.ReadyForConfirmation ||
			observed.PaymentAllowed || observed.SubmitActivated || observed.SubmissionBlocked || !observed.SubmissionFenceVerified || !observed.BrowserOpened || final && !observed.BrowserClosed ||
			!observed.CookieReadbackVerified || !observed.IdentityMatch || !observed.BillingVerified || !observed.FieldsVerified ||
			observed.TargetFingerprint != input.fingerprint || report.Quote == nil || report.Card == nil || len(observed.MissingFacts) != 0 ||
			report.EvidenceSource == "none" || report.CountrySource == "" {
			return report, fail("PREPARATION_REPORT_INVALID")
		}
		report.Code, report.ReadyForConfirmation = "PREPAYMENT_REVIEW_READY", true
	}
	return report, nil
}

func quoteMatchesInput(input Input, quote checkout.ReviewedQuote) bool {
	if quote.MinorUnitScale != 2 || quote.Plan != input.target.Plan || quote.Country != input.target.Country || quote.Currency != input.target.Currency {
		return false
	}
	toAmount := func(value int64) string {
		if value < 0 {
			return "invalid"
		}
		fraction := strconv.FormatInt(value%100, 10)
		if len(fraction) == 1 {
			fraction = "0" + fraction
		}
		return strconv.FormatInt(value/100, 10) + "." + fraction
	}
	subtotal, tax, discount, total := toAmount(quote.SubtotalMinor), toAmount(quote.TaxMinor), toAmount(quote.DiscountMinor), toAmount(quote.TotalMinor)
	review := checkout.ReviewQuote(checkout.QuoteReviewRequest{Expected: input.target, Budget: input.budget,
		Evidence: checkout.QuoteEvidence{Plan: quote.Plan, Country: quote.Country, Currency: quote.Currency,
			Subtotal: &subtotal, Tax: &tax, Discount: &discount, Total: &total, TaxIncluded: &quote.TaxIncluded}})
	return review.ReviewReady && review.Quote != nil && *review.Quote == quote
}

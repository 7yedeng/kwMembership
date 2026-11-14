package checkout

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/network"
	cdppage "github.com/chromedp/cdproto/page"
	cdpruntime "github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"

	"kwmembership/internal/sessionrestore"
)

// PreparePrepayment is intentionally independent of Execute's payment modes.
// It has no guard/permit and can never invoke a progression or submit control.
// It is suitable for an explicit local acceptance run; production fulfillment
// must continue to use the existing durable worker and action contracts.
func (c *ChromeExecutor) PreparePrepayment(ctx context.Context, request PrepaymentRequest) (PrepaymentReport, error) {
	if request.HoldOpen > 0 && !c.visible {
		err := prepaymentFail("PREPAYMENT_BROWSER_CONFIG_INVALID")
		return PrepaymentFailure(SafePrepaymentCode(err)), err
	}
	return runPrepayment(ctx, request, c.timeout, c.newPrepaymentDriver)
}

type chromePrepaymentDriver struct {
	owner           *ChromeExecutor
	ctx             context.Context
	cancelBrowser   context.CancelFunc
	cancelAllocator context.CancelFunc
	profile         string
	sessionBrowser  sessionrestore.SessionBrowser
	billingApplied  bool
	fence           *prepaymentFence
	once            sync.Once
	closeErr        error
	entryDiagnostic *CheckoutDiagnostic
}

func (c *ChromeExecutor) newPrepaymentDriver(parent context.Context) (prepaymentDriver, error) {
	path, err := filepath.Abs(c.path)
	if err != nil || strings.TrimSpace(c.path) == "" {
		return nil, prepaymentFail("PREPAYMENT_BROWSER_CONFIG_INVALID")
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || runtime.GOOS != "windows" && info.Mode()&0o111 == 0 {
		return nil, prepaymentFail("PREPAYMENT_BROWSER_CONFIG_INVALID")
	}
	profile, err := os.MkdirTemp("", "membership-prepayment-")
	if err != nil {
		return nil, prepaymentFail("PREPAYMENT_BROWSER_UNAVAILABLE")
	}
	// Use the small full-Chrome flag set even in optional headless acceptance;
	// never inherit default flags which could disable site-per-process.
	options := append(chromeOptions(path, c.proxyServer, true), chromedp.Flag("headless", !c.visible), chromedp.UserDataDir(profile), chromedp.Flag("no-sandbox", false), chromedp.Flag("disable-background-networking", true))
	allocator, cancelAllocator := chromedp.NewExecAllocator(parent, options...)
	quiet := func(string, ...any) {}
	browser, cancelBrowser := chromedp.NewContext(allocator,
		chromedp.WithLogf(quiet), chromedp.WithErrorf(quiet), chromedp.WithDebugf(quiet))
	driver := &chromePrepaymentDriver{owner: c, ctx: browser, cancelBrowser: cancelBrowser, cancelAllocator: cancelAllocator, profile: profile, fence: newPrepaymentFence()}
	driver.fence.protocol = newPrepaymentAutoProtocol(driver.handleFenceEvent)
	// First Run must use the browser lifetime, not a short-lived operation
	// context, or chromedp would kill the process when that operation ends.
	timer := time.AfterFunc(c.timeout, cancelBrowser)
	err = chromedp.Run(browser, network.Enable(), cdppage.Enable(), cdpruntime.Enable(), network.SetCacheDisabled(true))
	stopped := timer.Stop()
	if err != nil || !stopped {
		_ = driver.Close()
		return nil, prepaymentFail("PREPAYMENT_BROWSER_UNAVAILABLE")
	}
	driver.sessionBrowser, err = sessionrestore.UseChromeContext(browser)
	if err != nil {
		_ = driver.Close()
		return nil, prepaymentFail("PREPAYMENT_BROWSER_UNAVAILABLE")
	}
	driver.listenBrowserFence()
	driver.listenFence(browser)
	if err := driver.installFence(browser); err != nil {
		_ = driver.Close()
		return nil, prepaymentFail("PREPAYMENT_FENCE_UNAVAILABLE")
	}
	driver.fence.mainReady.Store(true)
	return driver, nil
}

func (driver *chromePrepaymentDriver) withContext(caller context.Context, action func(context.Context) error) error {
	if caller.Err() != nil {
		return caller.Err()
	}
	ctx, cancel := context.WithCancel(driver.ctx)
	stop := context.AfterFunc(caller, cancel)
	defer func() { stop(); cancel() }()
	return action(ctx)
}

func (driver *chromePrepaymentDriver) Restore(ctx context.Context, prepared sessionrestore.Prepared) (sessionrestore.Report, error) {
	return sessionrestore.Apply(ctx, prepared, driver.sessionBrowser, nil)
}

func (driver *chromePrepaymentDriver) OpenPlus(ctx context.Context, identity sessionrestore.Identity) error {
	expression, err := bindPlusPreparation(identity)
	if err != nil {
		return err
	}
	return driver.withContext(ctx, func(target context.Context) error {
		var entry prepaymentCheckoutEntry
		if err := chromedp.Run(target, chromedp.Evaluate(expression, &entry, authenticatedIdentityEvalOptions...)); err != nil {
			return prepaymentFail("PREPAYMENT_CHECKOUT_UNAVAILABLE")
		}
		driver.entryDiagnostic = SanitizeCheckoutDiagnostic(entry.Diagnostic)
		switch entry.ErrorKind {
		case "":
		case "identity_changed":
			return prepaymentFail("PREPAYMENT_IDENTITY_CHANGED")
		case "eligibility_unconfirmed":
			return prepaymentFail("PREPAYMENT_ELIGIBILITY_UNCONFIRMED")
		case "already_subscribed":
			return prepaymentFail("PREPAYMENT_ALREADY_SUBSCRIBED")
		default:
			return prepaymentFail("PREPAYMENT_CHECKOUT_UNAVAILABLE")
		}
		// Validate original facts before normalizing them for display.
		if !entry.Diagnostic.confirmsFreeCheckout() {
			return prepaymentFail("PREPAYMENT_CHECKOUT_CONTRACT_CHANGED")
		}
		checkoutURL, resolveErr := resolvePlusCheckoutEntry(entry.plusCheckoutEntry)
		if resolveErr != nil {
			driver.entryDiagnostic.Reason = "checkout_contract_unrecognized"
			return prepaymentFail("PREPAYMENT_CHECKOUT_CONTRACT_CHANGED")
		}
		if err := navigate(target, checkoutURL); err != nil {
			return prepaymentFail("PREPAYMENT_CHECKOUT_UNAVAILABLE")
		}
		return nil
	})
}

type prepaymentDOM struct {
	Origin       string  `json:"origin"`
	Plan         string  `json:"plan"`
	Currency     string  `json:"currency"`
	Subtotal     *string `json:"subtotal"`
	Tax          *string `json:"tax"`
	Discount     *string `json:"discount"`
	Total        *string `json:"total"`
	TaxIncluded  *bool   `json:"taxIncluded"`
	FinalControl bool    `json:"finalControl"`
	FinalEnabled bool    `json:"finalEnabled"`
	Progression  bool    `json:"progression"`
	Challenge    bool    `json:"challenge"`
	Source       string  `json:"source"`
}

func observationFromDOM(dom prepaymentDOM, frames []frameFact, origin, route string) (prepaymentObservation, error) {
	if !isCheckoutRoute(origin, route) || dom.Origin != origin {
		return prepaymentObservation{}, prepaymentFail("PREPAYMENT_PAGE_UNSUPPORTED")
	}
	fields := map[string]bool{}
	var fieldFrames []frameFields
	for _, frame := range frames {
		if frame.Origin != "https://chatgpt.com" && frame.Origin != "https://pay.openai.com" && frame.Origin != "https://js.stripe.com" {
			continue
		}
		if anyTrue(frame.Fields) {
			fieldFrames = append(fieldFrames, frameFields{FrameID: frame.FrameID, TargetContext: frame.TargetContext, Fields: frame.Fields})
		}
		for key, present := range frame.Fields {
			fields[key] = fields[key] || present
		}
	}
	evidence := QuoteEvidence{Plan: safePrepaymentFact(dom.Plan, "plus", "go", "pro", "pro lite"), Country: "PH",
		Currency: safePrepaymentFact(dom.Currency, "PHP", "EGP", "USD"), Subtotal: dom.Subtotal, Tax: dom.Tax, Discount: dom.Discount, Total: dom.Total, TaxIncluded: dom.TaxIncluded}
	return prepaymentObservation{
		Page:     inspectedPage{PageFacts: PageFacts{Origin: origin, RouteTemplate: route, Plan: evidence.Plan, Country: "PH", Currency: evidence.Currency, Fields: fields}, fieldFrames: fieldFrames},
		Evidence: evidence, Missing: missingPrepaymentFacts(evidence), EvidenceSource: sanitizedPrepaymentSource(dom.Source),
		FinalControl: dom.FinalControl, FinalEnabled: dom.FinalEnabled, Progression: dom.Progression, Challenge: dom.Challenge,
	}, nil
}

func (driver *chromePrepaymentDriver) observeOnce(ctx context.Context) (prepaymentObservation, error) {
	state := driver.SubmissionState()
	if state.Blocked {
		return prepaymentObservation{}, prepaymentFail("PREPAYMENT_SUBMISSION_BLOCKED")
	}
	if state.Failed {
		return prepaymentObservation{}, prepaymentFail("PREPAYMENT_FENCE_UNAVAILABLE")
	}
	if !state.Ready {
		return prepaymentObservation{}, prepaymentFail("PREPAYMENT_FENCE_PENDING")
	}
	var observation prepaymentObservation
	err := driver.withContext(ctx, func(target context.Context) error {
		var location string
		if err := chromedp.Run(target, chromedp.Location(&location)); err != nil {
			return prepaymentFail("PREPAYMENT_PAGE_UNSUPPORTED")
		}
		parsed, err := allowedCheckoutURL(location)
		if err != nil {
			return prepaymentFail("PREPAYMENT_PAGE_UNSUPPORTED")
		}
		var dom prepaymentDOM
		if err := chromedp.Run(target, chromedp.Evaluate(observePrepaymentDOMJS, &dom)); err != nil {
			return prepaymentFail("PREPAYMENT_PAGE_UNSUPPORTED")
		}
		frames, err := driver.inspectPrepaymentFrames(target)
		if err != nil {
			return prepaymentFail("PREPAYMENT_PAGE_UNSUPPORTED")
		}
		observation, err = observationFromDOM(dom, frames, parsed.Scheme+"://"+parsed.Host, routeTemplate(parsed))
		return err
	})
	return observation, err
}

// Observe waits for a stable semantic snapshot, not an injected test attribute.
// Partial facts are returned explicitly after the visible form has stabilized.
func (driver *chromePrepaymentDriver) Observe(ctx context.Context) (prepaymentObservation, error) {
	return waitPrepaymentObservation(ctx, pageTimeout, func() bool { return driver.billingApplied }, driver.observeOnce)
}

func waitPrepaymentObservation(ctx context.Context, timeout time.Duration, billingApplied func() bool, inspect func(context.Context) (prepaymentObservation, error)) (prepaymentObservation, error) {
	deadline := time.Now().Add(timeout)
	var last prepaymentObservation
	var previous string
	stable := 0
	latestReadSucceeded := false
	for time.Now().Before(deadline) {
		current, err := inspect(ctx)
		if err != nil && (SafePrepaymentCode(err) == "PREPAYMENT_SUBMISSION_BLOCKED" || SafePrepaymentCode(err) == "PREPAYMENT_FENCE_UNAVAILABLE") {
			return current, err
		}
		if err == nil {
			latestReadSucceeded = true
			last = current
			if current.Challenge {
				return current, nil
			}
			encoded, _ := json.Marshal(struct {
				Evidence                    QuoteEvidence
				Fields                      map[string]bool
				Final, Enabled, Progression bool
			}{current.Evidence, current.Page.Fields, current.FinalControl, current.FinalEnabled, current.Progression})
			key := string(encoded)
			if key == previous {
				stable++
			} else {
				stable, previous = 0, key
			}
			if stable >= 4 && (current.FinalControl || current.Progression) && anyTrue(current.Page.Fields) &&
				(!billingApplied() || len(current.Missing) == 0) {
				return current, nil
			}
		} else {
			// A renderer/navigation error breaks continuity. A prior good
			// quote must never survive newer unreadable browser state.
			latestReadSucceeded = false
			stable, previous = 0, ""
		}
		select {
		case <-ctx.Done():
			return last, ctx.Err()
		case <-time.After(pagePollInterval):
		}
	}
	if latestReadSucceeded && last.EvidenceSource != "" && len(last.Missing) > 0 {
		// Preserve only a fresh incomplete report so the caller can explain
		// missing facts. A complete but unstable quote is not acceptable.
		return last, nil
	}
	return last, prepaymentFail("PREPAYMENT_PAGE_UNSUPPORTED")
}

func (driver *chromePrepaymentDriver) Fill(ctx context.Context, page prepaymentObservation, material Material) error {
	if err := driver.requireSubmissionFence(); err != nil {
		return err
	}
	return driver.withContext(ctx, func(target context.Context) error {
		filled, err := processPrepaymentFields(target, page.Page, material, fillFrameFunction, false)
		if err != nil {
			return err
		}
		return validateFilledMaterial(material, filled)
	})
}

func (driver *chromePrepaymentDriver) FillBilling(ctx context.Context, page prepaymentObservation, billing BillingAddress) error {
	if err := driver.requireSubmissionFence(); err != nil {
		return err
	}
	return driver.withContext(ctx, func(target context.Context) error {
		filled, err := processPrepaymentFields(target, page.Page, Material{Billing: billing}, fillBillingFrameFunction, true)
		if err != nil {
			return err
		}
		if !filled["billingName"] || !filled["billingCountry"] || billing.Line2 != "" && !filled["billingLine2"] {
			return prepaymentFail("PREPAYMENT_FIELDS_NOT_VERIFIED")
		}
		driver.billingApplied = true
		return nil
	})
}

func (driver *chromePrepaymentDriver) Verify(ctx context.Context, page prepaymentObservation, material Material) error {
	if err := driver.requireSubmissionFence(); err != nil {
		return err
	}
	return driver.withContext(ctx, func(target context.Context) error {
		filled, err := processPrepaymentFields(target, page.Page, material, verifyFrameFunction, false)
		if err != nil {
			return err
		}
		return validateFilledMaterial(material, filled)
	})
}

func processPrepaymentFields(ctx context.Context, page inspectedPage, material Material, function string, billingOnly bool) (map[string]bool, error) {
	values := checkoutFillValues(material)
	filled := map[string]bool{}
	for _, frame := range page.fieldFrames {
		fragment := map[string]string{}
		for key, present := range frame.Fields {
			if billingOnly && !strings.HasPrefix(key, "billing") {
				continue
			}
			if value, known := values[key]; present && known {
				fragment[key] = value
			}
		}
		if len(fragment) == 0 {
			continue
		}
		if !billingOnly && frame.Fields["expiry"] {
			fragment["expiryYear"] = material.Card.ExpiryYear
		}
		payload, _ := json.Marshal(fragment)
		var result fillResult
		if err := evaluatePrepaymentFrame(ctx, frame, function+"("+string(payload)+")", &result, false); err != nil {
			return nil, prepaymentFail("PREPAYMENT_FIELDS_NOT_VERIFIED")
		}
		if err := collectFilledFields(result, fragment, filled); err != nil {
			return nil, prepaymentFail("PREPAYMENT_FIELDS_NOT_VERIFIED")
		}
	}
	return filled, nil
}

func (driver *chromePrepaymentDriver) Close() error {
	driver.once.Do(func() {
		driver.cancelBrowser()
		driver.cancelAllocator()
		if err := retryPrepaymentProfileRemoval(func() error {
			err := os.RemoveAll(driver.profile)
			if err != nil {
				driver.fence.note("cleanup_retry", err)
			}
			return err
		}, time.Sleep); err != nil {
			driver.closeErr = prepaymentFail("PREPAYMENT_CLEANUP_FAILED")
		}
	})
	return driver.closeErr
}

// Windows may briefly retain utility-process file handles after the owned
// browser has exited. Retry only this task's own temporary profile, with a
// fixed 2.9-second wait bound; a persistent failure still revokes success.
func retryPrepaymentProfileRemoval(remove func() error, pause func(time.Duration)) error {
	var err error
	for attempt := 0; attempt < 30; attempt++ {
		if err = remove(); err == nil {
			return nil
		}
		if attempt < 29 {
			pause(100 * time.Millisecond)
		}
	}
	return err
}

var _ PrepaymentExecutor = (*ChromeExecutor)(nil)

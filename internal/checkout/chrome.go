package checkout

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/network"
	cdppage "github.com/chromedp/cdproto/page"
	cdpruntime "github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"

	"kwmembership/internal/provider"
)

const pageTimeout = 30 * time.Second

type ChromeExecutor struct {
	path    string
	timeout time.Duration
}

func NewChromeExecutor(path string, timeout time.Duration) *ChromeExecutor {
	return &ChromeExecutor{path: path, timeout: timeout}
}

func chromeOptions(path string) []chromedp.ExecAllocatorOption {
	return append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.ExecPath(path),
		chromedp.Flag("incognito", true),
		chromedp.Flag("disable-extensions", true),
		chromedp.Flag("disable-sync", true),
		chromedp.WindowSize(1280, 900),
	)
}

// VerifyChrome launches an isolated headless process without visiting the
// network. Deployment checks use it to catch missing libraries or sandbox
// problems before the worker can claim an order.
func VerifyChrome(ctx context.Context, path string) error {
	allocator, cancelAllocator := chromedp.NewExecAllocator(ctx, chromeOptions(path)...)
	defer cancelAllocator()
	browser, cancelBrowser := chromedp.NewContext(allocator)
	defer cancelBrowser()
	var userAgent string
	if err := chromedp.Run(browser, chromedp.Navigate("about:blank"), chromedp.Evaluate(`navigator.userAgent`, &userAgent)); err != nil {
		return failure("HEADLESS_BROWSER_UNAVAILABLE", "launch headless Chrome", err)
	}
	if !strings.Contains(userAgent, "Chrome") {
		return failure("HEADLESS_BROWSER_UNAVAILABLE", "headless browser identity is not Chrome")
	}
	return nil
}

type frameFact struct {
	FrameID         cdp.FrameID       `json:"-"`
	Origin          string            `json:"origin"`
	RouteTemplate   string            `json:"routeTemplate"`
	Plan            string            `json:"plan"`
	Country         string            `json:"country"`
	Currency        string            `json:"currency"`
	DisplayedAmount *float64          `json:"displayedAmount"`
	StateMarker     string            `json:"stateMarker"`
	Fields          map[string]bool   `json:"fields"`
	Controls        map[string]string `json:"controls"`
}

type frameFields struct {
	FrameID cdp.FrameID
	Fields  map[string]bool
}

type inspectedPage struct {
	PageFacts
	controlFrames map[string]cdp.FrameID
	fieldFrames   []frameFields
}

type fillResult struct {
	Accepted bool     `json:"accepted"`
	Filled   []string `json:"filled"`
}

type activationResult struct {
	Activated bool `json:"activated"`
}

func (c *ChromeExecutor) Execute(parent context.Context, request Request) (Result, error) {
	if err := validateRequest(request); err != nil {
		return Result{}, err
	}
	ctx, cancel := context.WithTimeout(parent, c.timeout)
	defer cancel()
	allocator, cancelAllocator := chromedp.NewExecAllocator(ctx, chromeOptions(c.path)...)
	defer cancelAllocator()
	browser, cancelBrowser := chromedp.NewContext(allocator)
	defer cancelBrowser()
	if err := chromedp.Run(browser,
		network.Enable(),
		cdppage.Enable(),
		cdpruntime.Enable(),
		network.SetCacheDisabled(true),
		chromedp.ActionFunc(func(target context.Context) error { return setCookies(target, request.Cookies) }),
	); err != nil {
		return Result{}, failure("HEADLESS_BROWSER_UNAVAILABLE", "start headless browser", err)
	}
	if request.Mode == ModeUpgrade {
		return c.executeUpgrade(browser, request)
	}
	if err := navigate(browser, request.CheckoutURL); err != nil {
		return Result{}, err
	}
	page, err := c.waitPage(browser, request, "checkout", "")
	if err != nil {
		return Result{}, err
	}
	if request.Mode == ModePreflight {
		return Result{Page: page.PageFacts, Challenge: page.StateID == "PAYMENT_ACTION_REQUIRED"}, nil
	}
	return c.executeCheckout(browser, request, page, 0)
}

func validateRequest(request Request) error {
	if request.Mode != ModePreflight && request.Mode != ModeCheckout && request.Mode != ModeUpgrade {
		return failure("CHECKOUT_REQUEST_INVALID", "checkout mode is invalid")
	}
	if request.Stage != "plus" && request.Stage != "upgrade" {
		return failure("CHECKOUT_REQUEST_INVALID", "checkout stage is invalid")
	}
	if !map[string]bool{"plus": true, "x5": true, "x20": true}[request.TargetTier] {
		return failure("CHECKOUT_REQUEST_INVALID", "target membership tier is invalid")
	}
	contract := request.PriceContract
	if contract.ID == "" || contract.Version < 1 || contract.Currency != "PHP" || contract.MinAmount <= 0 || contract.MaxAmount < contract.MinAmount {
		return failure("CHECKOUT_REQUEST_INVALID", "checkout price contract is invalid")
	}
	if request.Mode == ModeUpgrade {
		if request.Stage != "upgrade" || request.TargetTier == "plus" || request.PlanURL != PlanManagementURL {
			return failure("CHECKOUT_REQUEST_INVALID", "upgrade checkout request is invalid")
		}
	} else if _, err := allowedCheckoutURL(request.CheckoutURL); err != nil {
		return err
	}
	if request.Mode != ModePreflight && (request.Material == nil || request.Guard == nil) {
		return failure("CHECKOUT_REQUEST_INVALID", "checkout material or action guard is missing")
	}
	if request.Mode != ModePreflight {
		material := request.Material
		if len(material.Card.Number) < 12 || len(material.Card.Number) > 19 || !decimal(material.Card.Number) || !luhnValid(material.Card.Number) ||
			(len(material.Card.CVV) != 3 && len(material.Card.CVV) != 4) || !decimal(material.Card.CVV) ||
			len(material.Card.ExpiryMonth) != 2 || !decimal(material.Card.ExpiryMonth) || len(material.Card.ExpiryYear) != 4 || !decimal(material.Card.ExpiryYear) ||
			material.Billing.Name == "" || material.Billing.Line1 == "" || material.Billing.City == "" ||
			material.Billing.State != "DE" || material.Billing.Country != "US" || material.Billing.PostalCode == "" {
			return failure("CHECKOUT_MATERIAL_INVALID", "checkout material is invalid")
		}
	}
	if len(request.Cookies) == 0 {
		return failure("COOKIE_PAYLOAD_INVALID", "checkout session cookies are missing")
	}
	return nil
}

func decimal(value string) bool {
	if value == "" {
		return false
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

func luhnValid(value string) bool {
	sum, doubled := 0, false
	for index := len(value) - 1; index >= 0; index-- {
		digit := int(value[index] - '0')
		if doubled {
			digit *= 2
			if digit > 9 {
				digit -= 9
			}
		}
		sum += digit
		doubled = !doubled
	}
	return sum%10 == 0
}

func allowedCheckoutURL(value string) (*url.URL, error) {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Fragment != "" {
		return nil, failure("CHECKOUT_URL_INVALID", "checkout URL is invalid")
	}
	route := routeTemplate(parsed)
	if !isCheckoutRoute(parsed.Scheme+"://"+parsed.Host, route) {
		return nil, failure("CHECKOUT_URL_INVALID", "checkout URL is outside the allowlist")
	}
	return parsed, nil
}

func routeTemplate(parsed *url.URL) string {
	path := parsed.Path
	switch parsed.Scheme + "://" + parsed.Host {
	case "https://chatgpt.com":
		if path == "/checkout" || path == "/checkout/" {
			return "/checkout"
		}
		if matchPath(path, "/checkout/") {
			return "/checkout/{id}"
		}
		if path == "/settings/subscription" || path == "/settings/subscription/" {
			return "/settings/subscription"
		}
		if path == "/settings/billing" || path == "/settings/billing/" {
			return "/settings/billing"
		}
		if path == "/account/billing/overview" || path == "/account/billing/overview/" {
			return "/account/billing/overview"
		}
	case "https://pay.openai.com":
		if matchPath(path, "/checkout/") {
			return "/checkout/{id}"
		}
		trimmed := strings.TrimSuffix(path, "/")
		if matchPath(path, "/pay/") || strings.HasPrefix(trimmed, "/c/pay/") && safeRouteID(strings.TrimPrefix(trimmed, "/c/pay/")) {
			return "/pay/{id}"
		}
	}
	return ""
}

func matchPath(path, prefix string) bool {
	trimmed := strings.TrimSuffix(path, "/")
	return strings.HasPrefix(trimmed, prefix) && safeRouteID(strings.TrimPrefix(trimmed, prefix))
}

func safeRouteID(value string) bool {
	if value == "" || len(value) > 200 {
		return false
	}
	for _, char := range value {
		if !(char >= 'a' && char <= 'z') && !(char >= 'A' && char <= 'Z') && !(char >= '0' && char <= '9') && char != '_' && char != '-' {
			return false
		}
	}
	return true
}

func isCheckoutRoute(origin, route string) bool {
	return origin == "https://chatgpt.com" && (route == "/checkout" || route == "/checkout/{id}") ||
		origin == "https://pay.openai.com" && (route == "/checkout/{id}" || route == "/pay/{id}")
}

func setCookies(ctx context.Context, cookies []provider.SessionCookie) error {
	for _, cookie := range cookies {
		params := network.SetCookie(cookie.Name, cookie.Value).
			WithURL("https://chatgpt.com/").WithDomain(cookie.Domain).WithPath(cookie.Path).
			WithSecure(cookie.Secure).WithHTTPOnly(cookie.HTTPOnly)
		switch cookie.SameSite {
		case "none":
			params = params.WithSameSite(network.CookieSameSiteNone)
		case "lax":
			params = params.WithSameSite(network.CookieSameSiteLax)
		case "strict":
			params = params.WithSameSite(network.CookieSameSiteStrict)
		}
		if cookie.Expiration != nil {
			whole, fraction := int64(*cookie.Expiration), *cookie.Expiration-float64(int64(*cookie.Expiration))
			expires := cdp.TimeSinceEpoch(time.Unix(whole, int64(fraction*float64(time.Second))).UTC())
			params = params.WithExpires(&expires)
		}
		if err := params.Do(ctx); err != nil {
			return err
		}
	}
	return nil
}

func navigate(ctx context.Context, value string) error {
	if err := chromedp.Run(ctx, chromedp.Navigate(value)); err != nil {
		return failure("CHECKOUT_PAGE_UNAVAILABLE", "navigate checkout page", err)
	}
	return nil
}

func (c *ChromeExecutor) executeUpgrade(ctx context.Context, request Request) (Result, error) {
	if err := navigate(ctx, request.PlanURL); err != nil {
		return Result{}, err
	}
	selection, err := c.waitPage(ctx, request, "selection", "")
	if err != nil {
		return Result{}, err
	}
	if selection.StateID == "PAYMENT_ACTION_REQUIRED" {
		return Result{Page: selection.PageFacts, Challenge: true}, nil
	}
	controlKey := "upgradeX5"
	if request.TargetTier == "x20" {
		controlKey = "upgradeX20"
	}
	action := Action{Kind: ActionProgression, Stage: request.Stage, ControlID: selection.Controls[controlKey], PageFingerprint: selection.StructuralHash, Page: selection.PageFacts}
	permit, err := c.beforeAndActivate(ctx, request, selection, controlKey, action)
	if err != nil {
		return Result{}, err
	}
	next, err := c.waitPage(ctx, request, "checkout", selection.StructuralHash)
	if err != nil {
		return Result{}, uncertain("PROGRESSION_OUTCOME_UNCERTAIN", "upgrade progression outcome is uncertain", action, permit, err)
	}
	outcome, err := request.Guard.AfterAction(ctx, action, permit)
	if err != nil {
		return Result{}, uncertain("PROGRESSION_OUTCOME_UNCERTAIN", "record upgrade progression outcome", action, permit, err)
	}
	if !outcome.Continue {
		return Result{Page: next.PageFacts, ProgressionCount: 1, StoppedByGuard: true}, nil
	}
	return c.executeCheckout(ctx, request, next, 1)
}

func (c *ChromeExecutor) executeCheckout(ctx context.Context, request Request, initial inspectedPage, progressionCount int) (Result, error) {
	page := initial
	seen := map[string]bool{}
	for transition := 0; transition < maxTransitions; transition++ {
		if page.StateID == "PAYMENT_ACTION_REQUIRED" {
			return Result{Page: page.PageFacts, Challenge: true, ProgressionCount: progressionCount}, nil
		}
		if page.StateID != "PAYMENT_PROGRESSION_READY" && page.StateID != "PAYMENT_FINAL_READY" {
			return Result{}, failure("CHECKOUT_UI_UNSUPPORTED", "checkout page structure is unsupported")
		}
		key := page.StateID + ":" + page.StructuralHash
		if seen[key] {
			return Result{}, failure("PAYMENT_REPEATED_STATE", "checkout page repeated the same state")
		}
		seen[key] = true
		if err := c.fillMaterial(ctx, page, *request.Material); err != nil {
			return Result{}, err
		}
		refreshed, err := c.inspect(ctx, request, "checkout")
		if err != nil || (refreshed.StateID != "PAYMENT_PROGRESSION_READY" && refreshed.StateID != "PAYMENT_FINAL_READY") {
			return Result{}, failure("PAYMENT_STATE_CHANGED_DURING_FILL", "checkout page changed while filling", err)
		}
		page = refreshed
		if page.StateID == "PAYMENT_PROGRESSION_READY" {
			action := Action{Kind: ActionProgression, Stage: request.Stage, ControlID: page.Controls["progression"], PageFingerprint: page.StructuralHash, Page: page.PageFacts}
			permit, err := c.beforeAndActivate(ctx, request, page, "progression", action)
			if err != nil {
				return Result{}, err
			}
			next, err := c.waitPage(ctx, request, "checkout", page.StructuralHash)
			if err != nil {
				return Result{}, uncertain("PROGRESSION_OUTCOME_UNCERTAIN", "checkout progression outcome is uncertain", action, permit, err)
			}
			outcome, err := request.Guard.AfterAction(ctx, action, permit)
			if err != nil {
				return Result{}, uncertain("PROGRESSION_OUTCOME_UNCERTAIN", "record checkout progression outcome", action, permit, err)
			}
			progressionCount++
			if !outcome.Continue {
				return Result{Page: next.PageFacts, ProgressionCount: progressionCount, StoppedByGuard: true}, nil
			}
			page = next
			continue
		}
		action := Action{Kind: ActionSubmit, Stage: request.Stage, ControlID: page.Controls["submit"], PageFingerprint: page.StructuralHash, Page: page.PageFacts}
		permit, err := c.beforeAndActivate(ctx, request, page, "submit", action)
		if err != nil {
			return Result{}, err
		}
		if _, err := request.Guard.AfterAction(ctx, action, permit); err != nil {
			return Result{}, uncertain("PAYMENT_OUTCOME_UNCERTAIN", "record checkout submission outcome", action, permit, err)
		}
		return Result{Page: page.PageFacts, SubmitActivated: true, ProgressionCount: progressionCount}, nil
	}
	return Result{}, failure("PAYMENT_TRANSITION_LIMIT", "checkout transition limit reached")
}

func (c *ChromeExecutor) beforeAndActivate(ctx context.Context, request Request, page inspectedPage, controlKey string, action Action) (Permit, error) {
	permit, err := request.Guard.BeforeAction(ctx, action)
	if err != nil {
		return Permit{}, err
	}
	frameID, ok := page.controlFrames[controlKey]
	if !ok || action.ControlID == "" {
		return Permit{}, uncertain("PERMIT_ACTIVATION_UNCERTAIN", "authorized checkout control is missing", action, permit, nil)
	}
	var result activationResult
	payload, _ := json.Marshal(action.ControlID)
	if err := evaluateFrame(ctx, frameID, activateControlFunction+"("+string(payload)+")", &result, true); err != nil || !result.Activated {
		if err == nil {
			err = errors.New("checkout control was not activated")
		}
		return Permit{}, uncertain("PERMIT_ACTIVATION_UNCERTAIN", "activate authorized checkout control", action, permit, err)
	}
	return permit, nil
}

func (c *ChromeExecutor) fillMaterial(ctx context.Context, page inspectedPage, material Material) error {
	values := map[string]string{
		"cardNumber": material.Card.Number, "expiry": material.Card.ExpiryMonth + "/" + material.Card.ExpiryYear[len(material.Card.ExpiryYear)-2:],
		"expiryMonth": material.Card.ExpiryMonth, "expiryYear": material.Card.ExpiryYear, "cvc": material.Card.CVV,
		"billingName": material.Billing.Name, "billingLine1": material.Billing.Line1, "billingCity": material.Billing.City,
		"billingState": material.Billing.State, "billingCountry": material.Billing.Country, "billingPostal": material.Billing.PostalCode,
	}
	filled := map[string]bool{}
	for _, frame := range page.fieldFrames {
		fragment := map[string]string{}
		for key, present := range frame.Fields {
			if present {
				fragment[key] = values[key]
			}
		}
		if len(fragment) == 0 {
			continue
		}
		payload, _ := json.Marshal(fragment)
		var result fillResult
		if err := evaluateFrame(ctx, frame.FrameID, fillFrameFunction+"("+string(payload)+")", &result, false); err != nil {
			return failure("PAYMENT_FIELDS_NOT_FILLED", "fill checkout fields", err)
		}
		for _, key := range result.Filled {
			filled[key] = true
		}
	}
	cardComplete := filled["cardNumber"] && filled["cvc"] && (filled["expiry"] || filled["expiryMonth"] && filled["expiryYear"])
	billingComplete := filled["billingName"] && filled["billingCountry"] && filled["billingPostal"]
	if !cardComplete || !billingComplete {
		return failure("PAYMENT_FIELDS_NOT_FILLED", "required checkout fields were not filled")
	}
	return nil
}

func (c *ChromeExecutor) waitPage(ctx context.Context, request Request, purpose, previousHash string) (inspectedPage, error) {
	deadline := time.Now().Add(pageTimeout)
	var last inspectedPage
	for time.Now().Before(deadline) {
		page, err := c.inspect(ctx, request, purpose)
		if err == nil {
			last = page
			ready := page.StateID != "UNKNOWN_PAYMENT_STATE"
			if ready && (previousHash == "" || page.StructuralHash != previousHash || page.StateID == "PAYMENT_ACTION_REQUIRED") {
				return page, nil
			}
		}
		select {
		case <-ctx.Done():
			return inspectedPage{}, failure("CHECKOUT_PAGE_TIMEOUT", "checkout page timed out", ctx.Err())
		case <-time.After(pagePollInterval):
		}
	}
	if last.StateID != "" {
		return last, failure("CHECKOUT_UI_UNSUPPORTED", "checkout page structure is unsupported")
	}
	return inspectedPage{}, failure("CHECKOUT_PAGE_TIMEOUT", "checkout page timed out")
}

func (c *ChromeExecutor) inspect(ctx context.Context, request Request, purpose string) (inspectedPage, error) {
	var location string
	if err := chromedp.Run(ctx, chromedp.Location(&location)); err != nil {
		return inspectedPage{}, err
	}
	parsed, err := url.Parse(location)
	if err != nil {
		return inspectedPage{}, err
	}
	topOrigin := parsed.Scheme + "://" + parsed.Host
	frames, err := inspectFrames(ctx)
	if err != nil {
		return inspectedPage{}, err
	}
	return mergeFacts(frames, topOrigin, routeTemplate(parsed), request, purpose)
}

func inspectFrames(ctx context.Context) ([]frameFact, error) {
	var facts []frameFact
	err := chromedp.Run(ctx, chromedp.ActionFunc(func(target context.Context) error {
		tree, err := cdppage.GetFrameTree().Do(target)
		if err != nil {
			return err
		}
		for _, frameID := range flattenFrames(tree) {
			var fact frameFact
			if err := evaluateFrameDirect(target, frameID, inspectFrameJS, &fact, false); err != nil {
				continue
			}
			fact.FrameID = frameID
			facts = append(facts, fact)
		}
		return nil
	}))
	return facts, err
}

func flattenFrames(tree *cdppage.FrameTree) []cdp.FrameID {
	if tree == nil || tree.Frame == nil {
		return nil
	}
	result := []cdp.FrameID{tree.Frame.ID}
	for _, child := range tree.ChildFrames {
		result = append(result, flattenFrames(child)...)
	}
	return result
}

func evaluateFrame(ctx context.Context, frameID cdp.FrameID, expression string, output any, userGesture bool) error {
	return chromedp.Run(ctx, chromedp.ActionFunc(func(target context.Context) error {
		return evaluateFrameDirect(target, frameID, expression, output, userGesture)
	}))
}

func evaluateFrameDirect(ctx context.Context, frameID cdp.FrameID, expression string, output any, userGesture bool) error {
	world, err := cdppage.CreateIsolatedWorld(frameID).WithWorldName("kwmembership-v1").Do(ctx)
	if err != nil {
		return err
	}
	remote, exception, err := cdpruntime.Evaluate(expression).WithContextID(world).
		WithReturnByValue(true).WithAwaitPromise(true).WithUserGesture(userGesture).Do(ctx)
	if err != nil {
		return err
	}
	if exception != nil {
		return exception
	}
	if remote == nil || len(remote.Value) == 0 {
		return errors.New("browser script returned no value")
	}
	return json.Unmarshal(remote.Value, output)
}

func mergeFacts(frames []frameFact, topOrigin, topRoute string, request Request, purpose string) (inspectedPage, error) {
	if !map[string]bool{"https://chatgpt.com": true, "https://pay.openai.com": true}[topOrigin] {
		return inspectedPage{}, failure("CHECKOUT_CONTEXT_INVALID", "checkout page origin is outside the allowlist")
	}
	fields := map[string]bool{}
	controls := map[string]string{}
	controlFrames := map[string]cdp.FrameID{}
	var fieldFrames []frameFields
	var top []frameFact
	for _, fact := range frames {
		if !map[string]bool{"https://chatgpt.com": true, "https://pay.openai.com": true, "https://js.stripe.com": true}[fact.Origin] {
			continue
		}
		for key, present := range fact.Fields {
			fields[key] = fields[key] || present
		}
		if anyTrue(fact.Fields) {
			fieldFrames = append(fieldFrames, frameFields{FrameID: fact.FrameID, Fields: fact.Fields})
		}
		if fact.Origin == topOrigin {
			top = append(top, fact)
		}
	}
	for _, key := range []string{"progression", "submit", "upgradeX5", "upgradeX20", "challenge"} {
		value, frameID, ok := uniqueControl(top, key)
		if ok {
			controls[key], controlFrames[key] = value, frameID
		}
	}
	page := inspectedPage{PageFacts: PageFacts{
		Origin: topOrigin, RouteTemplate: topRoute, Plan: uniqueFact(top, func(f frameFact) string { return f.Plan }),
		Country: uniqueFact(top, func(f frameFact) string { return f.Country }), Currency: uniqueFact(top, func(f frameFact) string { return f.Currency }),
		DisplayedAmount: uniqueAmount(top), StateMarker: uniqueFact(top, func(f frameFact) string { return f.StateMarker }),
		Fields: fields, Controls: controls,
	}, controlFrames: controlFrames, fieldFrames: fieldFrames}
	page.StateID = classify(page, request, purpose)
	hashInput, _ := json.Marshal(struct {
		StateID, Origin, Route, Plan, Country, Currency, Marker string
		Amount                                                  *float64
		Fields                                                  map[string]bool
		Controls                                                map[string]string
	}{page.StateID, page.Origin, page.RouteTemplate, page.Plan, page.Country, page.Currency, page.StateMarker, page.DisplayedAmount, page.Fields, page.Controls})
	digest := sha256.Sum256(hashInput)
	page.StructuralHash = hex.EncodeToString(digest[:])
	return page, nil
}

func classify(page inspectedPage, request Request, purpose string) string {
	expectedPlan := "plus"
	if request.Stage == "upgrade" {
		expectedPlan = map[string]string{"x5": "prolite", "x20": "pro"}[request.TargetTier]
	}
	base := page.Plan == expectedPlan && page.Country == "PH" && page.Currency == "PHP" && page.DisplayedAmount != nil &&
		*page.DisplayedAmount >= request.PriceContract.MinAmount && *page.DisplayedAmount <= request.PriceContract.MaxAmount
	if purpose == "selection" {
		base = base && page.Origin == "https://chatgpt.com" && page.RouteTemplate == "/settings/subscription"
		if !base {
			return "UNKNOWN_PAYMENT_STATE"
		}
		if page.Controls["challenge"] != "" {
			return "PAYMENT_ACTION_REQUIRED"
		}
		expected := "upgrade-x5"
		key := "upgradeX5"
		if request.TargetTier == "x20" {
			expected, key = "upgrade-x20", "upgradeX20"
		}
		if page.Controls[key] == expected && page.Controls["progression"] == "" && page.Controls["submit"] == "" {
			return "UPGRADE_SELECTION_READY"
		}
		return "UNKNOWN_PAYMENT_STATE"
	}
	base = base && isCheckoutRoute(page.Origin, page.RouteTemplate)
	if !base {
		return "UNKNOWN_PAYMENT_STATE"
	}
	if page.Controls["challenge"] != "" {
		return "PAYMENT_ACTION_REQUIRED"
	}
	cardFields := page.Fields["cardNumber"] && page.Fields["cvc"] && (page.Fields["expiry"] || page.Fields["expiryMonth"] && page.Fields["expiryYear"])
	billingCore := page.Fields["billingName"] && page.Fields["billingCountry"] && page.Fields["billingPostal"]
	addressCount := boolCount(page.Fields["billingLine1"], page.Fields["billingCity"], page.Fields["billingState"])
	if !cardFields || !billingCore || addressCount != 0 && addressCount != 3 || (page.Controls["progression"] == "") == (page.Controls["submit"] == "") {
		return "UNKNOWN_PAYMENT_STATE"
	}
	if page.Controls["progression"] != "" {
		return "PAYMENT_PROGRESSION_READY"
	}
	return "PAYMENT_FINAL_READY"
}

func anyTrue(values map[string]bool) bool {
	for _, value := range values {
		if value {
			return true
		}
	}
	return false
}

func boolCount(values ...bool) int {
	count := 0
	for _, value := range values {
		if value {
			count++
		}
	}
	return count
}

func uniqueFact(facts []frameFact, get func(frameFact) string) string {
	value := ""
	for _, fact := range facts {
		candidate := get(fact)
		if candidate == "" {
			continue
		}
		if value != "" && value != candidate {
			return ""
		}
		value = candidate
	}
	return value
}

func uniqueAmount(facts []frameFact) *float64 {
	var result *float64
	for _, fact := range facts {
		if fact.DisplayedAmount == nil {
			continue
		}
		if result != nil && *result != *fact.DisplayedAmount {
			return nil
		}
		value := *fact.DisplayedAmount
		result = &value
	}
	return result
}

func uniqueControl(facts []frameFact, key string) (string, cdp.FrameID, bool) {
	value := ""
	var frameID cdp.FrameID
	for _, fact := range facts {
		candidate := fact.Controls[key]
		if candidate == "" {
			continue
		}
		if value != "" && (value != candidate || frameID != fact.FrameID) {
			return "", "", false
		}
		value, frameID = candidate, fact.FrameID
	}
	return value, frameID, value != ""
}

var _ Executor = (*ChromeExecutor)(nil)

func (c *ChromeExecutor) String() string { return fmt.Sprintf("headless-chrome(%s)", c.path) }

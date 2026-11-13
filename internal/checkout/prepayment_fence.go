package checkout

import (
	"context"
	"errors"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/chromedp/cdproto"
	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/network"
	cdppage "github.com/chromedp/cdproto/page"
	cdpruntime "github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

const prepaymentBinding = "__kw_preconfirmation_blocked_v1"

// Known endpoint classes, not a claim to enumerate every private merchant API.
// Host-independent matching also blocks these routes in nested payment frames
// and the loopback negative tests. No URL or request body is retained or logged.
var knownConfirmationPath = regexp.MustCompile(`(?i)/(?:payment_intents|setup_intents|payment_pages)/[^/]+/confirm(?:/|$)`)

func isKnownPaymentConfirmation(rawURL string) bool {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	path := strings.TrimSuffix(strings.ToLower(parsed.Path), "/")
	return knownConfirmationPath.MatchString(path) || strings.HasSuffix(path, "/checkout-api/confirm") ||
		strings.HasSuffix(path, "/payments/confirm") || strings.HasSuffix(path, "/payment/confirm")
}

type submissionState struct{ Ready, Blocked, Failed bool }

type fenceAutoAttachment struct {
	parent   context.Context
	waiting  bool
	resuming bool
	resumed  bool
}

type fenceTarget struct {
	ctx          context.Context
	invalidate   context.CancelFunc
	iframe       bool
	installed    bool
	ready        bool
	sessionID    target.SessionID
	autoSessions map[target.SessionID]*fenceAutoAttachment
}

type prepaymentFence struct {
	blocked    atomic.Bool
	failed     atomic.Bool
	mainReady  atomic.Bool
	pending    atomic.Int64
	signal     chan struct{}
	signalOnce sync.Once
	mu         sync.Mutex
	targets    map[target.ID]*fenceTarget
	resume     *prepaymentAutoResumeRouter
	// Populated only by loopback _test.go fixtures. Production has no sink.
	diagnostic func(phase string, code int64, classification string)
}

func newPrepaymentFence() *prepaymentFence {
	return &prepaymentFence{signal: make(chan struct{}), targets: make(map[target.ID]*fenceTarget), resume: newPrepaymentAutoResumeRouter()}
}

func (fence *prepaymentFence) markBlocked() {
	fence.blocked.Store(true)
	fence.signalOnce.Do(func() { close(fence.signal) })
}
func (fence *prepaymentFence) markFailed(phase string, err error) {
	fence.note(phase, err)
	fence.failed.Store(true)
	fence.signalOnce.Do(func() { close(fence.signal) })
}

func (fence *prepaymentFence) note(phase string, err error) {
	fence.mu.Lock()
	sink := fence.diagnostic
	fence.mu.Unlock()
	if sink != nil {
		code, classification := classifyPrepaymentDiagnostic(err)
		sink(phase, code, classification)
	}
}

func classifyPrepaymentDiagnostic(err error) (int64, string) {
	if err == nil {
		return 0, "none"
	}
	if errors.Is(err, context.Canceled) {
		return 0, "cancelled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return 0, "deadline"
	}
	var protocol *cdproto.Error
	var system syscall.Errno
	var code int64
	if errors.As(err, &protocol) {
		code = protocol.Code
	} else if errors.As(err, &system) {
		code = int64(system)
	}
	// Inspect text only to select bounded categories. Never return raw text,
	// path, URL, request body, SessionID, or any caller material.
	message := strings.ToLower(err.Error())
	for _, known := range []struct{ fragment, classification string }{
		{"execution context", "execution_context_unavailable"},
		{"context with specified id", "execution_context_unavailable"},
		{"flat protocol", "flat_routing_rejected"},
		{"no session", "session_unavailable"},
		{"session with given id", "session_unavailable"},
		{"no such target", "target_unavailable"},
		{"target closed", "target_closed"},
		{"already handled", "request_already_handled"},
		{"interceptionid", "interception_unavailable"},
		{"used by another process", "profile_busy"},
		{"sharing violation", "profile_busy"},
		{"access is denied", "permission_denied"},
		{"permission denied", "permission_denied"},
	} {
		if strings.Contains(message, known.fragment) {
			return code, known.classification
		}
	}
	if code == -32601 {
		return code, "unsupported_method"
	}
	if protocol != nil {
		return code, "protocol_error"
	}
	return code, "other_error"
}

func (driver *chromePrepaymentDriver) SubmissionState() submissionState {
	if driver.fence == nil {
		return submissionState{Failed: true}
	}
	fence := driver.fence
	fence.mu.Lock()
	childrenReady := true
	for _, entry := range fence.targets {
		childrenReady = childrenReady && entry.ready
	}
	fence.mu.Unlock()
	return submissionState{Ready: fence.mainReady.Load() && fence.pending.Load() == 0 && childrenReady && !fence.failed.Load(), Blocked: fence.blocked.Load(), Failed: fence.failed.Load()}
}
func (driver *chromePrepaymentDriver) SubmissionSignal() <-chan struct{} { return driver.fence.signal }

// Discovery is browser-wide, but removal is limited to target IDs/sessions
// already attached through this owned page. No other tab is attached or closed.
func (driver *chromePrepaymentDriver) listenBrowserFence() {
	chromedp.ListenBrowser(driver.ctx, func(event any) {
		switch value := event.(type) {
		case *target.EventTargetDestroyed:
			driver.fence.removeTarget(value.TargetID)
		case *target.EventDetachedFromTarget:
			if driver.fence.removeSession(value.SessionID) && driver.ctx.Err() == nil {
				driver.fence.markFailed("browser_manual_detached", nil)
			}
		}
	})
}

func (fence *prepaymentFence) removeTarget(id target.ID) {
	fence.mu.Lock()
	fence.removeTargetLocked(id)
	fence.mu.Unlock()
}

func (fence *prepaymentFence) removeTargetLocked(id target.ID) {
	if entry := fence.targets[id]; entry != nil {
		// Cancel only the evidence facade, never the chromedp target owner.
		if entry.invalidate != nil {
			entry.invalidate()
		}
		delete(fence.targets, id)
	}
	for childID, entry := range fence.targets {
		for _, attachment := range entry.autoSessions {
			if attachment != nil && attachment.parent != nil {
				parent := chromedp.FromContext(attachment.parent)
				if parent != nil && parent.Target != nil && parent.Target.TargetID == id {
					fence.removeTargetLocked(childID)
					break
				}
			}
		}
	}
}

// Losing a manual protection session is an unknown-coverage failure. Detaching
// an automatic lifetime session only invalidates evidence: the manual Fetch
// interceptor remains alive until its owning browser closes it.
func (fence *prepaymentFence) removeSession(id target.SessionID) bool {
	if id == "" {
		return false
	}
	fence.mu.Lock()
	defer fence.mu.Unlock()
	manualLost := false
	for key, entry := range fence.targets {
		if entry.sessionID == id || entry.autoSessions[id] != nil {
			manualLost = manualLost || entry.sessionID == id
			// Context cancellation would CloseTarget. Leave lifetime ownership
			// with the parent, but never reuse this detached renderer's facts.
			fence.removeTargetLocked(key)
		}
	}
	return manualLost
}

func (driver *chromePrepaymentDriver) listenFence(ctx context.Context) {
	chromedp.ListenTarget(ctx, func(event any) {
		switch value := event.(type) {
		case *cdpruntime.EventBindingCalled:
			if value.Name == prepaymentBinding && value.Payload == "blocked-form-submit-v1" {
				driver.fence.markBlocked()
			}
		case *fetch.EventRequestPaused:
			if value.Request == nil {
				driver.fence.markFailed("fetch_request_missing", nil)
				return
			}
			blocked := isKnownPaymentConfirmation(value.Request.URL)
			if blocked {
				driver.fence.markBlocked()
			}
			go func() {
				err := chromedp.Run(ctx, chromedp.ActionFunc(func(target context.Context) error {
					if blocked {
						return fetch.FailRequest(value.RequestID, network.ErrorReasonBlockedByClient).Do(target)
					}
					return fetch.ContinueRequest(value.RequestID).Do(target)
				}))
				if err != nil && ctx.Err() == nil && driver.ctx.Err() == nil {
					driver.fence.markFailed("fetch_response", err)
				}
			}()
		case *target.EventAttachedToTarget:
			if value.TargetInfo == nil || value.TargetInfo.TargetID == "" || value.SessionID == "" {
				driver.fence.markFailed("attachment_invalid", nil)
				return
			}
			switch value.TargetInfo.Type {
			case "iframe", "worker", "service_worker", "shared_worker":
				driver.attachPrepaymentTarget(ctx, value.TargetInfo.TargetID, value.TargetInfo.Type, value.SessionID, value.WaitingForDebugger)
			default:
				// An unexpected related target cannot be called protected.
				driver.fence.markFailed("attachment_unsupported", nil)
				return
			}
		case *target.EventDetachedFromTarget:
			if driver.fence.removeSession(value.SessionID) && ctx.Err() == nil && driver.ctx.Err() == nil {
				driver.fence.markFailed("target_manual_detached", nil)
			}
		case *target.EventTargetDestroyed:
			driver.fence.removeTarget(value.TargetID)
		case *target.EventReceivedMessageFromTarget:
			driver.fence.resume.Handle(value.SessionID, value.Message)
		}
	})
}

func (driver *chromePrepaymentDriver) installFence(ctx context.Context) error {
	return chromedp.Run(ctx,
		cdpruntime.AddBinding(prepaymentBinding),
		chromedp.ActionFunc(func(target context.Context) error {
			_, err := cdppage.AddScriptToEvaluateOnNewDocument(prepaymentFormFenceJS).Do(target)
			return err
		}),
		fetch.Enable().WithPatterns([]*fetch.RequestPattern{{URLPattern: "*", RequestStage: fetch.RequestStageRequest}}),
		// Keep only the automatic lifetime channel non-flat so its original
		// wait-for-debugger session can be explicitly resumed after protection.
		// Field inspection still uses chromedp's separate flat owned session.
		target.SetAutoAttach(true, true).WithFlatten(false),
		chromedp.Evaluate(prepaymentFormFenceJS, nil),
	)
}

func (driver *chromePrepaymentDriver) installWorkerFence(ctx context.Context) error {
	// chromedp recognizes WorkerGlobalScope and initializes only available
	// domains. Workers have no forms/Page domain: protect their requests only.
	// Unsupported Fetch fails closed without disabling or replacing the worker.
	return chromedp.Run(ctx,
		fetch.Enable().WithPatterns([]*fetch.RequestPattern{{URLPattern: "*", RequestStage: fetch.RequestStageRequest}}),
		// Worker targets implement the auto-attach-only Target domain. Protect
		// nested workers recursively; a rejected command is not full coverage.
		target.SetAutoAttach(true, true).WithFlatten(false),
	)
}

func (driver *chromePrepaymentDriver) attachPrepaymentTarget(parent context.Context, id target.ID, kind string, autoSession target.SessionID, waiting bool) {
	fence := driver.fence
	fence.mu.Lock()
	if existing, exists := fence.targets[id]; exists {
		if existing.autoSessions[autoSession] == nil {
			existing.autoSessions[autoSession] = &fenceAutoAttachment{parent: parent, waiting: waiting}
		}
		existing.ready = fenceTargetReady(existing)
		fence.mu.Unlock()
		driver.resumeAutoAttachments(id, existing)
		return
	}
	entry := &fenceTarget{iframe: kind == "iframe", autoSessions: map[target.SessionID]*fenceAutoAttachment{
		autoSession: {parent: parent, waiting: waiting},
	}}
	fence.targets[id] = entry
	fence.pending.Add(1)
	fence.mu.Unlock()
	go func() {
		defer fence.pending.Add(-1)
		fence.note("child_context_start", nil)
		child, err := newPrepaymentTargetContext(driver.ctx, id)
		if err != nil {
			fence.markFailed("child_context", err)
			return
		}
		driver.listenFence(child)
		fence.note("child_install_start", nil)
		// Auto-attachment paused this renderer. Install its own interception and
		// document hooks before resuming it; no site-isolation flags are changed.
		var installErr error
		if entry.iframe {
			installErr = driver.installFence(child)
		} else {
			installErr = driver.installWorkerFence(child)
		}
		if installErr != nil {
			fence.note("child_install_result", installErr)
			fence.mu.Lock()
			stillOwned := fence.targets[id] == entry
			fence.mu.Unlock()
			if stillOwned && driver.ctx.Err() == nil {
				fence.markFailed("child_install", installErr)
			}
			return
		}
		fence.note("child_install_complete", nil)
		fence.mu.Lock()
		exists := fence.targets[id] == entry
		if exists {
			entry.ctx, entry.invalidate = context.WithCancel(child)
			entry.installed = true
			entry.sessionID = chromedp.FromContext(child).Target.SessionID
			entry.ready = fenceTargetReady(entry)
		}
		fence.mu.Unlock()
		if !exists {
			return
		}
		driver.resumeAutoAttachments(id, entry)
	}()
}

func fenceTargetReady(entry *fenceTarget) bool {
	if entry == nil || !entry.installed {
		return false
	}
	for _, attachment := range entry.autoSessions {
		if attachment == nil || attachment.waiting && !attachment.resumed {
			return false
		}
	}
	return true
}

func (driver *chromePrepaymentDriver) resumeAutoAttachments(id target.ID, entry *fenceTarget) {
	fence := driver.fence
	type pendingResume struct {
		id         target.SessionID
		attachment *fenceAutoAttachment
	}
	var work []pendingResume
	fence.mu.Lock()
	if fence.targets[id] != entry || !entry.installed {
		fence.mu.Unlock()
		return
	}
	for sessionID, attachment := range entry.autoSessions {
		if attachment.waiting && !attachment.resumed && !attachment.resuming {
			attachment.resuming = true
			fence.pending.Add(1)
			work = append(work, pendingResume{sessionID, attachment})
		}
	}
	entry.ready = fenceTargetReady(entry)
	fence.mu.Unlock()
	for _, next := range work {
		go func() {
			defer fence.pending.Add(-1)
			fence.note("original_session_resume_start", nil)
			err := fence.resume.Resume(driver.ctx, next.attachment.parent, next.id)
			fence.mu.Lock()
			stillOwned := fence.targets[id] == entry
			if stillOwned && err == nil {
				next.attachment.resumed = true
				entry.ready = fenceTargetReady(entry)
			}
			fence.mu.Unlock()
			if stillOwned && err != nil && driver.ctx.Err() == nil {
				fence.markFailed("original_session_resume", err)
			}
		}()
	}
}

func newPrepaymentTargetContext(parent context.Context, id target.ID) (context.Context, error) {
	if parent == nil || parent.Err() != nil {
		return nil, prepaymentFail("PREPAYMENT_FENCE_UNAVAILABLE")
	}
	state := chromedp.FromContext(parent)
	if state == nil || state.Browser == nil || state.Target == nil || id == "" {
		return nil, prepaymentFail("PREPAYMENT_FENCE_UNAVAILABLE")
	}
	// WithLogf/WithErrorf/WithDebugf are Browser allocation options and panic
	// when a Browser already exists. Child targets inherit its quiet logging.
	child, _ := chromedp.NewContext(parent, chromedp.WithTargetID(id))
	return child, nil
}

func (driver *chromePrepaymentDriver) frameContexts() []context.Context {
	driver.fence.mu.Lock()
	defer driver.fence.mu.Unlock()
	result := []context.Context{driver.ctx}
	for _, entry := range driver.fence.targets {
		if entry.iframe && entry.ready && entry.ctx != nil && entry.ctx.Err() == nil {
			result = append(result, entry.ctx)
		}
	}
	return result
}

func (driver *chromePrepaymentDriver) requireSubmissionFence() error {
	state := driver.SubmissionState()
	if state.Blocked {
		return prepaymentFail("PREPAYMENT_SUBMISSION_BLOCKED")
	}
	if state.Failed || !state.Ready {
		return prepaymentFail("PREPAYMENT_FENCE_UNAVAILABLE")
	}
	return nil
}

// Main-world hooks block native and event-driven form submission. Input/change
// remain available for tax calculation and field validation. The authoritative
// attempt marker is held in Go via Runtime binding, not a writable page flag.
const prepaymentFormFenceJS = `(() => {
  if (typeof HTMLFormElement === 'undefined') return;
  const report = () => { try { globalThis.__kw_preconfirmation_blocked_v1('blocked-form-submit-v1'); } catch (_) {} };
  const stop = event => { event.preventDefault(); event.stopImmediatePropagation(); report(); };
  document.addEventListener('submit', stop, true);
  for (const name of ['submit','requestSubmit']) {
    const descriptor = Object.getOwnPropertyDescriptor(HTMLFormElement.prototype,name);
    if (descriptor && !descriptor.configurable) continue;
    Object.defineProperty(HTMLFormElement.prototype,name,{value:function(){report();},writable:false,configurable:false});
  }
})()`

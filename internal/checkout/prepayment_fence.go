package checkout

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

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
	parent    context.Context
	protocol  context.Context
	parentID  target.ID
	sessionID target.SessionID
	waiting   bool
	installed bool
	resumed   bool
}

type fenceTarget struct {
	ctx            context.Context
	invalidate     context.CancelFunc
	iframe         bool
	installed      bool
	manualStarting bool
	manualReady    bool
	ready          bool
	sessionID      target.SessionID
	autoSessions   map[target.SessionID]*fenceAutoAttachment
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
	protocol   *prepaymentAutoProtocol
	// Populated only by loopback _test.go fixtures. Production has no sink.
	diagnostic func(phase string, code int64, classification string)
}

func newPrepaymentFence() *prepaymentFence {
	return &prepaymentFence{signal: make(chan struct{}), targets: make(map[target.ID]*fenceTarget)}
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
	if protocol != nil {
		message = strings.ToLower(protocol.Message)
	}
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
	dropped := fence.removeTargetLocked(id)
	fence.mu.Unlock()
	fence.dropProtocols(dropped)
}

func (fence *prepaymentFence) removeTargetLocked(id target.ID) []*fenceAutoAttachment {
	var dropped []*fenceAutoAttachment
	if entry := fence.targets[id]; entry != nil {
		// Cancel only the evidence facade, never the chromedp target owner.
		if entry.invalidate != nil {
			entry.invalidate()
		}
		for _, attachment := range entry.autoSessions {
			dropped = append(dropped, attachment)
		}
		delete(fence.targets, id)
	}
	for childID, entry := range fence.targets {
		for _, attachment := range entry.autoSessions {
			if attachment != nil && attachment.parentID == id {
				dropped = append(dropped, fence.removeTargetLocked(childID)...)
				break
			}
		}
	}
	return dropped
}

func (fence *prepaymentFence) dropProtocols(attachments []*fenceAutoAttachment) {
	if fence.protocol == nil {
		return
	}
	for _, attachment := range attachments {
		if attachment != nil {
			fence.protocol.Drop(attachment.parent, attachment.sessionID)
		}
	}
}

// Original automatic sessions carry the persistent protection. Losing one at
// any bootstrap stage is a coverage failure, even before installed becomes
// true. A manual field session loss also invalidates the evidence boundary.
func (fence *prepaymentFence) removeSession(id target.SessionID) bool {
	if id == "" {
		return false
	}
	fence.mu.Lock()
	protectionLost := false
	var dropped []*fenceAutoAttachment
	for key, entry := range fence.targets {
		if entry.sessionID == id || entry.autoSessions[id] != nil {
			protectionLost = true
			// Context cancellation would CloseTarget. Leave lifetime ownership
			// with the parent, but never reuse this detached renderer's facts.
			dropped = append(dropped, fence.removeTargetLocked(key)...)
		}
	}
	fence.mu.Unlock()
	fence.dropProtocols(dropped)
	return protectionLost
}

func (driver *chromePrepaymentDriver) listenFence(ctx context.Context) {
	executor, err := initializedPrepaymentFrameExecutor(ctx)
	if err != nil {
		driver.fence.markFailed("main_listener_context", err)
		return
	}
	executor = context.WithValue(executor, prepaymentFenceOwnerKey{}, chromedp.FromContext(ctx).Target.TargetID)
	chromedp.ListenTarget(ctx, func(event any) { driver.handleFenceEvent(executor, event) })
}

type prepaymentFenceOwnerKey struct{}

// Both root and original non-flat sessions pass their actual CDP executor.
// Never use chromedp.Run here: an auto-session inherits Context values from its
// parent, but its executor must remain the session which paused the request.
func (driver *chromePrepaymentDriver) handleFenceEvent(ctx context.Context, event any) {
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
			operation, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			var err error
			if blocked {
				err = fetch.FailRequest(value.RequestID, network.ErrorReasonBlockedByClient).Do(operation)
			} else {
				err = fetch.ContinueRequest(value.RequestID).Do(operation)
			}
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
		driver.fence.protocol.Drop(ctx, value.SessionID)
		if driver.fence.removeSession(value.SessionID) && ctx.Err() == nil && driver.ctx.Err() == nil {
			driver.fence.markFailed("target_manual_detached", nil)
		}
	case *target.EventTargetDestroyed:
		driver.fence.removeTarget(value.TargetID)
	case *target.EventReceivedMessageFromTarget:
		driver.fence.protocol.Handle(ctx, value.SessionID, value.Message)
	case prepaymentAutoProtocolFailure:
		driver.fence.markFailed("original_session_event", nil)
	}
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

func (driver *chromePrepaymentDriver) attachPrepaymentTarget(parent context.Context, id target.ID, kind string, autoSession target.SessionID, waiting bool) {
	fence := driver.fence
	parentID, _ := parent.Value(prepaymentFenceOwnerKey{}).(target.ID)
	// The carrier changes only owner metadata; the protocol router identifies
	// the parent by its actual Target/auto-session executor, never by ctx address.
	protocol, err := fence.protocol.NewSession(context.WithValue(parent, prepaymentFenceOwnerKey{}, id), autoSession)
	if err != nil {
		fence.markFailed("original_session_register", err)
		return
	}
	attachment := &fenceAutoAttachment{parent: parent, protocol: protocol, parentID: parentID,
		sessionID: autoSession, waiting: waiting}
	fence.mu.Lock()
	entry := fence.targets[id]
	if entry == nil {
		entry = &fenceTarget{iframe: kind == "iframe", autoSessions: make(map[target.SessionID]*fenceAutoAttachment)}
		fence.targets[id] = entry
	}
	if entry.autoSessions[autoSession] != nil {
		fence.mu.Unlock()
		return
	}
	entry.autoSessions[autoSession] = attachment
	entry.ready = false
	fence.pending.Add(1)
	fence.mu.Unlock()
	go func() {
		defer fence.pending.Add(-1)
		fence.note("original_bootstrap_start", nil)
		// These commands do not require a default execution context. They run
		// on the original paused session, before any manual chromedp attach.
		if err := installPrepaymentAutoFence(protocol, entry.iframe, waiting); err != nil {
			driver.failOwnedPrepaymentTarget(id, entry, "original_bootstrap", err)
			return
		}
		fence.mu.Lock()
		attachment.installed = true
		entry.installed = everyPrepaymentAutoFenceInstalled(entry)
		fence.mu.Unlock()
		fence.note("original_bootstrap_complete", nil)
		if waiting {
			fence.note("original_session_resume_start", nil)
			if err := cdpruntime.RunIfWaitingForDebugger().Do(protocol); err != nil {
				driver.failOwnedPrepaymentTarget(id, entry, "original_session_resume", err)
				return
			}
		}
		fence.mu.Lock()
		if fence.targets[id] != entry {
			fence.mu.Unlock()
			return
		}
		attachment.resumed = true
		startManual := entry.iframe && !entry.manualStarting
		if startManual {
			entry.manualStarting = true
		}
		if !entry.iframe && entry.ctx == nil {
			entry.ctx, entry.invalidate = context.WithCancel(protocol)
		}
		entry.ready = fenceTargetReady(entry)
		fence.mu.Unlock()
		if startManual {
			fence.note("default_context_wait_start", nil)
			if err := waitPrepaymentFrameContext(protocol); err != nil {
				driver.failOwnedPrepaymentTarget(id, entry, "default_context_wait", err)
				return
			}
			fence.note("manual_field_context_start", nil)
			if err := driver.attachPrepaymentFieldContext(id, entry); err != nil {
				driver.failOwnedPrepaymentTarget(id, entry, "manual_field_context", err)
				return
			}
			fence.note("manual_field_context_complete", nil)
		}
	}()
}

func fenceTargetReady(entry *fenceTarget) bool {
	if entry == nil || !entry.installed || entry.iframe && (!entry.manualReady || entry.ctx == nil) {
		return false
	}
	for _, attachment := range entry.autoSessions {
		if attachment == nil || !attachment.installed || !attachment.resumed {
			return false
		}
	}
	return true
}

func everyPrepaymentAutoFenceInstalled(entry *fenceTarget) bool {
	for _, attachment := range entry.autoSessions {
		if attachment == nil || !attachment.installed {
			return false
		}
	}
	return len(entry.autoSessions) > 0
}

func (driver *chromePrepaymentDriver) failOwnedPrepaymentTarget(id target.ID, entry *fenceTarget, phase string, err error) {
	driver.fence.note(phase+"_result", err)
	driver.fence.mu.Lock()
	stillOwned := driver.fence.targets[id] == entry
	driver.fence.mu.Unlock()
	if stillOwned && driver.ctx.Err() == nil {
		driver.fence.markFailed(phase, err)
	}
}

// The original auto-session is deliberately bootstrapped without evaluating
// JavaScript: a paused new OOPIF need not have a default context yet.
func installPrepaymentAutoFence(protocol context.Context, iframe, waiting bool) error {
	ctx, cancel := context.WithTimeout(protocol, 5*time.Second)
	defer cancel()
	if err := cdpruntime.Enable().Do(ctx); err != nil {
		return err
	}
	if iframe {
		if err := cdppage.Enable().Do(ctx); err != nil {
			return err
		}
		if err := cdpruntime.AddBinding(prepaymentBinding).Do(ctx); err != nil {
			return err
		}
		if _, err := cdppage.AddScriptToEvaluateOnNewDocument(prepaymentFormFenceJS).Do(ctx); err != nil {
			return err
		}
	}
	if err := fetch.Enable().WithPatterns([]*fetch.RequestPattern{{URLPattern: "*", RequestStage: fetch.RequestStageRequest}}).Do(ctx); err != nil {
		return err
	}
	if err := target.SetAutoAttach(true, true).WithFlatten(false).Do(ctx); err != nil {
		return err
	}
	if iframe && !waiting {
		_, exception, err := cdpruntime.Evaluate(prepaymentFormFenceJS).Do(ctx)
		if err != nil {
			return err
		}
		if exception != nil {
			return prepaymentFail("PREPAYMENT_FENCE_UNAVAILABLE")
		}
	}
	return nil
}

const prepaymentFrameContextReadyJS = `Boolean(typeof document !== 'undefined' && location.href !== 'about:blank' && (document.readyState === 'interactive' || document.readyState === 'complete'))`

func waitPrepaymentFrameContext(protocol context.Context) error {
	ctx, cancel := context.WithTimeout(protocol, 5*time.Second)
	defer cancel()
	for {
		object, exception, err := cdpruntime.Evaluate(prepaymentFrameContextReadyJS).WithReturnByValue(true).Do(ctx)
		var ready bool
		if err == nil && exception == nil && object != nil && json.Unmarshal(object.Value, &ready) == nil && ready {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (driver *chromePrepaymentDriver) attachPrepaymentFieldContext(id target.ID, entry *fenceTarget) error {
	child, err := newPrepaymentTargetContext(driver.ctx, id)
	if err != nil {
		return err
	}
	// Keep this derived execution context alive after success. Cancelling it
	// invalidates the Target event loop/evidence, but does NOT cancel the
	// chromedp owner (whose cancellation would CloseTarget). Register the
	// invalidator before first Run so a destroyed target cannot leave a pending
	// command waiting on a response from a detached CDP session.
	execution, invalidate := context.WithCancel(child)
	driver.fence.mu.Lock()
	if driver.fence.targets[id] != entry {
		driver.fence.mu.Unlock()
		invalidate()
		return prepaymentFail("PREPAYMENT_FENCE_UNAVAILABLE")
	}
	entry.ctx, entry.invalidate = execution, invalidate
	driver.fence.mu.Unlock()
	timer := time.AfterFunc(5*time.Second, invalidate)
	// The manual connection is only a field executor. Remove its additional
	// automatic sessions, leaving the original protected protocol tree intact.
	err = chromedp.Run(execution, target.SetAutoAttach(false, false).WithFlatten(true))
	stopped := timer.Stop()
	if !stopped {
		return context.DeadlineExceeded
	}
	if err != nil {
		return err
	}
	driver.fence.mu.Lock()
	defer driver.fence.mu.Unlock()
	if driver.fence.targets[id] != entry {
		return prepaymentFail("PREPAYMENT_FENCE_UNAVAILABLE")
	}
	entry.manualReady = true
	entry.sessionID = chromedp.FromContext(child).Target.SessionID
	entry.ready = fenceTargetReady(entry)
	return nil
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

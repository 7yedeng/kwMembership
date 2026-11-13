package checkout

import (
	"context"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"

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

type fenceTarget struct {
	ctx          context.Context
	iframe       bool
	ready        bool
	sessionID    target.SessionID
	autoSessions map[target.SessionID]bool
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
}

func newPrepaymentFence() *prepaymentFence {
	return &prepaymentFence{signal: make(chan struct{}), targets: make(map[target.ID]*fenceTarget)}
}

func (fence *prepaymentFence) markBlocked() {
	fence.blocked.Store(true)
	fence.signalOnce.Do(func() { close(fence.signal) })
}
func (fence *prepaymentFence) markFailed() {
	fence.failed.Store(true)
	fence.signalOnce.Do(func() { close(fence.signal) })
}

func (driver *chromePrepaymentDriver) SubmissionState() submissionState {
	if driver.fence == nil {
		return submissionState{Failed: true}
	}
	fence := driver.fence
	return submissionState{Ready: fence.mainReady.Load() && fence.pending.Load() == 0 && !fence.failed.Load(), Blocked: fence.blocked.Load(), Failed: fence.failed.Load()}
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
			driver.fence.removeSession(value.SessionID)
		}
	})
}

func (fence *prepaymentFence) removeTarget(id target.ID) {
	fence.mu.Lock()
	delete(fence.targets, id)
	fence.mu.Unlock()
}

func (fence *prepaymentFence) removeSession(id target.SessionID) {
	if id == "" {
		return
	}
	fence.mu.Lock()
	defer fence.mu.Unlock()
	for key, entry := range fence.targets {
		if entry.sessionID == id || entry.autoSessions[id] {
			// Context cancellation would CloseTarget. Leave lifetime ownership
			// with the parent, but never reuse this detached renderer's facts.
			delete(fence.targets, key)
		}
	}
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
				driver.fence.markFailed()
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
					driver.fence.markFailed()
				}
			}()
		case *target.EventAttachedToTarget:
			if value.TargetInfo == nil {
				return
			}
			switch value.TargetInfo.Type {
			case "iframe", "worker", "service_worker", "shared_worker":
				driver.attachPrepaymentTarget(value.TargetInfo.TargetID, value.TargetInfo.Type, value.SessionID)
			default:
				// An unexpected related target cannot be called protected.
				driver.fence.markFailed()
				return
			}
		case *target.EventDetachedFromTarget:
			driver.fence.removeSession(value.SessionID)
		case *target.EventTargetDestroyed:
			driver.fence.removeTarget(value.TargetID)
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
		target.SetAutoAttach(true, true).WithFlatten(true),
		chromedp.Evaluate(prepaymentFormFenceJS, nil),
	)
}

func (driver *chromePrepaymentDriver) installWorkerFence(ctx context.Context) error {
	// chromedp recognizes WorkerGlobalScope and initializes only available
	// domains. Workers have no forms/Page domain: protect their requests only.
	// Unsupported Fetch fails closed without disabling or replacing the worker.
	return chromedp.Run(ctx,
		fetch.Enable().WithPatterns([]*fetch.RequestPattern{{URLPattern: "*", RequestStage: fetch.RequestStageRequest}}),
	)
}

func (driver *chromePrepaymentDriver) attachPrepaymentTarget(id target.ID, kind string, autoSession target.SessionID) {
	fence := driver.fence
	fence.mu.Lock()
	if existing, exists := fence.targets[id]; exists {
		existing.autoSessions[autoSession] = true
		fence.mu.Unlock()
		return
	}
	entry := &fenceTarget{iframe: kind == "iframe", autoSessions: map[target.SessionID]bool{autoSession: true}}
	fence.targets[id] = entry
	fence.pending.Add(1)
	fence.mu.Unlock()
	go func() {
		defer fence.pending.Add(-1)
		child, err := newPrepaymentTargetContext(driver.ctx, id)
		if err != nil {
			fence.markFailed()
			return
		}
		driver.listenFence(child)
		// Auto-attachment paused this renderer. Install its own interception and
		// document hooks before resuming it; no site-isolation flags are changed.
		var installErr error
		if entry.iframe {
			installErr = driver.installFence(child)
		} else {
			installErr = driver.installWorkerFence(child)
		}
		if installErr != nil {
			fence.mu.Lock()
			stillOwned := fence.targets[id] == entry
			fence.mu.Unlock()
			if stillOwned && driver.ctx.Err() == nil {
				fence.markFailed()
			}
			return
		}
		fence.mu.Lock()
		exists := fence.targets[id] == entry
		if exists {
			entry.ctx = child
			entry.ready = true
			entry.sessionID = chromedp.FromContext(child).Target.SessionID
		}
		fence.mu.Unlock()
		if !exists {
			return
		}
		if err := chromedp.Run(child, cdpruntime.RunIfWaitingForDebugger()); err != nil && driver.ctx.Err() == nil {
			fence.markFailed()
		}
	}()
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

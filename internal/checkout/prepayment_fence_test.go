package checkout

import (
	"context"
	"testing"
	"time"

	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"

	"kwmembership/internal/sessionrestore"
)

func TestPrepaymentKnownConfirmationRoutesAreBlockedWithoutReadingBody(t *testing.T) {
	for _, url := range []string{
		"https://api.stripe.com/v1/payment_intents/pi_synthetic/confirm",
		"https://api.stripe.com/v1/setup_intents/seti_synthetic/confirm?opaque=synthetic",
		"https://api.stripe.com/v1/payment_pages/cs_synthetic/confirm/",
		"https://chatgpt.com/checkout-api/confirm",
		"https://chatgpt.com/backend-api/payments/confirm",
		"http://127.0.0.1:1234/v1/payment_intents/pi_fixture/confirm",
	} {
		if !isKnownPaymentConfirmation(url) {
			t.Fatal("known confirmation escaped")
		}
	}
	for _, url := range []string{
		"https://chatgpt.com/backend-api/payments/checkout",
		"https://api.stripe.com/v1/payment_pages/cs_fixture/init",
		"https://api.stripe.com/v1/tax/calculations",
		"https://chatgpt.com/api/auth/session",
		"https://example.invalid/read?next=/payment_intents/pi_x/confirm",
	} {
		if isKnownPaymentConfirmation(url) {
			t.Fatal("read/quote request was classified as confirmation")
		}
	}
}

type prepaymentPausedNavigationFake struct{ *prepaymentFake }

func (fake *prepaymentPausedNavigationFake) OpenPlus(ctx context.Context, identity sessionrestore.Identity) error {
	if err := fake.prepaymentFake.OpenPlus(ctx, identity); err != nil {
		return err
	}
	fake.fenceState = submissionState{Failed: true}
	close(fake.fenceSignal)
	<-ctx.Done()
	return ctx.Err()
}

func TestPrepaymentFenceFailureInterruptsPausedNavigationBeforeGlobalTimeout(t *testing.T) {
	request, fake := prepaymentFixture(t)
	fake.fenceSignal = make(chan struct{})
	driver := &prepaymentPausedNavigationFake{prepaymentFake: fake}
	started := time.Now()
	report, err := runPrepayment(context.Background(), request, 5*time.Second, func(context.Context) (prepaymentDriver, error) {
		return driver, nil
	})
	if err == nil || report.Code != "PREPAYMENT_FENCE_UNAVAILABLE" || !report.BrowserClosed || report.ReadyForConfirmation {
		t.Fatalf("failed protection did not stop navigation: %v %+v", err, report)
	}
	if time.Since(started) > time.Second {
		t.Fatal("paused navigation waited for the global timeout")
	}
}

func TestPrepaymentChildContextInheritsExistingBrowserWithoutAllocationOptions(t *testing.T) {
	parent, cancel := chromedp.NewContext(context.Background())
	defer cancel()
	state := chromedp.FromContext(parent)
	// A deliberately unstarted structural fixture: creating a child context
	// must not run CDP, launch Chrome or apply Browser allocation options.
	state.Browser = &chromedp.Browser{}
	state.Target = &chromedp.Target{}
	child, err := newPrepaymentTargetContext(parent, "owned-iframe")
	if err != nil || child == nil {
		t.Fatalf("child context failed: %v", err)
	}
	childState := chromedp.FromContext(child)
	if childState.Browser != state.Browser || childState.Target != nil {
		t.Fatal("child replaced the owned browser or initialized a target")
	}
	if _, err := newPrepaymentTargetContext(context.Background(), "unowned"); err == nil {
		t.Fatal("unowned context accepted")
	}
}

func TestPrepaymentFenceDropsOnlyOwnedDetachedSessions(t *testing.T) {
	fence := newPrepaymentFence()
	owner, cancel := context.WithCancel(context.Background())
	defer cancel()
	frameEvidence, invalidateFrame := context.WithCancel(owner)
	workerEvidence, invalidateWorker := context.WithCancel(owner)
	fence.targets["iframe-one"] = &fenceTarget{ctx: frameEvidence, invalidate: invalidateFrame, ready: true, iframe: true, sessionID: "manual-one", autoSessions: map[target.SessionID]*fenceAutoAttachment{"auto-one": {}}}
	fence.targets["worker-two"] = &fenceTarget{ctx: workerEvidence, invalidate: invalidateWorker, ready: true, sessionID: "manual-two", autoSessions: map[target.SessionID]*fenceAutoAttachment{"auto-two": {}}}
	fence.removeSession("unrelated-session")
	if len(fence.targets) != 2 {
		t.Fatal("unrelated session changed owned targets")
	}
	fence.removeSession("auto-one")
	if len(fence.targets) != 1 || fence.targets["iframe-one"] != nil || owner.Err() != nil || frameEvidence.Err() == nil {
		t.Fatal("detached frame retained evidence or closed owner")
	}
	if !fence.removeSession("manual-two") {
		t.Fatal("lost protection session was not distinguished")
	}
	if len(fence.targets) != 0 || owner.Err() != nil || workerEvidence.Err() == nil {
		t.Fatal("detached worker retained evidence or closed owner")
	}
}

func TestPrepaymentFenceDestroyedTargetCannotSupplyFrameContext(t *testing.T) {
	fence := newPrepaymentFence()
	owner := context.Background()
	evidence, invalidate := context.WithCancel(owner)
	fence.targets["gone"] = &fenceTarget{ctx: evidence, invalidate: invalidate, ready: true, iframe: true}
	fence.targets["worker"] = &fenceTarget{ctx: owner, ready: true}
	fence.removeTarget("gone")
	if evidence.Err() == nil || owner.Err() != nil {
		t.Fatal("destroyed target facts remained live or owner was cancelled")
	}
	driver := &chromePrepaymentDriver{ctx: owner, fence: fence}
	if contexts := driver.frameContexts(); len(contexts) != 1 || contexts[0] != owner {
		t.Fatal("destroyed or worker target leaked into frame routing")
	}
}

func TestPrepaymentTargetCannotBeReadyBeforeEveryOriginalPauseIsResumed(t *testing.T) {
	entry := &fenceTarget{installed: true, autoSessions: map[target.SessionID]*fenceAutoAttachment{
		"flat-not-paused": {waiting: false},
		"original-one":    {waiting: true, resumed: true},
		"original-two":    {waiting: true},
	}}
	if fenceTargetReady(entry) {
		t.Fatal("a second paused session was skipped")
	}
	entry.autoSessions["original-two"].resumed = true
	if !fenceTargetReady(entry) {
		t.Fatal("completed original sessions remained unready")
	}
	entry.installed = false
	if fenceTargetReady(entry) {
		t.Fatal("resume alone replaced protection installation")
	}
}

func TestPrepaymentFenceReadinessIncludesEveryOwnedTarget(t *testing.T) {
	fence := newPrepaymentFence()
	fence.mainReady.Store(true)
	fence.targets["pending"] = &fenceTarget{installed: true}
	driver := &chromePrepaymentDriver{ctx: context.Background(), fence: fence}
	if driver.SubmissionState().Ready {
		t.Fatal("zero pending counter concealed unready original session")
	}
	fence.targets["pending"].ready = true
	if !driver.SubmissionState().Ready {
		t.Fatal("all confirmed protections did not become ready")
	}
}

func TestPrepaymentSubmissionDuringHoldRevokesReadyAndCloses(t *testing.T) {
	request, fake := prepaymentFixture(t)
	request.HoldOpen = 5 * time.Second
	fake.fenceSignal = make(chan struct{})
	request.OnReady = func(report PrepaymentReport) {
		if !report.SubmissionFenceVerified || report.SubmissionBlocked {
			t.Fatal("ready lacked fence evidence")
		}
		fake.fenceState.Blocked = true
		close(fake.fenceSignal)
	}
	started := time.Now()
	report, err := runPrepaymentFake(request, fake)
	if err == nil || report.Code != "PREPAYMENT_SUBMISSION_BLOCKED" || report.ReadyForConfirmation || !report.SubmissionBlocked || !report.BrowserClosed {
		t.Fatalf("bad blocked state: %v %+v", err, report)
	}
	if time.Since(started) > time.Second {
		t.Fatal("hold did not stop promptly")
	}
}

func TestPrepaymentUnknownFenceCoverageNeverClaimsReady(t *testing.T) {
	request, fake := prepaymentFixture(t)
	fake.fenceState = submissionState{Failed: true}
	report, err := runPrepaymentFake(request, fake)
	if err == nil || report.Code != "PREPAYMENT_FENCE_UNAVAILABLE" || report.SubmissionFenceVerified || report.ReadyForConfirmation {
		t.Fatalf("unconfirmed fence passed: %v %+v", err, report)
	}
}

func TestPrepaymentHoldEndsWithFreshQuoteAndFieldReadback(t *testing.T) {
	request, fake := prepaymentFixture(t)
	request.HoldOpen = time.Millisecond
	report, err := runPrepayment(context.Background(), request, time.Second, func(context.Context) (prepaymentDriver, error) { return fake, nil })
	if err != nil || !report.ReadyForConfirmation || fake.verified != 3 {
		t.Fatalf("hold did not recheck: %v %+v", err, report)
	}
}

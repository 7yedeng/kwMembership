package checkout

import (
	"context"
	"testing"
	"time"

	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
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
	fence.targets["iframe-one"] = &fenceTarget{ctx: owner, ready: true, iframe: true, sessionID: "manual-one", autoSessions: map[target.SessionID]bool{"auto-one": true}}
	fence.targets["worker-two"] = &fenceTarget{ctx: owner, ready: true, sessionID: "manual-two", autoSessions: map[target.SessionID]bool{"auto-two": true}}
	fence.removeSession("unrelated-session")
	if len(fence.targets) != 2 {
		t.Fatal("unrelated session changed owned targets")
	}
	fence.removeSession("auto-one")
	if len(fence.targets) != 1 || fence.targets["iframe-one"] != nil || owner.Err() != nil {
		t.Fatal("detached frame retained evidence or closed owner")
	}
	fence.removeSession("manual-two")
	if len(fence.targets) != 0 || owner.Err() != nil {
		t.Fatal("detached worker retained evidence or closed owner")
	}
}

func TestPrepaymentFenceDestroyedTargetCannotSupplyFrameContext(t *testing.T) {
	fence := newPrepaymentFence()
	owner := context.Background()
	fence.targets["gone"] = &fenceTarget{ctx: owner, ready: true, iframe: true}
	fence.targets["worker"] = &fenceTarget{ctx: owner, ready: true}
	fence.removeTarget("gone")
	driver := &chromePrepaymentDriver{ctx: owner, fence: fence}
	if contexts := driver.frameContexts(); len(contexts) != 1 || contexts[0] != owner {
		t.Fatal("destroyed or worker target leaked into frame routing")
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

package checkout

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/cdp"
)

type frameOwnerKey struct{}

type frameReaderFixture struct {
	trees   map[string][]cdp.FrameID
	facts   map[string]map[cdp.FrameID]frameFact
	calls   []string
	scripts []string
	readErr error
}

func (fixture *frameReaderFixture) frameIDs(ctx context.Context) ([]cdp.FrameID, error) {
	owner, _ := ctx.Value(frameOwnerKey{}).(string)
	return fixture.trees[owner], nil
}

func (fixture *frameReaderFixture) inspect(ctx context.Context, id cdp.FrameID, script string) (frameFact, error) {
	owner, _ := ctx.Value(frameOwnerKey{}).(string)
	fixture.calls = append(fixture.calls, owner+":"+string(id))
	fixture.scripts = append(fixture.scripts, script)
	if fact, found := fixture.facts[owner][id]; found {
		return fact, nil
	}
	if fixture.readErr != nil {
		return frameFact{}, fixture.readErr
	}
	return frameFact{}, errors.New("frame belongs to a different renderer")
}

func frameOwner(name string) context.Context {
	return context.WithValue(context.Background(), frameOwnerKey{}, name)
}

func TestPrepaymentFramesRecoverRemoteFrameInItsOwningTarget(t *testing.T) {
	main, child := frameOwner("main"), frameOwner("child")
	fixture := &frameReaderFixture{
		trees: map[string][]cdp.FrameID{"main": {"main-frame", "remote-frame", "nested-frame"}, "child": {"remote-frame", "nested-frame"}},
		facts: map[string]map[cdp.FrameID]frameFact{
			"main": {"main-frame": {Origin: "https://pay.openai.com", Fields: map[string]bool{"billingName": true}}},
			"child": {
				"remote-frame": {Origin: "https://js.stripe.com", Fields: map[string]bool{"cardNumber": true}},
				"nested-frame": {Origin: "https://js.stripe.com", Fields: map[string]bool{"cvc": true}},
			},
		},
	}
	facts, err := inspectOwnedPrepaymentFrames(context.Background(), []context.Context{main, child, child}, "fixture-safe-script", fixture)
	if err != nil || len(facts) != 3 {
		t.Fatalf("remote frame resolution failed: facts=%d err=%v", len(facts), err)
	}
	wantCalls := "main:main-frame,main:remote-frame,main:nested-frame,child:remote-frame,child:nested-frame"
	if strings.Join(fixture.calls, ",") != wantCalls {
		t.Fatalf("wrong renderer or deduplication order: %v", fixture.calls)
	}
	for _, script := range fixture.scripts {
		if script != "fixture-safe-script" {
			t.Fatal("inspection did not preserve the caller's script")
		}
	}
	if facts[0].TargetContext != main || facts[1].TargetContext != child || facts[2].TargetContext != child {
		t.Fatal("frame facts were not bound to their persistent owning targets")
	}
	for _, fact := range facts {
		if fact.TargetContext.Err() != nil {
			t.Fatal("inspection cancelled a persistent target context")
		}
	}
}

func TestPrepaymentFramesDoNotReturnPartialEvidenceForUnresolvedRemoteFrame(t *testing.T) {
	main := frameOwner("main")
	fixture := &frameReaderFixture{
		trees:   map[string][]cdp.FrameID{"main": {"main-frame", "remote-frame"}},
		facts:   map[string]map[cdp.FrameID]frameFact{"main": {"main-frame": {Origin: "https://pay.openai.com"}}},
		readErr: errors.New("private-CDP-error-with-sensitive-material"),
	}
	facts, err := inspectOwnedPrepaymentFrames(context.Background(), []context.Context{main}, "fixture-safe-script", fixture)
	if facts != nil || SafePrepaymentCode(err) != "PREPAYMENT_PAGE_UNSUPPORTED" || strings.Contains(err.Error(), "sensitive") {
		t.Fatal("unresolved remote frame returned partial or unsafe evidence")
	}
}

func TestPrepaymentFramesRejectMissingOwnersAndEmptyTrees(t *testing.T) {
	for _, name := range []string{"no targets", "nil owner", "empty tree", "empty frame ID", "no readable frames"} {
		t.Run(name, func(t *testing.T) {
			targets := []context.Context{frameOwner("main")}
			fixture := &frameReaderFixture{trees: map[string][]cdp.FrameID{}}
			switch name {
			case "no targets":
				targets = nil
			case "nil owner":
				targets[0] = nil
			case "empty frame ID":
				fixture.trees["main"] = []cdp.FrameID{""}
			case "no readable frames":
				fixture.trees["main"] = []cdp.FrameID{"unreadable"}
			}
			facts, err := inspectOwnedPrepaymentFrames(context.Background(), targets, "fixture-safe-script", fixture)
			if facts != nil || SafePrepaymentCode(err) != "PREPAYMENT_PAGE_UNSUPPORTED" {
				t.Fatal("invalid frame ownership was accepted")
			}
		})
	}
}

func TestPrepaymentFrameOperationCancellationDoesNotCloseOwnedTarget(t *testing.T) {
	caller, cancelCaller := context.WithCancel(context.Background())
	defer cancelCaller()
	owned, cancelOwner := context.WithCancel(frameOwner("persistent-child"))
	defer cancelOwner()
	started := make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		finished <- withPrepaymentFrameContext(caller, owned, func(ctx context.Context) error {
			if ctx.Value(frameOwnerKey{}) != "persistent-child" {
				return errors.New("wrong owner")
			}
			close(started)
			<-ctx.Done()
			return ctx.Err()
		})
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("operation did not start")
	}
	cancelCaller()
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("caller cancellation lost: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("caller cancellation did not reach frame operation")
	}
	if owned.Err() != nil {
		t.Fatal("caller cancellation closed the persistent target")
	}
}

func TestPrepaymentFrameOperationUsesCallerForLegacyFrameWithoutTarget(t *testing.T) {
	caller := frameOwner("main")
	var derived context.Context
	if err := withPrepaymentFrameContext(caller, nil, func(ctx context.Context) error {
		derived = ctx
		if ctx.Value(frameOwnerKey{}) != "main" {
			return errors.New("main caller was not inherited")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if derived == nil || derived.Err() == nil || caller.Err() != nil {
		t.Fatal("temporary operation or persistent caller lifetime is incorrect")
	}
}

func TestPrepaymentFrameOperationRejectsDetachedTargetBeforeExecution(t *testing.T) {
	owned, cancel := context.WithCancel(frameOwner("detached"))
	cancel()
	called := false
	err := withPrepaymentFrameContext(context.Background(), owned, func(context.Context) error { called = true; return nil })
	if !errors.Is(err, context.Canceled) || called {
		t.Fatal("detached target was executed or silently replaced by the main tab")
	}
}

func TestPrepaymentFrameEvaluationNeverInitializesAnUnknownBrowserContext(t *testing.T) {
	var output bool
	err := evaluatePrepaymentFrame(context.Background(), frameFields{FrameID: "unbound"}, "true", &output, false)
	if SafePrepaymentCode(err) != "PREPAYMENT_FIELDS_NOT_VERIFIED" || output {
		t.Fatal("uninitialized target was accepted")
	}
}

type unmarshalableFrameContext struct{ context.Context }

func (unmarshalableFrameContext) MarshalJSON() ([]byte, error) {
	return nil, errors.New("target contexts must not enter JSON")
}

func TestPrepaymentFrameContextIsExcludedFromFactsAndFieldJSON(t *testing.T) {
	ctx := unmarshalableFrameContext{context.Background()}
	for _, value := range []any{
		frameFact{FrameID: "frame", TargetContext: ctx, Fields: map[string]bool{"cvc": true}},
		frameFields{FrameID: "frame", TargetContext: ctx, Fields: map[string]bool{"cvc": true}},
	} {
		encoded, err := json.Marshal(value)
		if err != nil || strings.Contains(string(encoded), "TargetContext") {
			t.Fatal("target context escaped into evidence JSON")
		}
	}
}

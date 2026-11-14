package checkout

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	cdpruntime "github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
	jsonv2 "github.com/go-json-experiment/json"
)

type autoProtocolTestRootKey struct{}
type autoProtocolTestOwnerKey struct{}
type autoProtocolTestExecutor func(context.Context, string, any, any) error

func (execute autoProtocolTestExecutor) Execute(ctx context.Context, method string, params, result any) error {
	return execute(ctx, method, params, result)
}

type autoProtocolTestWire struct {
	ID     int64           `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

func autoProtocolTestForward(params any) (target.SessionID, autoProtocolTestWire, error) {
	encoded, err := jsonv2.Marshal(params, chromedp.DefaultMarshalOptions)
	if err != nil {
		return "", autoProtocolTestWire{}, err
	}
	var wrapper struct {
		SessionID target.SessionID `json:"sessionId"`
		Message   string           `json:"message"`
	}
	var command autoProtocolTestWire
	if json.Unmarshal(encoded, &wrapper) != nil || wrapper.SessionID == "" || json.Unmarshal([]byte(wrapper.Message), &command) != nil || command.ID <= 0 {
		return "", command, errors.New("invalid test forwarding envelope")
	}
	return wrapper.SessionID, command, nil
}

func autoProtocolTestSetup(onEvent func(context.Context, any), execute autoProtocolTestExecutor) (*prepaymentAutoProtocol, context.Context) {
	protocol := newPrepaymentAutoProtocol(onEvent)
	identity := &chromedp.Target{}
	root := context.WithValue(context.Background(), autoProtocolTestRootKey{}, identity)
	protocol.root = func(ctx context.Context) (*chromedp.Target, cdp.Executor, error) {
		identity, _ := ctx.Value(autoProtocolTestRootKey{}).(*chromedp.Target)
		if identity == nil {
			return nil, nil, errors.New("missing test parent")
		}
		return identity, execute, nil
	}
	return protocol, root
}

func autoProtocolTestPending(protocol *prepaymentAutoProtocol) int {
	protocol.mu.Lock()
	defer protocol.mu.Unlock()
	return len(protocol.pending)
}

func autoProtocolTestNestedEvent(session target.SessionID, message string) string {
	encoded, _ := json.Marshal(map[string]any{"method": "Target.receivedMessageFromTarget", "params": map[string]any{"sessionId": session, "message": message}})
	return string(encoded)
}

func TestPrepaymentAutoProtocolSessionOwnershipUsesTargetAndRouteIdentity(t *testing.T) {
	sends := 0
	protocol, root := autoProtocolTestSetup(nil, func(context.Context, string, any, any) error { sends++; return nil })
	carrier := context.WithValue(root, autoProtocolTestOwnerKey{}, "frame-one")
	first, err := protocol.NewSession(carrier, "same-opaque-id")
	if err != nil || first.Value(autoProtocolTestOwnerKey{}) != "frame-one" {
		t.Fatal("session did not inherit actual target ownership")
	}
	alias, err := protocol.NewSession(context.WithValue(root, autoProtocolTestOwnerKey{}, "another-carrier"), "same-opaque-id")
	if err != nil || alias != first {
		t.Fatal("context carrier changed route identity")
	}
	otherRoot := context.WithValue(root, autoProtocolTestRootKey{}, &chromedp.Target{})
	second, err := protocol.NewSession(otherRoot, "same-opaque-id")
	if err != nil || second == first {
		t.Fatal("different root target collided with original session")
	}
	child, err := protocol.NewSession(context.WithValue(first, autoProtocolTestOwnerKey{}, "child"), "same-opaque-id")
	if err != nil || child == first || child.Value(autoProtocolTestOwnerKey{}) != "child" {
		t.Fatal("nested session did not preserve its own owner and parent route")
	}
	foreign := newPrepaymentAutoProtocol(nil)
	if _, err := foreign.NewSession(first, "foreign-child"); err == nil {
		t.Fatal("accepted another protocol's logical session")
	}
	if _, err := newPrepaymentAutoProtocol(nil).NewSession(context.Background(), "uninitialized"); err == nil || sends != 0 {
		t.Fatal("session construction sent commands or allocated an implicit browser")
	}
}

func TestPrepaymentAutoProtocolActualTypedBootstrapAndFetchCommands(t *testing.T) {
	var protocol *prepaymentAutoProtocol
	var root context.Context
	var previous int64
	execute := autoProtocolTestExecutor(func(ctx context.Context, method string, params, _ any) error {
		sid, command, err := autoProtocolTestForward(params)
		if err != nil || method != "Target.sendMessageToTarget" || sid != "original" || command.ID <= previous || autoProtocolTestPending(protocol) != 1 {
			return errors.New("wrong command route or shared request sequence")
		}
		previous = command.ID
		var parameters map[string]json.RawMessage
		deadline, hasDeadline := ctx.Deadline()
		if !hasDeadline || time.Until(deadline) > 5*time.Second || json.Unmarshal(command.Params, &parameters) != nil || parameters == nil {
			return errors.New("commands need a bounded deadline and parameter object")
		}
		result := `{}`
		switch command.Method {
		case "Page.addScriptToEvaluateOnNewDocument":
			result = `{"identifier":"installed-fence"}`
		case "Runtime.evaluate":
			result = `{"result":{"type":"boolean","value":true}}`
		}
		protocol.Handle(root, sid, fmt.Sprintf(`{"id":%d,"result":%s}`, command.ID, result))
		return nil
	})
	protocol, root = autoProtocolTestSetup(nil, execute)
	session, err := protocol.NewSession(root, "original")
	if err != nil {
		t.Fatal(err)
	}
	commands := []struct {
		name string
		run  func(context.Context) error
	}{
		{"runtime enable", cdpruntime.Enable().Do},
		{"page enable", page.Enable().Do},
		{"binding", cdpruntime.AddBinding(prepaymentBinding).Do},
		{"new document fence", func(ctx context.Context) error {
			id, err := page.AddScriptToEvaluateOnNewDocument(prepaymentFormFenceJS).Do(ctx)
			if err == nil && id != "installed-fence" {
				return errors.New("script acknowledgement was not decoded")
			}
			return err
		}},
		{"fetch enable", fetch.Enable().WithPatterns([]*fetch.RequestPattern{{URLPattern: "*", RequestStage: fetch.RequestStageRequest}}).Do},
		{"nested attach", target.SetAutoAttach(true, true).WithFlatten(false).Do},
		{"original resume", cdpruntime.RunIfWaitingForDebugger().Do},
		{"context ready", func(ctx context.Context) error {
			value, exception, err := cdpruntime.Evaluate(prepaymentFrameContextReadyJS).WithReturnByValue(true).Do(ctx)
			if err == nil && (exception != nil || value == nil || value.Type != cdpruntime.TypeBoolean || string(value.Value) != "true") {
				return errors.New("fixed readiness probe was not decoded")
			}
			return err
		}},
		{"existing document fence", func(ctx context.Context) error {
			_, _, err := cdpruntime.Evaluate(prepaymentFormFenceJS).Do(ctx)
			return err
		}},
		{"continue", fetch.ContinueRequest("request-synthetic").Do},
		{"block", fetch.FailRequest("request-synthetic", network.ErrorReasonBlockedByClient).Do},
	}
	for _, command := range commands {
		t.Run(command.name, func(t *testing.T) {
			if err := command.run(session); err != nil {
				t.Fatal(err)
			}
		})
	}
	if previous != int64(len(commands)) || autoProtocolTestPending(protocol) != 0 || root.Err() != nil {
		t.Fatal("bootstrap, resume and Fetch did not share one clean command sequence")
	}
}

func TestPrepaymentAutoProtocolEventsStayLiveWithoutPendingCommandsAndFollowNestedOwnership(t *testing.T) {
	var protocol *prepaymentAutoProtocol
	var child context.Context
	var seen []context.Context
	callback := func(ctx context.Context, event any) {
		switch value := event.(type) {
		case *target.EventAttachedToTarget:
			var err error
			child, err = protocol.NewSession(context.WithValue(ctx, autoProtocolTestOwnerKey{}, "nested-frame"), value.SessionID)
			if err != nil {
				t.Error("listener could not synchronously register nested session")
			}
		case *fetch.EventRequestPaused:
			if value.RequestID != "synthetic-request" || value.Request == nil {
				t.Error("critical event lost fields")
			}
			seen = append(seen, ctx)
		}
	}
	protocol, root := autoProtocolTestSetup(callback, func(context.Context, string, any, any) error { return errors.New("unexpected send") })
	session, err := protocol.NewSession(context.WithValue(root, autoProtocolTestOwnerKey{}, "outer-frame"), "outer")
	if err != nil {
		t.Fatal(err)
	}
	paused := `{"method":"Fetch.requestPaused","params":{"requestId":"synthetic-request","request":{"url":"http://127.0.0.1/fixture","method":"GET","headers":{}},"frameId":"frame","resourceType":"Document"}}`
	protocol.Handle(root, "outer", paused)
	protocol.Handle(root, "unknown", paused)
	protocol.Handle(context.WithValue(root, autoProtocolTestRootKey{}, &chromedp.Target{}), "outer", paused)
	protocol.Handle(root, "outer", `{"method":"Target.attachedToTarget","params":{"sessionId":"inner","targetInfo":{"targetId":"child-frame","type":"iframe","title":"","url":"about:blank","attached":true,"canAccessOpener":false},"waitingForDebugger":true}}`)
	protocol.Handle(root, "outer", autoProtocolTestNestedEvent("inner", paused))
	if child == nil || len(seen) != 2 || seen[0] != session || seen[1] != child || seen[1].Value(autoProtocolTestOwnerKey{}) != "nested-frame" || autoProtocolTestPending(protocol) != 0 {
		t.Fatal("no-pending events were lost or routed to the wrong original session")
	}
}

func TestPrepaymentAutoProtocolNestedCommandWaitsForBothExactAcknowledgments(t *testing.T) {
	for _, childFirst := range []bool{true, false} {
		t.Run(fmt.Sprintf("child-first-%t", childFirst), func(t *testing.T) {
			var protocol *prepaymentAutoProtocol
			var root context.Context
			protocol, root = autoProtocolTestSetup(nil, func(_ context.Context, method string, params, _ any) error {
				outerSID, outer, err := autoProtocolTestForward(params)
				if err != nil || method != "Target.sendMessageToTarget" || outerSID != "outer" || outer.Method != "Target.sendMessageToTarget" {
					return errors.New("nested command did not travel through its actual parent session")
				}
				innerSID, inner, err := autoProtocolTestForward(outer.Params)
				if err != nil || innerSID != "inner" || inner.Method != "Runtime.runIfWaitingForDebugger" || inner.ID == outer.ID || autoProtocolTestPending(protocol) != 2 {
					return errors.New("inner command did not have its own acknowledgement")
				}
				protocol.Handle(root, "outer", fmt.Sprintf(`{"id":%d,"result":{}}`, inner.ID))
				protocol.Handle(root, "inner", fmt.Sprintf(`{"id":%d,"result":{}}`, inner.ID))
				if autoProtocolTestPending(protocol) != 2 {
					return errors.New("wrong parent or session consumed inner acknowledgement")
				}
				outerAck := fmt.Sprintf(`{"id":%d,"result":{}}`, outer.ID)
				innerAck := autoProtocolTestNestedEvent("inner", fmt.Sprintf(`{"id":%d,"result":{}}`, inner.ID))
				if childFirst {
					protocol.Handle(root, "outer", innerAck)
					protocol.Handle(root, "outer", outerAck)
				} else {
					protocol.Handle(root, "outer", outerAck)
					protocol.Handle(root, "outer", innerAck)
				}
				return nil
			})
			outer, _ := protocol.NewSession(root, "outer")
			inner, _ := protocol.NewSession(outer, "inner")
			if err := cdpruntime.RunIfWaitingForDebugger().Do(inner); err != nil || autoProtocolTestPending(protocol) != 0 {
				t.Fatal("nested command failed or retained a pending response")
			}
		})
	}
}

func TestPrepaymentAutoProtocolErrorsPreserveCodeWithoutPrintablePayload(t *testing.T) {
	var protocol *prepaymentAutoProtocol
	var root context.Context
	protocol, root = autoProtocolTestSetup(nil, func(_ context.Context, _ string, params, _ any) error {
		sid, command, err := autoProtocolTestForward(params)
		if err == nil {
			protocol.Handle(root, sid, fmt.Sprintf(`{"id":%d,"error":{"code":-32000,"message":"sensitive-synthetic-material"}}`, command.ID))
		}
		return err
	})
	session, _ := protocol.NewSession(root, "original")
	err := cdpruntime.Enable().Do(session)
	var inner *cdproto.Error
	if err == nil || SafePrepaymentCode(err) != "PREPAYMENT_FENCE_UNAVAILABLE" || !errors.As(err, &inner) || inner.Code != -32000 || inner.Message != "sensitive-synthetic-material" {
		t.Fatal("fixed error lost protocol classification evidence")
	}
	if strings.Contains(fmt.Sprintf("%v %+v %#v", err, err, err), "sensitive") || autoProtocolTestPending(protocol) != 0 {
		t.Fatal("error leaked raw material or a pending command")
	}
}

func TestPrepaymentAutoProtocolDropAndCancellationReleasePendingWithoutClosingRoot(t *testing.T) {
	for _, mode := range []string{"caller", "drop", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			started := make(chan autoProtocolTestWire, 1)
			protocol, root := autoProtocolTestSetup(nil, func(_ context.Context, _ string, params, _ any) error {
				_, command, err := autoProtocolTestForward(params)
				if err == nil {
					started <- command
				}
				return err
			})
			outer, _ := protocol.NewSession(root, "outer")
			inner, _ := protocol.NewSession(outer, "inner")
			caller, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			finished := make(chan error, 1)
			go func() { finished <- cdp.ExecutorFromContext(inner).Execute(caller, "Runtime.enable", nil, nil) }()
			var sent autoProtocolTestWire
			select {
			case sent = <-started:
			case <-time.After(time.Second):
				t.Fatal("command did not reach original parent")
			}
			switch mode {
			case "caller":
				cancel()
			case "drop":
				protocol.Drop(root, "outer")
			}
			select {
			case err := <-finished:
				if err == nil || err.Error() != "PREPAYMENT_FENCE_UNAVAILABLE" {
					t.Fatal("canceled or unacknowledged command was accepted")
				}
			case <-time.After(time.Second):
				t.Fatal("cancellation left command blocked")
			}
			protocol.Handle(root, "outer", fmt.Sprintf(`{"id":%d,"result":{}}`, sent.ID))
			if root.Err() != nil || autoProtocolTestPending(protocol) != 0 || mode == "drop" && (outer.Err() == nil || inner.Err() == nil) || mode != "drop" && inner.Err() != nil {
				t.Fatal("logical cancellation leaked waiters, closed parent, or failed to cascade")
			}
			if mode == "drop" {
				if err := cdpruntime.Enable().Do(inner); err == nil {
					t.Fatal("detached logical session could send again")
				}
			}
		})
	}
}

func TestPrepaymentAutoProtocolCriticalMalformedEventsFailClosed(t *testing.T) {
	failures := 0
	protocol, root := autoProtocolTestSetup(func(_ context.Context, event any) {
		if _, ok := event.(prepaymentAutoProtocolFailure); ok {
			failures++
		}
	}, func(context.Context, string, any, any) error { return nil })
	_, _ = protocol.NewSession(root, "original")
	for _, raw := range []string{
		`{"method":"Fetch.requestPaused","params":{},"unexpected":true}`,
		`{"method":"Runtime.bindingCalled"}`,
		`{"method":"Target.receivedMessageFromTarget","params":{"sessionId":null,"message":"{}"}}`,
		`{"method":"Fetch.requestPaused","method":"Runtime.bindingCalled","params":{}}`,
		`{"method":"Target.attachedToTarget","params":[]}`,
	} {
		before := failures
		protocol.Handle(root, "original", raw)
		if failures != before+1 {
			t.Fatal("malformed critical event was silently ignored")
		}
	}
	protocol.Handle(root, "original", `{"method":"Runtime.executionContextCreated","params":{}}`)
	if failures != 5 {
		t.Fatal("ordinary unused event was treated as a protocol failure")
	}
}

func TestPrepaymentAutoProtocolRejectsCommandsOutsideClosedLifecycleBoundary(t *testing.T) {
	sends := 0
	protocol, root := autoProtocolTestSetup(nil, func(context.Context, string, any, any) error { sends++; return nil })
	session, _ := protocol.NewSession(root, "original")
	cases := []struct {
		method string
		params any
	}{
		{"Runtime.evaluate", cdpruntime.Evaluate("document.querySelector('button').click()")},
		{"Runtime.evaluate", cdpruntime.Evaluate(prepaymentFrameContextReadyJS).WithUserGesture(true)},
		{"Runtime.evaluate", cdpruntime.Evaluate(prepaymentFrameContextReadyJS).WithContextID(123)},
		{"Page.addScriptToEvaluateOnNewDocument", page.AddScriptToEvaluateOnNewDocument("arbitrary-script")},
		{"Page.addScriptToEvaluateOnNewDocument", page.AddScriptToEvaluateOnNewDocument(prepaymentFormFenceJS).WithRunImmediately(true)},
		{"Fetch.continueRequest", fetch.ContinueRequest("id").WithURL("https://unrelated.invalid/")},
		{"Fetch.enable", fetch.Enable().WithPatterns([]*fetch.RequestPattern{{URLPattern: "*", RequestStage: fetch.RequestStageResponse}})},
		{"Fetch.disable", nil},
		{"Target.setAutoAttach", target.SetAutoAttach(true, false).WithFlatten(false)},
		{"Target.sendMessageToTarget", map[string]any{"sessionId": "unowned", "message": "{}"}},
		{"Runtime.evaluate", map[string]any{"expression": prepaymentFrameContextReadyJS, "userGesture": "false"}},
	}
	for _, test := range cases {
		if err := cdp.Execute(session, test.method, test.params, nil); SafePrepaymentCode(err) != "PREPAYMENT_FENCE_UNAVAILABLE" {
			t.Fatal("out-of-bound lifecycle command was allowed")
		}
	}
	if sends != 0 || autoProtocolTestPending(protocol) != 0 {
		t.Fatal("disallowed command reached transport")
	}
}

func TestPrepaymentAutoProtocolConcurrentResumeAndFetchResponsesKeepUniqueIDs(t *testing.T) {
	const count = 12
	type sentCommand struct {
		session target.SessionID
		id      int64
	}
	sent := make(chan sentCommand, count)
	protocol, root := autoProtocolTestSetup(nil, func(_ context.Context, _ string, params, _ any) error {
		sid, command, err := autoProtocolTestForward(params)
		if err == nil {
			sent <- sentCommand{sid, command.ID}
		}
		return err
	})
	session, _ := protocol.NewSession(root, "original")
	results := make(chan error, count)
	var workers sync.WaitGroup
	for index := 0; index < count; index++ {
		workers.Add(1)
		go func(index int) {
			defer workers.Done()
			if index%2 == 0 {
				results <- cdpruntime.RunIfWaitingForDebugger().Do(session)
			} else {
				results <- fetch.ContinueRequest(fetch.RequestID(fmt.Sprintf("synthetic-%d", index))).Do(session)
			}
		}(index)
	}
	commands := make([]sentCommand, 0, count)
	ids := make(map[int64]bool)
	for len(commands) < count {
		select {
		case command := <-sent:
			if ids[command.id] {
				t.Fatal("concurrent lifecycle and Fetch command reused an ID")
			}
			ids[command.id] = true
			commands = append(commands, command)
		case <-time.After(time.Second):
			t.Fatal("concurrent commands did not reach transport")
		}
	}
	for index := len(commands) - 1; index >= 0; index-- {
		command := commands[index]
		protocol.Handle(root, command.session, fmt.Sprintf(`{"id":%d,"result":{}}`, command.id))
	}
	workers.Wait()
	for index := 0; index < count; index++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if autoProtocolTestPending(protocol) != 0 || session.Err() != nil {
		t.Fatal("concurrent acknowledgements leaked state or canceled the session")
	}
}

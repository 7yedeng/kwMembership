package checkout

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
	jsonv2 "github.com/go-json-experiment/json"
	"github.com/go-json-experiment/json/jsontext"
)

const maxPrepaymentAutoProtocolMessage = 1 << 20
const maxPrepaymentAutoProtocolDepth = 16

type prepaymentAutoProtocolFailure struct{}
type prepaymentAutoProtocolKey struct{}

type prepaymentAutoProtocolError struct{ cause error }

func (*prepaymentAutoProtocolError) Error() string { return "PREPAYMENT_FENCE_UNAVAILABLE" }
func (*prepaymentAutoProtocolError) Code() string  { return "PREPAYMENT_FENCE_UNAVAILABLE" }
func (*prepaymentAutoProtocolError) GoString() string {
	return "prepaymentAutoProtocolError{redacted}"
}
func (err *prepaymentAutoProtocolError) Unwrap() error { return err.cause }

func autoProtocolFailure(cause error) error { return &prepaymentAutoProtocolError{cause: cause} }

type prepaymentAutoRouteKey struct {
	root   *chromedp.Target
	parent *prepaymentAutoSession
	id     target.SessionID
}

type prepaymentAutoProtocolReply struct {
	value json.RawMessage
	err   error
}

type prepaymentAutoProtocolPending struct {
	owner *prepaymentAutoSession
	reply chan prepaymentAutoProtocolReply
}

// rootResolver exists only as an internal unit-test seam. Production always
// takes an already initialized parent Target; it never allocates a browser.
type prepaymentAutoRootResolver func(context.Context) (*chromedp.Target, cdp.Executor, error)

type prepaymentAutoProtocol struct {
	mu      sync.Mutex
	nextID  int64
	routes  map[prepaymentAutoRouteKey]*prepaymentAutoSession
	pending map[int64]prepaymentAutoProtocolPending
	onEvent func(context.Context, any)
	root    prepaymentAutoRootResolver
}

func (*prepaymentAutoProtocol) String() string   { return "prepaymentAutoProtocol{redacted}" }
func (*prepaymentAutoProtocol) GoString() string { return "prepaymentAutoProtocol{redacted}" }

type prepaymentAutoSession struct {
	protocol *prepaymentAutoProtocol
	key      prepaymentAutoRouteKey
	parent   *prepaymentAutoSession
	root     cdp.Executor
	ctx      context.Context
	cancel   context.CancelFunc
	depth    int
}

func (*prepaymentAutoSession) String() string   { return "prepaymentAutoSession{redacted}" }
func (*prepaymentAutoSession) GoString() string { return "prepaymentAutoSession{redacted}" }

func newPrepaymentAutoProtocol(onEvent func(context.Context, any)) *prepaymentAutoProtocol {
	return &prepaymentAutoProtocol{routes: make(map[prepaymentAutoRouteKey]*prepaymentAutoSession),
		pending: make(map[int64]prepaymentAutoProtocolPending), onEvent: onEvent,
		root: func(ctx context.Context) (*chromedp.Target, cdp.Executor, error) {
			state := chromedp.FromContext(ctx)
			if state == nil || state.Browser == nil || state.Target == nil {
				return nil, nil, autoProtocolFailure(nil)
			}
			return state.Target, state.Target, nil
		}}
}

func (protocol *prepaymentAutoProtocol) parentRoute(parent context.Context, id target.SessionID) (prepaymentAutoRouteKey, *prepaymentAutoSession, cdp.Executor, error) {
	if protocol == nil || parent == nil || id == "" {
		return prepaymentAutoRouteKey{}, nil, nil, autoProtocolFailure(nil)
	}
	if owner, ok := parent.Value(prepaymentAutoProtocolKey{}).(*prepaymentAutoSession); ok {
		if owner == nil || owner.protocol != protocol {
			return prepaymentAutoRouteKey{}, nil, nil, autoProtocolFailure(nil)
		}
		return prepaymentAutoRouteKey{parent: owner, id: id}, owner, nil, nil
	}
	if protocol.root == nil {
		return prepaymentAutoRouteKey{}, nil, nil, autoProtocolFailure(nil)
	}
	identity, executor, err := protocol.root(parent)
	if err != nil || identity == nil || executor == nil {
		return prepaymentAutoRouteKey{}, nil, nil, autoProtocolFailure(err)
	}
	return prepaymentAutoRouteKey{root: identity, id: id}, nil, executor, nil
}

// NewSession is synchronous bookkeeping only and is safe in a Target listener.
// The context inherits driver ownership values even when its parent is a carrier
// made with context.WithValue. Route identity does not depend on that pointer.
func (protocol *prepaymentAutoProtocol) NewSession(parent context.Context, id target.SessionID) (context.Context, error) {
	key, owner, root, err := protocol.parentRoute(parent, id)
	if err != nil || parent.Err() != nil {
		return nil, autoProtocolFailure(err)
	}
	depth := 1
	if owner != nil {
		depth = owner.depth + 1
		if owner.ctx.Err() != nil {
			return nil, autoProtocolFailure(owner.ctx.Err())
		}
	}
	if depth > maxPrepaymentAutoProtocolDepth {
		return nil, autoProtocolFailure(nil)
	}
	protocol.mu.Lock()
	defer protocol.mu.Unlock()
	if existing := protocol.routes[key]; existing != nil {
		if existing.ctx.Err() != nil {
			return nil, autoProtocolFailure(existing.ctx.Err())
		}
		return existing.ctx, nil
	}
	base, cancel := context.WithCancel(parent)
	session := &prepaymentAutoSession{protocol: protocol, key: key, parent: owner, root: root, cancel: cancel, depth: depth}
	session.ctx = cdp.WithExecutor(context.WithValue(base, prepaymentAutoProtocolKey{}, session), session)
	protocol.routes[key] = session
	return session.ctx, nil
}

// Drop invalidates only logical session contexts. It neither detaches nor
// closes browser targets. Descendants and already returned evidence contexts
// become unusable immediately; their pending commands observe cancellation.
func (protocol *prepaymentAutoProtocol) Drop(parent context.Context, id target.SessionID) {
	key, _, _, err := protocol.parentRoute(parent, id)
	if err != nil {
		return
	}
	protocol.mu.Lock()
	if session := protocol.routes[key]; session != nil {
		session.cancel()
	}
	for candidate, session := range protocol.routes {
		if session.ctx.Err() != nil {
			delete(protocol.routes, candidate)
		}
	}
	protocol.mu.Unlock()
}

func (session *prepaymentAutoSession) Execute(caller context.Context, method string, params, result any) error {
	encoded, err := jsonv2.Marshal(params, chromedp.DefaultMarshalOptions)
	// Commands without parameters still use an object on the wire. An
	// explicit JSON null is not a valid CDP parameter dictionary.
	if string(encoded) == "null" {
		encoded = []byte(`{}`)
	}
	if err != nil || len(encoded) > maxPrepaymentAutoProtocolMessage || !allowedPrepaymentAutoCommand(method, encoded) {
		return autoProtocolFailure(nil)
	}
	return session.execute(caller, method, encoded, result)
}

func (session *prepaymentAutoSession) execute(caller context.Context, method string, params json.RawMessage, result any) error {
	if session == nil || caller == nil || session.ctx.Err() != nil || caller.Err() != nil {
		return autoProtocolFailure(nil)
	}
	deadline := time.Now().Add(prepaymentAutoResumeTimeout)
	if until, ok := caller.Deadline(); ok && until.Before(deadline) {
		deadline = until
	}
	ctx, cancel := context.WithDeadline(session.ctx, deadline)
	stop := context.AfterFunc(caller, cancel)
	defer func() { stop(); cancel() }()
	if ctx.Err() != nil || caller.Err() != nil {
		return autoProtocolFailure(ctx.Err())
	}
	protocol := session.protocol
	protocol.mu.Lock()
	if protocol.routes[session.key] != session || protocol.nextID >= 1<<31-1 {
		protocol.mu.Unlock()
		return autoProtocolFailure(nil)
	}
	protocol.nextID++
	id := protocol.nextID
	replies := make(chan prepaymentAutoProtocolReply, 1)
	protocol.pending[id] = prepaymentAutoProtocolPending{owner: session, reply: replies}
	protocol.mu.Unlock()
	defer func() { protocol.mu.Lock(); delete(protocol.pending, id); protocol.mu.Unlock() }()
	message, err := json.Marshal(struct {
		ID     int64           `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params,omitempty"`
	}{ID: id, Method: method, Params: params})
	if err != nil || len(message) > maxPrepaymentAutoProtocolMessage {
		return autoProtocolFailure(nil)
	}
	forward, err := json.Marshal(struct {
		SessionID target.SessionID `json:"sessionId"`
		Message   string           `json:"message"`
	}{session.key.id, string(message)})
	if err != nil {
		return autoProtocolFailure(nil)
	}
	if session.parent != nil {
		// This private forwarding path can only carry a command already
		// checked by the leaf's closed method/parameter allowlist.
		err = session.parent.execute(ctx, "Target.sendMessageToTarget", forward, nil)
	} else {
		// The generic executor re-marshals parameters. A bare []byte would
		// become a base64 JSON string instead of the CDP wrapper object.
		err = session.root.Execute(ctx, "Target.sendMessageToTarget", jsontext.Value(forward), nil)
	}
	if err != nil {
		return autoProtocolFailure(err)
	}
	select {
	case response := <-replies:
		if ctx.Err() != nil || caller.Err() != nil {
			return autoProtocolFailure(ctx.Err())
		}
		if response.err != nil {
			return response.err
		}
		if result != nil {
			if err := jsonv2.Unmarshal(response.value, result, chromedp.DefaultUnmarshalOptions); err != nil {
				return autoProtocolFailure(nil)
			}
		}
		return nil
	case <-ctx.Done():
		return autoProtocolFailure(ctx.Err())
	}
}

// Handle processes a message emitted by a known child of this exact parent.
// Events remain active when no command is pending, so Fetch never loses its
// listener after bootstrap. Driver callbacks must only register/queue work.
func (protocol *prepaymentAutoProtocol) Handle(parent context.Context, id target.SessionID, raw string) {
	key, _, _, err := protocol.parentRoute(parent, id)
	if err != nil {
		return
	}
	protocol.mu.Lock()
	session := protocol.routes[key]
	protocol.mu.Unlock()
	if session == nil || session.ctx.Err() != nil {
		return
	}
	fields, err := decodePrepaymentAutoObject(raw)
	if err != nil {
		protocol.emit(session, prepaymentAutoProtocolFailure{})
		return
	}
	if rawID, present := fields["id"]; present {
		var commandID int64
		if json.Unmarshal(rawID, &commandID) != nil || commandID <= 0 {
			return
		}
		protocol.mu.Lock()
		pending, exists := protocol.pending[commandID]
		if exists && pending.owner == session {
			delete(protocol.pending, commandID)
		} else {
			exists = false
		}
		protocol.mu.Unlock()
		if !exists {
			return
		}
		response := prepaymentAutoProtocolReply{value: fields["result"]}
		if rawError, failed := fields["error"]; failed {
			var protocolError cdproto.Error
			if json.Unmarshal(rawError, &protocolError) == nil && protocolError.Code != 0 {
				response.err = autoProtocolFailure(&protocolError)
			} else {
				response.err = autoProtocolFailure(nil)
			}
		} else {
			var object map[string]json.RawMessage
			if len(fields) != 2 || json.Unmarshal(response.value, &object) != nil || object == nil {
				response.err = autoProtocolFailure(nil)
			}
		}
		select {
		case pending.reply <- response:
		default:
		}
		return
	}
	var method string
	if json.Unmarshal(fields["method"], &method) != nil {
		protocol.emit(session, prepaymentAutoProtocolFailure{})
		return
	}
	switch method {
	case "Fetch.requestPaused", "Runtime.bindingCalled", "Target.attachedToTarget", "Target.detachedFromTarget", "Target.targetDestroyed":
		if len(fields) != 2 {
			protocol.emit(session, prepaymentAutoProtocolFailure{})
			return
		}
		event, err := cdproto.UnmarshalMessage(&cdproto.Message{Method: cdproto.MethodType(method), Params: jsontext.Value(fields["params"])}, chromedp.DefaultUnmarshalOptions)
		if err != nil {
			protocol.emit(session, prepaymentAutoProtocolFailure{})
			return
		}
		protocol.emit(session, event)
	case "Target.receivedMessageFromTarget":
		var event target.EventReceivedMessageFromTarget
		if len(fields) != 2 || json.Unmarshal(fields["params"], &event) != nil || event.SessionID == "" {
			protocol.emit(session, prepaymentAutoProtocolFailure{})
			return
		}
		protocol.Handle(session.ctx, event.SessionID, event.Message)
	}
}

func (protocol *prepaymentAutoProtocol) emit(session *prepaymentAutoSession, event any) {
	if protocol.onEvent != nil && session.ctx.Err() == nil {
		protocol.onEvent(session.ctx, event)
	}
}

func decodePrepaymentAutoObject(raw string) (map[string]json.RawMessage, error) {
	if len(raw) > maxPrepaymentAutoProtocolMessage {
		return nil, errors.New("protocol message too large")
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return nil, errors.New("invalid protocol message")
	}
	fields := make(map[string]json.RawMessage)
	for decoder.More() {
		token, err := decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok || len(fields) > 8 {
			return nil, errors.New("invalid protocol fields")
		}
		if _, exists := fields[key]; exists {
			return nil, errors.New("duplicate protocol field")
		}
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return nil, errors.New("invalid protocol field")
		}
		fields[key] = value
	}
	end, err := decoder.Token()
	var trailing any
	if err != nil || end != json.Delim('}') || decoder.Decode(&trailing) != io.EOF {
		return nil, errors.New("invalid protocol envelope")
	}
	return fields, nil
}

func allowedPrepaymentAutoCommand(method string, raw []byte) bool {
	var values map[string]json.RawMessage
	if string(raw) == "null" {
		values = map[string]json.RawMessage{}
	} else if json.Unmarshal(raw, &values) != nil || values == nil {
		return false
	}
	text := func(key string) string { var value string; _ = json.Unmarshal(values[key], &value); return value }
	flag := func(key string, want bool) bool {
		var value *bool
		return json.Unmarshal(values[key], &value) == nil && value != nil && *value == want
	}
	optionalFlag := func(key string, want bool) bool {
		_, present := values[key]
		return !present || flag(key, want)
	}
	keysOnly := func(allowed ...string) bool {
		for key := range values {
			known := false
			for _, candidate := range allowed {
				if key == candidate {
					known = true
					break
				}
			}
			if !known {
				return false
			}
		}
		return true
	}
	switch method {
	case "Runtime.enable", "Runtime.runIfWaitingForDebugger":
		return len(values) == 0
	case "Page.enable":
		return len(values) == 0 || len(values) == 1 && flag("enableFileChooserOpenedEvent", false)
	case "Runtime.addBinding":
		return len(values) == 1 && text("name") == prepaymentBinding
	case "Page.addScriptToEvaluateOnNewDocument":
		return keysOnly("source", "includeCommandLineAPI", "runImmediately") && text("source") == prepaymentFormFenceJS &&
			optionalFlag("includeCommandLineAPI", false) && optionalFlag("runImmediately", false)
	case "Runtime.evaluate":
		expression := text("expression")
		if expression != prepaymentFormFenceJS && expression != prepaymentFrameContextReadyJS {
			return false
		}
		for key, rawValue := range values {
			if key == "expression" {
				continue
			}
			var value *bool
			if json.Unmarshal(rawValue, &value) != nil || value == nil {
				return false
			}
			switch key {
			case "returnByValue", "allowUnsafeEvalBlockedByCSP":
			case "includeCommandLineAPI", "silent", "generatePreview", "userGesture", "awaitPromise", "throwOnSideEffect", "disableBreaks", "replMode":
				if *value {
					return false
				}
			default:
				return false
			}
		}
		return true
	case "Fetch.enable":
		var patterns []map[string]json.RawMessage
		if !keysOnly("patterns", "handleAuthRequests") || !optionalFlag("handleAuthRequests", false) ||
			json.Unmarshal(values["patterns"], &patterns) != nil || len(patterns) != 1 || len(patterns[0]) != 2 {
			return false
		}
		var pattern, stage string
		return json.Unmarshal(patterns[0]["urlPattern"], &pattern) == nil && pattern == "*" &&
			json.Unmarshal(patterns[0]["requestStage"], &stage) == nil && stage == "Request"
	case "Fetch.continueRequest":
		for key := range values {
			if key != "requestId" && key != "interceptResponse" {
				return false
			}
		}
		return text("requestId") != "" && optionalFlag("interceptResponse", false)
	case "Fetch.failRequest":
		return len(values) == 2 && text("requestId") != "" && text("errorReason") == "BlockedByClient"
	case "Target.setAutoAttach":
		return len(values) == 3 && flag("autoAttach", true) && flag("waitForDebuggerOnStart", true) && flag("flatten", false)
	default:
		return false
	}
}

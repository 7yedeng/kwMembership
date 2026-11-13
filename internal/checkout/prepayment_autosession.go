package checkout

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/target"
)

const prepaymentAutoResumeTimeout = 5 * time.Second
const maxPrepaymentAutoResumeAck = 16 << 10

type prepaymentAutoResumeSender func(context.Context, target.SessionID, string) error

// This router sends one fixed lifecycle command to a paused, non-flat auto
// session through its already initialized parent Target. It cannot navigate,
// fill fields, attach a target, submit a payment, or initialize a browser.
type prepaymentAutoResumeRouter struct {
	mu      sync.Mutex
	nextID  int64
	pending map[target.SessionID]map[int64]chan bool
	sender  prepaymentAutoResumeSender
}

func newPrepaymentAutoResumeRouter() *prepaymentAutoResumeRouter {
	return &prepaymentAutoResumeRouter{
		pending: make(map[target.SessionID]map[int64]chan bool),
		sender:  sendPrepaymentAutoResume,
	}
}

func sendPrepaymentAutoResume(ctx context.Context, sessionID target.SessionID, message string) error {
	executor, err := initializedPrepaymentFrameExecutor(ctx)
	if err != nil {
		return prepaymentFail("PREPAYMENT_FENCE_UNAVAILABLE")
	}
	// Chromium retains this method for non-flat sessions; the generated cdproto
	// package omits its deprecated constructor. Do not use it for flat sessions.
	return cdp.Execute(executor, "Target.sendMessageToTarget", struct {
		SessionID target.SessionID `json:"sessionId"`
		Message   string           `json:"message"`
	}{SessionID: sessionID, Message: message}, nil)
}

func (router *prepaymentAutoResumeRouter) Resume(caller context.Context, parent context.Context, sessionID target.SessionID) error {
	failure := func() error { return prepaymentFail("PREPAYMENT_FENCE_UNAVAILABLE") }
	if router == nil || caller == nil || parent == nil || sessionID == "" || router.sender == nil ||
		caller.Err() != nil || parent.Err() != nil {
		return failure()
	}
	deadline := time.Now().Add(prepaymentAutoResumeTimeout)
	if callerDeadline, ok := caller.Deadline(); ok && callerDeadline.Before(deadline) {
		deadline = callerDeadline
	}
	// Keep the parent's initialized Target values, with the shorter operation
	// deadline. Cancelling this derived context never closes the owned Target.
	ctx, cancel := context.WithDeadline(parent, deadline)
	stop := context.AfterFunc(caller, cancel)
	defer func() { stop(); cancel() }()
	if caller.Err() != nil || ctx.Err() != nil {
		return failure()
	}

	router.mu.Lock()
	// CDP request IDs are signed 32-bit integers. Exhaustion fails rather than
	// wrapping and accepting a late response for a previous command.
	if router.nextID >= 1<<31-1 || router.pending == nil {
		router.mu.Unlock()
		return failure()
	}
	router.nextID++
	id := router.nextID
	response := make(chan bool, 1)
	if router.pending[sessionID] == nil {
		router.pending[sessionID] = make(map[int64]chan bool)
	}
	router.pending[sessionID][id] = response
	router.mu.Unlock()
	defer router.remove(sessionID, id)

	message, err := json.Marshal(struct {
		ID     int64  `json:"id"`
		Method string `json:"method"`
	}{ID: id, Method: "Runtime.runIfWaitingForDebugger"})
	if err != nil || ctx.Err() != nil || caller.Err() != nil {
		return failure()
	}
	// Register before sending because the inner response can arrive before
	// the outer Target.sendMessageToTarget acknowledgment.
	if err := router.sender(ctx, sessionID, string(message)); err != nil {
		return failure()
	}
	select {
	case accepted := <-response:
		if !accepted || ctx.Err() != nil || caller.Err() != nil || parent.Err() != nil {
			return failure()
		}
		return nil
	case <-ctx.Done():
		return failure()
	}
}

func (router *prepaymentAutoResumeRouter) remove(sessionID target.SessionID, id int64) {
	router.mu.Lock()
	defer router.mu.Unlock()
	delete(router.pending[sessionID], id)
	if len(router.pending[sessionID]) == 0 {
		delete(router.pending, sessionID)
	}
}

// Handle is called synchronously by the parent's CDP event listener. No CDP
// command, blocking channel send, caller callback, or raw error logging occurs.
func (router *prepaymentAutoResumeRouter) Handle(sessionID target.SessionID, rawMessage string) {
	if router == nil || sessionID == "" {
		return
	}
	router.mu.Lock()
	known := len(router.pending[sessionID]) > 0
	router.mu.Unlock()
	if !known || len(rawMessage) > maxPrepaymentAutoResumeAck {
		return
	}
	id, accepted, valid := decodePrepaymentAutoResumeAck(rawMessage)
	if !valid {
		return
	}
	router.mu.Lock()
	response := router.pending[sessionID][id]
	if response != nil {
		delete(router.pending[sessionID], id)
		if len(router.pending[sessionID]) == 0 {
			delete(router.pending, sessionID)
		}
	}
	router.mu.Unlock()
	if response == nil {
		return
	}
	select {
	case response <- accepted:
	default:
	}
}

func decodePrepaymentAutoResumeAck(raw string) (int64, bool, bool) {
	decoder := json.NewDecoder(strings.NewReader(raw))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return 0, false, false
	}
	fields := make(map[string]json.RawMessage, 3)
	for decoder.More() {
		token, err := decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok || (key != "id" && key != "result" && key != "error") {
			return 0, false, false
		}
		if _, duplicate := fields[key]; duplicate {
			return 0, false, false
		}
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return 0, false, false
		}
		fields[key] = value
	}
	end, err := decoder.Token()
	var trailing any
	if err != nil || end != json.Delim('}') || decoder.Decode(&trailing) != io.EOF {
		return 0, false, false
	}
	var id int64
	if json.Unmarshal(fields["id"], &id) != nil || id <= 0 || id > 1<<31-1 {
		return 0, false, false
	}
	if _, errorPresent := fields["error"]; errorPresent {
		return id, false, true
	}
	var result map[string]json.RawMessage
	if json.Unmarshal(fields["result"], &result) != nil || result == nil || len(result) != 0 {
		return id, false, true
	}
	return id, true, true
}

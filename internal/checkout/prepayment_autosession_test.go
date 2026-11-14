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

	"github.com/chromedp/cdproto/target"
)

func autoResumeMessageID(message string) (int64, error) {
	var decoded struct {
		ID     int64  `json:"id"`
		Method string `json:"method"`
	}
	decoder := json.NewDecoder(strings.NewReader(message))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		return 0, err
	}
	if decoded.ID <= 0 || decoded.Method != "Runtime.runIfWaitingForDebugger" {
		return 0, errors.New("unexpected lifecycle command")
	}
	return decoded.ID, nil
}

func pendingAutoResumes(router *prepaymentAutoResumeRouter) int {
	router.mu.Lock()
	defer router.mu.Unlock()
	count := 0
	for _, requests := range router.pending {
		count += len(requests)
	}
	return count
}

func TestPrepaymentAutoResumeAcceptsOnlyItsOriginalSessionAndInnerAcknowledgment(t *testing.T) {
	router := newPrepaymentAutoResumeRouter()
	parent := context.WithValue(context.Background(), frameOwnerKey{}, "original-parent")
	started := time.Now()
	router.sender = func(ctx context.Context, session target.SessionID, message string) error {
		id, err := autoResumeMessageID(message)
		if err != nil {
			return err
		}
		deadline, ok := ctx.Deadline()
		if !ok || deadline.After(started.Add(5*time.Second+100*time.Millisecond)) || time.Until(deadline) > 5*time.Second ||
			ctx.Value(frameOwnerKey{}) != "original-parent" || session != "auto-original" || pendingAutoResumes(router) != 1 {
			return errors.New("wrong parent, timeout, registration, or original session")
		}
		// Neither a different session's response nor the wrong inner ID can
		// acknowledge the pending command, even if their payloads look valid.
		router.Handle("different-session", fmt.Sprintf(`{"id":%d,"result":{}}`, id))
		router.Handle(session, fmt.Sprintf(`{"id":%d,"result":{}}`, id+1))
		if pendingAutoResumes(router) != 1 {
			return errors.New("unrelated acknowledgment consumed this command")
		}
		// The actual inner response is allowed to arrive before the outer send
		// returns; this also verifies Handle does not block in the listener.
		router.Handle(session, fmt.Sprintf(`{"id":%d,"result":{}}`, id))
		router.Handle(session, fmt.Sprintf(`{"id":%d,"result":{}}`, id))
		return nil
	}
	if err := router.Resume(context.Background(), parent, "auto-original"); err != nil {
		t.Fatal(err)
	}
	if pendingAutoResumes(router) != 0 || parent.Err() != nil {
		t.Fatal("completed command leaked or cancelled its parent target")
	}
}

func TestPrepaymentAutoResumeInnerErrorsAndInvalidResultsFailWithoutRawOutput(t *testing.T) {
	responses := map[string]string{
		"protocol error":  `{"id":%d,"error":{"code":-32000,"message":"sensitive-session-URL-material"}}`,
		"error null":      `{"id":%d,"error":null,"result":{}}`,
		"no result":       `{"id":%d}`,
		"null result":     `{"id":%d,"result":null}`,
		"array result":    `{"id":%d,"result":[]}`,
		"nonempty result": `{"id":%d,"result":{"unexpected":"sensitive-session-URL-material"}}`,
	}
	for name, response := range responses {
		t.Run(name, func(t *testing.T) {
			router := newPrepaymentAutoResumeRouter()
			router.sender = func(_ context.Context, session target.SessionID, message string) error {
				id, err := autoResumeMessageID(message)
				if err != nil {
					return err
				}
				router.Handle(session, fmt.Sprintf(response, id))
				return nil
			}
			err := router.Resume(context.Background(), context.Background(), "auto-session")
			if SafePrepaymentCode(err) != "PREPAYMENT_FENCE_UNAVAILABLE" || strings.Contains(err.Error(), "sensitive") || pendingAutoResumes(router) != 0 {
				t.Fatal("invalid inner result was accepted, leaked, or retained")
			}
		})
	}
}

func TestPrepaymentAutoResumeOuterAcknowledgmentAloneCannotComplete(t *testing.T) {
	router := newPrepaymentAutoResumeRouter()
	caller, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	router.sender = func(context.Context, target.SessionID, string) error { return nil }
	if err := router.Resume(caller, context.Background(), "auto-session"); SafePrepaymentCode(err) != "PREPAYMENT_FENCE_UNAVAILABLE" || pendingAutoResumes(router) != 0 {
		t.Fatal("outer acknowledgment was treated as the original session's resume acknowledgment")
	}
}

func TestPrepaymentAutoResumeCallerOrParentCancellationReleasesWaiter(t *testing.T) {
	for _, cancelled := range []string{"caller", "parent"} {
		t.Run(cancelled, func(t *testing.T) {
			caller, cancelCaller := context.WithCancel(context.Background())
			parent, cancelParent := context.WithCancel(context.Background())
			defer cancelCaller()
			defer cancelParent()
			router := newPrepaymentAutoResumeRouter()
			started := make(chan int64, 1)
			router.sender = func(_ context.Context, _ target.SessionID, message string) error {
				id, err := autoResumeMessageID(message)
				if err != nil {
					return err
				}
				started <- id
				return nil
			}
			finished := make(chan error, 1)
			go func() { finished <- router.Resume(caller, parent, "auto-session") }()
			var id int64
			select {
			case id = <-started:
			case <-time.After(time.Second):
				t.Fatal("resume sender did not start")
			}
			if cancelled == "caller" {
				cancelCaller()
			} else {
				cancelParent()
			}
			select {
			case err := <-finished:
				if SafePrepaymentCode(err) != "PREPAYMENT_FENCE_UNAVAILABLE" {
					t.Fatal("cancelled resume did not return its fixed error")
				}
			case <-time.After(time.Second):
				t.Fatal("cancelled resume remained blocked")
			}
			router.Handle("auto-session", fmt.Sprintf(`{"id":%d,"result":{}}`, id))
			if pendingAutoResumes(router) != 0 || cancelled == "caller" && parent.Err() != nil {
				t.Fatal("late response revived a waiter or caller cancellation closed the parent")
			}
		})
	}
}

func TestPrepaymentAutoResumeDeadlineCoversSendAndSanitizesSendErrors(t *testing.T) {
	for _, block := range []bool{false, true} {
		router := newPrepaymentAutoResumeRouter()
		caller, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		router.sender = func(ctx context.Context, _ target.SessionID, _ string) error {
			if block {
				<-ctx.Done()
			}
			return errors.New("raw-session-URL-secret")
		}
		err := router.Resume(caller, context.Background(), "auto-session")
		cancel()
		if SafePrepaymentCode(err) != "PREPAYMENT_FENCE_UNAVAILABLE" || strings.Contains(err.Error(), "secret") || pendingAutoResumes(router) != 0 {
			t.Fatal("send failure was not bounded, sanitized, and removed")
		}
	}
}

func TestPrepaymentAutoResumeConcurrentCommandsDoNotCrossAcknowledge(t *testing.T) {
	const count = 12
	type command struct {
		session target.SessionID
		id      int64
	}
	router := newPrepaymentAutoResumeRouter()
	sent := make(chan command, count)
	router.sender = func(_ context.Context, session target.SessionID, message string) error {
		id, err := autoResumeMessageID(message)
		if err == nil {
			sent <- command{session, id}
		}
		return err
	}
	results := make(chan error, count)
	var workers sync.WaitGroup
	for index := 0; index < count; index++ {
		workers.Add(1)
		go func(index int) {
			defer workers.Done()
			results <- router.Resume(context.Background(), context.Background(), target.SessionID(fmt.Sprintf("auto-%d", index%3)))
		}(index)
	}
	commands := make([]command, 0, count)
	ids := make(map[int64]bool)
	for len(commands) < count {
		select {
		case command := <-sent:
			if ids[command.id] {
				t.Fatal("duplicate inner command ID")
			}
			ids[command.id] = true
			commands = append(commands, command)
		case <-time.After(time.Second):
			t.Fatal("concurrent resume did not send")
		}
	}
	for index := len(commands) - 1; index >= 0; index-- {
		command := commands[index]
		router.Handle(command.session, fmt.Sprintf(`{"id":%d,"result":{}}`, command.id))
	}
	workers.Wait()
	for index := 0; index < count; index++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if pendingAutoResumes(router) != 0 {
		t.Fatal("concurrent completion leaked waiters")
	}
}

func TestPrepaymentAutoResumeRejectsAmbiguousMessagesAndAcceptsLaterMatchingAck(t *testing.T) {
	router := newPrepaymentAutoResumeRouter()
	router.sender = func(_ context.Context, session target.SessionID, message string) error {
		id, err := autoResumeMessageID(message)
		if err != nil {
			return err
		}
		for _, raw := range []string{
			`null`, `[]`, `{`,
			fmt.Sprintf(`{"id":%d,"id":%d,"result":{}}`, id, id),
			fmt.Sprintf(`{"id":%d,"result":{},"method":"event"}`, id),
			fmt.Sprintf(`{"id":%d,"result":{}}{}`, id),
			fmt.Sprintf(`{"id":%d.0,"result":{}}`, id),
			strings.Repeat(" ", maxPrepaymentAutoResumeAck+1),
		} {
			router.Handle(session, raw)
			if pendingAutoResumes(router) != 1 {
				return errors.New("ambiguous message consumed a pending command")
			}
		}
		router.Handle(session, fmt.Sprintf(`{"id":%d,"result":{ }}`, id))
		return nil
	}
	if err := router.Resume(context.Background(), context.Background(), "auto-session"); err != nil {
		t.Fatal(err)
	}
}

func TestPrepaymentAutoResumeDefaultSenderDoesNotInitializeBrowser(t *testing.T) {
	router := newPrepaymentAutoResumeRouter()
	if err := router.Resume(context.Background(), context.Background(), "uninitialized-parent"); SafePrepaymentCode(err) != "PREPAYMENT_FENCE_UNAVAILABLE" || pendingAutoResumes(router) != 0 {
		t.Fatal("default sender accepted a parent without an initialized Target")
	}
}

func TestPrepaymentAutoResumeDoesNotWrapExhaustedIDsOrSendCancelledCommands(t *testing.T) {
	for _, name := range []string{"exhausted", "cancelled", "empty session"} {
		t.Run(name, func(t *testing.T) {
			router := newPrepaymentAutoResumeRouter()
			called := false
			router.sender = func(context.Context, target.SessionID, string) error { called = true; return nil }
			caller, cancel := context.WithCancel(context.Background())
			defer cancel()
			session := target.SessionID("auto-session")
			switch name {
			case "exhausted":
				router.nextID = 1<<31 - 1
			case "cancelled":
				cancel()
			case "empty session":
				session = ""
			}
			if err := router.Resume(caller, context.Background(), session); SafePrepaymentCode(err) != "PREPAYMENT_FENCE_UNAVAILABLE" || called || pendingAutoResumes(router) != 0 {
				t.Fatal("invalid lifecycle command was sent")
			}
		})
	}
}

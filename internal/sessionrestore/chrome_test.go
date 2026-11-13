package sessionrestore

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)

func TestUseChromeContextRequiresStartedCallerTarget(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	defer cancelParent()
	ctx, cleanup := chromedp.NewContext(parent)
	defer cleanup()
	for _, candidate := range []context.Context{nil, context.Background(), ctx} {
		facade, err := UseChromeContext(candidate)
		if SafeCode(err) != "SESSION_BROWSER_CONFIG_INVALID" || facade != nil {
			t.Fatal("an uninitialized context became a session browser", err)
		}
	}
	if ctx.Err() != nil || chromedp.FromContext(ctx).Browser != nil || chromedp.FromContext(ctx).Target != nil {
		t.Fatal("wrapping a context launched or cancelled a browser")
	}
	cancelParent()
	if _, err := UseChromeContext(ctx); SafeCode(err) != "SESSION_CANCELLED" {
		t.Fatal("closed owner was accepted", err)
	}
	if _, ownsLifecycle := any(&chromeSession{}).(interface{ Close() error }); ownsLifecycle {
		t.Fatal("borrowed facade exposes lifecycle ownership")
	}
}

func TestAllocatorKeepsOwnedContextAfterFirstAction(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var allocatedContext context.Context
	err := startPersistentBrowser(ctx, time.Second, cancel, func(received context.Context) error {
		allocatedContext = received
		return nil
	})
	if err != nil || allocatedContext != ctx || allocatedContext.Err() != nil {
		t.Fatal("allocator did not retain the owned browser lifetime", err)
	}
	// A later operation may end without ending the allocator's context.
	action, endAction := context.WithCancel(allocatedContext)
	endAction()
	if action.Err() == nil || allocatedContext.Err() != nil {
		t.Fatal("action cancellation ended browser lifetime")
	}
	cancel()
	if allocatedContext.Err() == nil {
		t.Fatal("owned cleanup did not cancel allocator")
	}
}

func TestCookieFactRejectsOpaquePartitionWithoutVisibleKey(t *testing.T) {
	opaque := &network.Cookie{PartitionKeyOpaque: true}
	if !cookieFact(opaque).Partitioned {
		t.Fatal("opaque partition was treated as unpartitioned")
	}
	keyed := &network.Cookie{PartitionKey: &network.CookiePartitionKey{}}
	if !cookieFact(keyed).Partitioned {
		t.Fatal("visible partition was treated as unpartitioned")
	}
	if cookieFact(&network.Cookie{}).Partitioned {
		t.Fatal("unpartitioned cookie was misclassified")
	}
}

func TestStartupTimeoutAbortsOwnedBrowser(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := startPersistentBrowser(ctx, time.Millisecond, cancel, func(received context.Context) error {
		<-received.Done()
		return received.Err()
	})
	if !errors.Is(err, context.Canceled) || ctx.Err() == nil {
		t.Fatal("startup timeout did not abort browser", err)
	}
}

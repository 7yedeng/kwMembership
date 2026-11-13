package checkout

import (
	"context"

	"github.com/chromedp/cdproto/cdp"
	cdppage "github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

// Frame IDs alone do not select a renderer. The fence registry supplies only
// already initialized targets owned by this checkout page, including its OOPIFs.
func (driver *chromePrepaymentDriver) inspectPrepaymentFrames(caller context.Context) ([]frameFact, error) {
	return driver.inspectPrepaymentFramesWithScript(caller, inspectFrameJS)
}

func (driver *chromePrepaymentDriver) inspectPrepaymentFramesWithScript(caller context.Context, script string) ([]frameFact, error) {
	if driver == nil || driver.fence == nil {
		return nil, prepaymentFail("PREPAYMENT_PAGE_UNSUPPORTED")
	}
	return inspectOwnedPrepaymentFrames(caller, driver.frameContexts(), script, chromePrepaymentFrameReader{})
}

// A small reader boundary lets unit tests exercise renderer ownership and
// cancellation without creating a browser or substituting any real hostname.
type prepaymentFrameReader interface {
	frameIDs(context.Context) ([]cdp.FrameID, error)
	inspect(context.Context, cdp.FrameID, string) (frameFact, error)
}

type chromePrepaymentFrameReader struct{}

func (chromePrepaymentFrameReader) frameIDs(ctx context.Context) ([]cdp.FrameID, error) {
	executor, err := initializedPrepaymentFrameExecutor(ctx)
	if err != nil {
		return nil, err
	}
	tree, err := cdppage.GetFrameTree().Do(executor)
	if err != nil {
		return nil, err
	}
	return flattenFrames(tree), nil
}

func (chromePrepaymentFrameReader) inspect(ctx context.Context, id cdp.FrameID, script string) (frameFact, error) {
	var fact frameFact
	executor, err := initializedPrepaymentFrameExecutor(ctx)
	if err == nil {
		err = evaluateFrameDirect(executor, id, script, &fact, false)
	}
	return fact, err
}

func inspectOwnedPrepaymentFrames(caller context.Context, targets []context.Context, script string, reader prepaymentFrameReader) ([]frameFact, error) {
	if caller == nil || len(targets) == 0 || script == "" || reader == nil {
		return nil, prepaymentFail("PREPAYMENT_PAGE_UNSUPPORTED")
	}
	if caller.Err() != nil {
		return nil, caller.Err()
	}
	var facts []frameFact
	required := make(map[cdp.FrameID]bool)
	read := make(map[cdp.FrameID]bool)
	for _, owned := range targets {
		if owned == nil {
			return nil, prepaymentFail("PREPAYMENT_PAGE_UNSUPPORTED")
		}
		err := withPrepaymentFrameContext(caller, owned, func(ctx context.Context) error {
			ids, err := reader.frameIDs(ctx)
			if err != nil {
				return err
			}
			if len(ids) == 0 {
				return prepaymentFail("PREPAYMENT_PAGE_UNSUPPORTED")
			}
			for _, id := range ids {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if id == "" {
					return prepaymentFail("PREPAYMENT_PAGE_UNSUPPORTED")
				}
				required[id] = true
				if read[id] {
					continue
				}
				fact, err := reader.inspect(ctx, id, script)
				if err != nil {
					if ctx.Err() != nil {
						return ctx.Err()
					}
					// A main-renderer tree can mention a remote frame whose
					// isolated world exists only in a later child target.
					// Do not mark it read until the owning target succeeds.
					continue
				}
				fact.FrameID, fact.TargetContext = id, owned
				facts = append(facts, fact)
				read[id] = true
			}
			return nil
		})
		if err != nil {
			if caller.Err() != nil {
				return nil, caller.Err()
			}
			// A detached owner is not interchangeable with the main tab.
			// Never return raw CDP error text or partial evidence as success.
			return nil, prepaymentFail("PREPAYMENT_PAGE_UNSUPPORTED")
		}
	}
	if caller.Err() != nil {
		return nil, caller.Err()
	}
	if len(facts) == 0 || len(read) != len(required) {
		// The caller may observe again while attachment/navigation settles.
		return nil, prepaymentFail("PREPAYMENT_PAGE_UNSUPPORTED")
	}
	return facts, nil
}

// Derive a short operation context without cancelling the registry's target.
// In chromedp, cancelling a WithTargetID context would also close that iframe.
func withPrepaymentFrameContext(caller, owned context.Context, action func(context.Context) error) error {
	if caller == nil || action == nil {
		return prepaymentFail("PREPAYMENT_PAGE_UNSUPPORTED")
	}
	if caller.Err() != nil {
		return caller.Err()
	}
	if owned == nil {
		owned = caller
	}
	if owned.Err() != nil {
		return owned.Err()
	}
	ctx, cancel := context.WithCancel(owned)
	stop := context.AfterFunc(caller, cancel)
	defer func() { stop(); cancel() }()
	if caller.Err() != nil {
		return caller.Err()
	}
	err := action(ctx)
	if caller.Err() != nil {
		return caller.Err()
	}
	if owned.Err() != nil {
		return owned.Err()
	}
	return err
}

func initializedPrepaymentFrameExecutor(ctx context.Context) (context.Context, error) {
	if ctx == nil || ctx.Err() != nil {
		return nil, prepaymentFail("PREPAYMENT_PAGE_UNSUPPORTED")
	}
	state := chromedp.FromContext(ctx)
	if state == nil || state.Browser == nil || state.Target == nil {
		return nil, prepaymentFail("PREPAYMENT_PAGE_UNSUPPORTED")
	}
	// Deliberately do not call chromedp.Run: an uninitialized context must not
	// allocate a browser or create a replacement tab during field inspection.
	return cdp.WithExecutor(ctx, state.Target), nil
}

func evaluatePrepaymentFrame(caller context.Context, frame frameFields, expression string, output any, userGesture bool) error {
	if frame.FrameID == "" {
		return prepaymentFail("PREPAYMENT_FIELDS_NOT_VERIFIED")
	}
	return withPrepaymentFrameContext(caller, frame.TargetContext, func(ctx context.Context) error {
		executor, err := initializedPrepaymentFrameExecutor(ctx)
		if err != nil {
			return prepaymentFail("PREPAYMENT_FIELDS_NOT_VERIFIED")
		}
		if err := evaluateFrameDirect(executor, frame.FrameID, expression, output, userGesture); err != nil {
			return prepaymentFail("PREPAYMENT_FIELDS_NOT_VERIFIED")
		}
		return nil
	})
}

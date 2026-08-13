package processor

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"os"
	"time"

	"kwmembership/internal/checkout"
	"kwmembership/internal/config"
	"kwmembership/internal/provider"
	"kwmembership/internal/secure"
	"kwmembership/internal/store"
)

type Processor struct {
	config     config.Config
	store      *store.Store
	lease      store.Lease
	decrypter  *secure.Decrypter
	httpClient *http.Client
	membership *provider.MembershipClient
	renewal    *provider.RenewalClient
	address    *provider.AddressClient
	executor   checkout.Executor
	now        func() time.Time
	// requireLeaseFence is set by New. Package tests may deliberately construct
	// a zero-value Processor literal around a minimal schema; production callers
	// cannot bypass the lease fence because they enter through New and RunOnce.
	requireLeaseFence bool
}

type Result struct {
	IntakeProcessed         bool
	InventoryProcessed      bool
	EligibilityProcessed    bool
	PaymentProcessed        bool
	CheckoutProcessed       bool
	ReconciliationProcessed bool
}

func New(cfg config.Config, repository *store.Store, lease store.Lease, decrypter *secure.Decrypter) *Processor {
	return NewWithExecutor(cfg, repository, lease, decrypter,
		checkout.NewChromeExecutor(cfg.ChromePath, cfg.ChromeProxyServer, cfg.VisibleBrowser, cfg.BrowserTimeout, cfg.HumanChallengeTimeout))
}

func NewWithExecutor(cfg config.Config, repository *store.Store, lease store.Lease, decrypter *secure.Decrypter, executor checkout.Executor) *Processor {
	client := &http.Client{
		Timeout: cfg.HTTPTimeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return &Processor{
		config: cfg, store: repository, lease: lease, decrypter: decrypter,
		httpClient:        client,
		membership:        provider.NewMembershipClient(client),
		renewal:           provider.NewRenewalClient(client),
		address:           provider.NewAddressClient(client, cfg.APIURL),
		executor:          executor,
		now:               time.Now,
		requireLeaseFence: true,
	}
}

// RunOnce is the external interface of the Go Membership Processor module.
// It owns all membership due-work; callers only schedule it and observe the
// durable projection already exposed by the Node API/admin UI.
func (p *Processor) RunOnce(ctx context.Context) (Result, error) {
	if err := p.assertWorkAllowed(ctx); err != nil {
		if errors.Is(err, errProcessorStandby) {
			return Result{}, nil
		}
		return Result{}, err
	}
	var result Result
	var firstError error
	processed, err := p.tickIntake(ctx)
	if err != nil {
		firstError = err
	}
	result.IntakeProcessed = processed
	if err := p.assertWorkAllowed(ctx); err != nil {
		if errors.Is(err, errProcessorStandby) {
			return result, withoutStandby(firstError)
		}
		return result, err
	}
	processed, err = p.tickInventory(ctx)
	if err != nil && firstError == nil {
		firstError = err
	}
	result.InventoryProcessed = processed
	if err := p.assertWorkAllowed(ctx); err != nil {
		if errors.Is(err, errProcessorStandby) {
			return result, withoutStandby(firstError)
		}
		return result, err
	}
	processed, err = p.tickEligibility(ctx)
	if err != nil && firstError == nil {
		firstError = err
	}
	result.EligibilityProcessed = processed
	if err := p.assertWorkAllowed(ctx); err != nil {
		if errors.Is(err, errProcessorStandby) {
			return result, withoutStandby(firstError)
		}
		return result, err
	}
	processed, err = p.tickPayment(ctx)
	if err != nil && firstError == nil {
		firstError = err
	}
	result.PaymentProcessed = processed
	if err := p.assertWorkAllowed(ctx); err != nil {
		if errors.Is(err, errProcessorStandby) {
			return result, withoutStandby(firstError)
		}
		return result, err
	}
	processed, err = p.tickCheckout(ctx)
	if err != nil && firstError == nil {
		firstError = err
	}
	result.CheckoutProcessed = processed
	if err := p.assertWorkAllowed(ctx); err != nil {
		if errors.Is(err, errProcessorStandby) {
			return result, withoutStandby(firstError)
		}
		return result, err
	}
	processed, err = p.tickReconciliation(ctx)
	if err != nil && firstError == nil {
		firstError = err
	}
	result.ReconciliationProcessed = processed
	return result, firstError
}

var errProcessorStandby = errors.New("membership processor is in maintenance standby")

func withoutStandby(err error) error {
	if errors.Is(err, errProcessorStandby) {
		return nil
	}
	return err
}

func (p *Processor) assertWorkAllowed(ctx context.Context) error {
	if _, err := os.Stat(p.config.MaintenancePath); err == nil {
		return errProcessorStandby
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return p.store.AssertLease(ctx, p.lease, p.now())
}

func (p *Processor) withFencedImmediate(ctx context.Context, fn func(*sql.Tx) error) error {
	return p.store.WithImmediate(ctx, func(tx *sql.Tx) error {
		if p.requireLeaseFence || p.lease != (store.Lease{}) {
			if err := p.store.AssertLeaseTx(ctx, tx, p.lease, p.now()); err != nil {
				return err
			}
		}
		return fn(tx)
	})
}

func (p *Processor) fencedExec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	var result sql.Result
	err := p.withFencedImmediate(ctx, func(tx *sql.Tx) error {
		var err error
		result, err = tx.ExecContext(ctx, query, args...)
		return err
	})
	return result, err
}

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"kwmembership/internal/checkout"
	"kwmembership/internal/config"
	executorbridge "kwmembership/internal/executor"
	"kwmembership/internal/processor"
	"kwmembership/internal/secure"
	"kwmembership/internal/store"
)

var version = "dev"

// maintenanceDrain serializes tick admission with lease-status publication.
// Reading the marker and marking a tick in flight must be one operation;
// otherwise a heartbeat can publish standby in the gap before RunOnce starts.
type maintenanceDrain struct {
	mu          sync.Mutex
	inFlight    bool
	leaseStatus string
}

func newMaintenanceDrain() *maintenanceDrain {
	return &maintenanceDrain{leaseStatus: "active"}
}

func (drain *maintenanceDrain) beginTick(
	ctx context.Context,
	cfg config.Config,
	repository *store.Store,
	lease store.Lease,
) (bool, error) {
	drain.mu.Lock()
	defer drain.mu.Unlock()
	status, err := maintenanceStatus(cfg.MaintenancePath)
	if err != nil {
		return false, err
	}
	if status != "active" || drain.inFlight {
		return false, nil
	}
	// A removed marker may race a previously published standby heartbeat.
	// Restore the durable lease status before admitting new work.
	if drain.leaseStatus != "active" {
		if err := repository.HeartbeatLease(ctx, lease, "active", time.Now(), cfg.LeaseTTL); err != nil {
			return false, err
		}
		drain.leaseStatus = "active"
	}
	drain.inFlight = true
	return true, nil
}

func (drain *maintenanceDrain) finishTick() {
	drain.mu.Lock()
	drain.inFlight = false
	drain.mu.Unlock()
}

func (drain *maintenanceDrain) heartbeat(
	ctx context.Context,
	cfg config.Config,
	repository *store.Store,
	lease store.Lease,
) error {
	drain.mu.Lock()
	defer drain.mu.Unlock()
	status, err := maintenanceStatus(cfg.MaintenancePath)
	if err != nil {
		return err
	}
	if status == "standby" && drain.inFlight {
		status = "active"
	}
	if err := repository.HeartbeatLease(ctx, lease, status, time.Now(), cfg.LeaseTTL); err != nil {
		return err
	}
	drain.leaseStatus = status
	return nil
}

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--check" {
		if err := checkConfiguration(); err != nil {
			log.Printf("[kwMembership worker] configuration invalid: %v", err)
			os.Exit(1)
		}
		log.Printf("[kwMembership worker] shared kwRedeem database configuration is valid")
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx); err != nil {
		log.Printf("[kwMembership worker] stopped: %v", err)
		os.Exit(1)
	}
}

func checkConfiguration() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if _, err := os.Stat(cfg.DatabasePath); err != nil {
		return fmt.Errorf("open kwRedeem database path: %w", err)
	}
	repository, err := store.Open(cfg.DatabasePath)
	if err != nil {
		return err
	}
	defer repository.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := repository.VerifySharedSchema(ctx); err != nil {
		return err
	}
	if cfg.CheckoutExecutor == "legacy-go" {
		return checkout.VerifyChrome(ctx, cfg.ChromePath, cfg.ChromeProxyServer, cfg.VisibleBrowser)
	}
	return nil
}

func run(ctx context.Context) (runErr error) {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}
	repository, err := store.Open(cfg.DatabasePath)
	if err != nil {
		return err
	}
	defer func() {
		if err := repository.Close(); err != nil {
			runErr = errors.Join(runErr, fmt.Errorf("close membership store: %w", err))
		}
	}()

	now := time.Now()
	if err := repository.EnsureLeaseTable(ctx, now); err != nil {
		return err
	}
	decrypter, err := secure.NewDecrypter(cfg.EncryptionKey)
	if err != nil {
		return fmt.Errorf("initialize credential decrypter: %w", err)
	}
	holderToken, err := randomHolderToken()
	if err != nil {
		return err
	}
	lease, err := repository.AcquireLease(ctx, "go", holderToken, version, now, cfg.LeaseTTL)
	if err != nil {
		return fmt.Errorf("acquire membership processor lease: %w", err)
	}
	log.Printf("[kwMembership worker] lease acquired at epoch %d (version %s)", lease.Epoch, version)

	var paymentExecutor checkout.Executor
	var executorServer *http.Server
	var executorListener net.Listener
	if cfg.CheckoutExecutor == "python" {
		bridge, err := executorbridge.NewBridge(repository.DB(), cfg.ExecutorSecret)
		if err != nil {
			return fmt.Errorf("initialize Python executor bridge: %w", err)
		}
		executorListener, err = net.Listen("tcp", cfg.ExecutorListenAddress)
		if err != nil {
			return fmt.Errorf("listen for Python executor: %w", err)
		}
		executorServer = &http.Server{
			Handler: bridge.Handler(), ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 30 * time.Second,
		}
		paymentExecutor = bridge
		log.Printf("[kwMembership worker] Python executor bridge listening on %s", cfg.ExecutorListenAddress)
	} else {
		paymentExecutor = checkout.NewChromeExecutor(cfg.ChromePath, cfg.ChromeProxyServer, cfg.VisibleBrowser, cfg.BrowserTimeout, cfg.HumanChallengeTimeout)
	}

	runner := processor.NewWithExecutor(cfg, repository, lease, decrypter, paymentExecutor)
	runErr = runLoopWithExecutorServer(ctx, cfg, repository, lease, runner, executorServer, executorListener)

	releaseCtx, cancelRelease := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelRelease()
	if err := repository.ReleaseLease(releaseCtx, lease, time.Now()); err != nil {
		runErr = errors.Join(runErr, fmt.Errorf("release membership processor lease: %w", err))
	} else if runErr == nil {
		log.Printf("[kwMembership worker] lease released")
	}
	return runErr
}

func runLoopWithExecutorServer(
	ctx context.Context,
	cfg config.Config,
	repository *store.Store,
	lease store.Lease,
	runner *processor.Processor,
	server *http.Server,
	listener net.Listener,
) error {
	if server == nil {
		return runLoop(ctx, cfg, repository, lease, runner)
	}
	serverCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	serverErrors := make(chan error, 1)
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErrors <- err
			cancel()
		}
	}()
	loopErrors := make(chan error, 1)
	go func() { loopErrors <- runLoop(serverCtx, cfg, repository, lease, runner) }()
	var result error
	loopStopped := false
	select {
	case err := <-serverErrors:
		result = fmt.Errorf("Python executor bridge failed: %w", err)
	case result = <-loopErrors:
		loopStopped = true
	}
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		result = errors.Join(result, fmt.Errorf("shutdown Python executor bridge: %w", err))
	}
	cancel()
	if !loopStopped {
		select {
		case loopErr := <-loopErrors:
			if result == nil {
				result = loopErr
			}
		case <-time.After(5 * time.Second):
			result = errors.Join(result, errors.New("membership run loop did not stop after executor bridge shutdown"))
		}
	}
	return result
}

func runLoop(
	parent context.Context,
	cfg config.Config,
	repository *store.Store,
	lease store.Lease,
	runner *processor.Processor,
) error {
	ctx, cancel := context.WithCancel(parent)
	heartbeatErrors := make(chan error, 1)
	drain := newMaintenanceDrain()
	var heartbeats sync.WaitGroup
	heartbeats.Add(1)
	go func() {
		defer heartbeats.Done()
		if err := heartbeatLoop(ctx, cfg, repository, lease, drain); err != nil {
			heartbeatErrors <- err
			cancel()
		}
	}()
	defer func() {
		cancel()
		heartbeats.Wait()
	}()

	poll := time.NewTimer(0)
	defer poll.Stop()
	for {
		select {
		case err := <-heartbeatErrors:
			return heartbeatFailure(err)
		case <-parent.Done():
			return stopError(parent, heartbeatErrors)
		case <-ctx.Done():
			return stopError(parent, heartbeatErrors)
		case <-poll.C:
			started, err := drain.beginTick(ctx, cfg, repository, lease)
			if err != nil {
				return err
			}
			if started {
				err := func() error {
					defer drain.finishTick()
					return runTick(ctx, repository, lease, runner)
				}()
				if err != nil {
					if errors.Is(err, store.ErrLeaseLost) {
						return err
					}
					if ctx.Err() != nil {
						return stopError(parent, heartbeatErrors)
					}
					return err
				}
			}
			poll.Reset(cfg.PollInterval)
		}
	}
}

func runTick(ctx context.Context, repository *store.Store, lease store.Lease, runner *processor.Processor) error {
	result, tickErr := runner.RunOnce(ctx)
	if errors.Is(tickErr, store.ErrLeaseLost) {
		return fmt.Errorf("membership processor lease lost during tick: %w", tickErr)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err := repository.RecordTick(ctx, lease, time.Now(), tickErr); err != nil {
		return fmt.Errorf("record membership processor tick: %w", err)
	}
	if tickErr != nil {
		log.Printf("[kwMembership worker] tick failed (%s): %v", store.ErrorCode(tickErr), tickErr)
		return nil
	}
	if result.IntakeProcessed || result.InventoryProcessed || result.EligibilityProcessed || result.PaymentProcessed || result.CheckoutProcessed || result.ReconciliationProcessed {
		log.Printf(
			"[kwMembership worker] tick completed (intake=%t inventory=%t eligibility=%t payment=%t checkout=%t reconciliation=%t)",
			result.IntakeProcessed,
			result.InventoryProcessed,
			result.EligibilityProcessed,
			result.PaymentProcessed,
			result.CheckoutProcessed,
			result.ReconciliationProcessed,
		)
	}
	return nil
}

func heartbeatLoop(
	ctx context.Context,
	cfg config.Config,
	repository *store.Store,
	lease store.Lease,
	drain *maintenanceDrain,
) error {
	ticker := time.NewTicker(cfg.HeartbeatInterval)
	defer ticker.Stop()
	for {
		if err := drain.heartbeat(ctx, cfg, repository, lease); err != nil {
			if errors.Is(err, store.ErrLeaseLost) {
				return err
			}
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func maintenanceStatus(path string) (string, error) {
	_, err := os.Stat(path)
	if err == nil {
		return "standby", nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return "active", nil
	}
	return "", fmt.Errorf("inspect maintenance marker: %w", err)
}

func heartbeatFailure(err error) error {
	if errors.Is(err, store.ErrLeaseLost) {
		return fmt.Errorf("membership processor lease lost during heartbeat: %w", err)
	}
	return fmt.Errorf("membership processor heartbeat failed: %w", err)
}

func stopError(parent context.Context, heartbeatErrors <-chan error) error {
	select {
	case err := <-heartbeatErrors:
		return heartbeatFailure(err)
	default:
	}
	if parent.Err() != nil {
		return nil
	}
	return fmt.Errorf("membership processor stopped unexpectedly: %w", context.Canceled)
}

func randomHolderToken() (string, error) {
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("generate membership processor holder token: %w", err)
	}
	return "go-worker-" + hex.EncodeToString(random), nil
}

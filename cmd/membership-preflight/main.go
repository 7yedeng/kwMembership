package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"kwmembership/internal/checkout"
	"kwmembership/internal/preflight"
)

const usage = `membership-preflight [--check | --inspect-session | --run | --interactive-preflight] < input.json

Default --check validates local input only; remoteVerified remains false.
--inspect-session reads a raw Session and prints known field types only.
--run can create a remote Plus checkout session, but cannot fill or submit payment.
--interactive-preflight accepts metadata only, opens a fresh headed browser, and
waits for the user to log in and navigate to a Plus or Go checkout without collecting tokens.
Go is observation-only: automatic Session checkout and payment remain unsupported.
Egyptian Go observation requires explicit priceContract.country=EG and currency=EGP.
Legacy PH/PHP input remains supported. Amounts must be supplied, never inferred.
Run options: --chrome-path PATH [--proxy-server URL] [--headed] [--timeout 90s]
Interactive login has a fixed 5m wait and a 5m30s outer browser budget; --timeout
does not shorten these. SIGINT/SIGTERM also cancel an unfinished stdin read.
Session credentials are accepted only through stdin. No worker database is opened.
`

type executorFactory func(preflight.BrowserOptions) (checkout.Executor, error)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	exitCode := runCLI(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr, preflight.NewBrowserExecutor, time.Now)
	stop()
	os.Exit(exitCode)
}

func runCLI(ctx context.Context, arguments []string, stdin io.Reader, stdout, stderr io.Writer, factory executorFactory, now func() time.Time) int {
	flags := flag.NewFlagSet("membership-preflight", flag.ContinueOnError)
	// flag errors can echo an argument value; replace them with a safe code.
	flags.SetOutput(io.Discard)
	check := flags.Bool("check", false, "validate local input")
	run := flags.Bool("run", false, "run non-payment checkout preflight")
	inspect := flags.Bool("inspect-session", false, "inspect known raw Session fields")
	interactive := flags.Bool("interactive-preflight", false, "wait for login and checkout in a fresh headed browser")
	chromePath := flags.String("chrome-path", "", "Chrome executable")
	proxy := flags.String("proxy-server", "", "explicit proxy")
	headed := flags.Bool("headed", false, "private display browser")
	timeout := flags.Duration("timeout", 90*time.Second, "browser timeout")
	if err := flags.Parse(arguments); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, _ = io.WriteString(stderr, usage)
			return 0
		}
		return writeReport(stdout, preflight.Failure("check", "PREFLIGHT_ARGUMENTS_INVALID"), 2)
	}
	modes := 0
	for _, selected := range []bool{*check, *run, *inspect, *interactive} {
		if selected {
			modes++
		}
	}
	if modes > 1 || flags.NArg() != 0 {
		return writeReport(stdout, preflight.Failure("check", "PREFLIGHT_ARGUMENTS_INVALID"), 2)
	}
	operation := "check"
	inputLimit := int64(preflight.MaxInputBytes)
	if *run {
		operation = "run"
	} else if *interactive {
		operation = "interactive-preflight"
	} else if *inspect {
		operation = "inspect-session"
		inputLimit = preflight.MaxSessionBytes
	}
	raw, err := preflight.ReadInput(ctx, stdin, inputLimit)
	if err != nil {
		return writeReport(stdout, preflight.Failure(operation, preflight.SafeCode(err)), 2)
	}
	if *inspect {
		report, err := preflight.InspectSession(bytes.NewReader(raw), now())
		if err != nil {
			return writeReport(stdout, preflight.Failure("inspect-session", preflight.SafeCode(err)), 2)
		}
		return writeReport(stdout, report, 0)
	}
	var prepared preflight.Prepared
	if *interactive {
		prepared, err = preflight.DecodeInteractive(bytes.NewReader(raw))
	} else {
		prepared, err = preflight.Decode(bytes.NewReader(raw), now())
	}
	if err != nil {
		return writeReport(stdout, preflight.Failure(operation, preflight.SafeCode(err)), 2)
	}
	if !*run && !*interactive {
		return writeReport(stdout, preflight.Check(prepared), 0)
	}
	executor, err := factory(preflight.BrowserOptions{
		ChromePath: *chromePath, ProxyServer: *proxy, Headed: *headed || *interactive, Timeout: *timeout,
	})
	if err != nil {
		return writeReport(stdout, preflight.Failure(operation, preflight.SafeCode(err)), 2)
	}
	var report preflight.Report
	if *interactive {
		report, err = preflight.RunInteractive(ctx, prepared, executor, func() error {
			_, err := io.WriteString(stderr, "INTERACTIVE_LOGIN_REQUIRED\n")
			return err
		})
	} else {
		report, err = preflight.Run(ctx, prepared, executor)
	}
	if err != nil {
		report.Code = preflight.SafeCode(err)
		return writeReport(stdout, report, 1)
	}
	return writeReport(stdout, report, 0)
}

func writeReport(output io.Writer, report preflight.Report, exitCode int) int {
	if err := json.NewEncoder(output).Encode(report); err != nil {
		// Do not print a writer error: it can contain sensitive destination data.
		return 1
	}
	return exitCode
}

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"kwmembership/internal/preparation"
)

const usage = `membership-prepare [--check | --run --chrome-path PATH] < input.json

Default --check validates input offline and does not construct a browser or API client.
--run restores the supplied account, prepares Plus PH/PHP checkout, reads one
selected existing Zovo card snapshot, fills actual billing/card data, verifies
the final price and fields, then stops before every payment-control activation.
The same card snapshot supplies billing and card material; no opening, funding,
membership/direct-charge API, submit or renewal action is exposed.
All inputs, including Session and API credentials, come through stdin only.
Options: --show-browser [--hold-open 2m] [--timeout 3m]
Hold is limited to 5m and requires a visible browser. There is no pay command.
--gui-protocol accepts one compact input JSON line; any later input or EOF cancels.
stdout is redacted NDJSON: {event:"ready"|"result",report:{...}}.
ready is emitted before optional hold; result follows browser cleanup.
Unknown card-balance currency is reported as unknown, never assumed to be USD.
`

type event struct {
	Event  string             `json:"event"`
	Report preparation.Report `json:"report"`
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := runCLI(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr, preparation.NewExecutor, preparation.NewCardReader, time.Now)
	stop()
	os.Exit(code)
}

func runCLI(parent context.Context, arguments []string, stdin io.Reader, stdout, stderr io.Writer,
	executorFactory preparation.ExecutorFactory, readerFactory preparation.ReaderFactory, now func() time.Time) int {
	flags := flag.NewFlagSet("membership-prepare", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	check := flags.Bool("check", false, "validate input without browser or network")
	execute := flags.Bool("run", false, "prepare checkout without submission")
	chrome := flags.String("chrome-path", "", "Chrome executable")
	show := flags.Bool("show-browser", false, "show isolated browser")
	hold := flags.Duration("hold-open", 0, "hold the reviewed browser for observation")
	timeout := flags.Duration("timeout", 3*time.Minute, "operation timeout")
	gui := flags.Bool("gui-protocol", false, "private stdin cancellation protocol")
	var outputMu sync.Mutex
	writeEvent := func(kind string, report preparation.Report) error {
		outputMu.Lock()
		defer outputMu.Unlock()
		return json.NewEncoder(stdout).Encode(event{Event: kind, Report: report})
	}
	failure := func(code string, exit int) int {
		if writeEvent("result", preparation.Failure(code)) != nil {
			return 1
		}
		return exit
	}
	if err := flags.Parse(arguments); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, _ = io.WriteString(stderr, usage)
			return 0
		}
		return failure("PREPARATION_ARGUMENTS_INVALID", 2)
	}
	options := preparation.Options{ChromePath: *chrome, ShowBrowser: *show, HoldOpen: *hold, Timeout: *timeout}
	if flags.NArg() != 0 || *check && *execute || *timeout < time.Second || *timeout > 5*time.Minute ||
		!*execute && (*show || *hold != 0 || *chrome != "") || *execute && options.Validate() != nil {
		return failure("PREPARATION_ARGUMENTS_INVALID", 2)
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	raw, err := readInput(ctx, stdin, *gui, cancel)
	if err != nil {
		return failure(preparation.SafeCode(err), 2)
	}
	input, err := preparation.Parse(raw, now())
	clear(raw)
	if err != nil {
		return failure(preparation.SafeCode(err), 2)
	}
	defer input.Clear()
	if !*execute {
		if writeEvent("result", preparation.Check(input)) != nil {
			return 1
		}
		return 0
	}
	var outputErr error
	report, err := preparation.Run(ctx, input, options, executorFactory, readerFactory, now, preparation.Callbacks{
		OnProgress: func(stage string) {
			if preparation.IsProgressPhase(stage) {
				_, _ = io.WriteString(stderr, "PREPARATION_STAGE "+stage+"\n")
			}
		},
		OnReady: func(report preparation.Report) {
			if outputErr = writeEvent("ready", report); outputErr != nil {
				cancel()
			}
		},
	})
	if outputErr != nil || writeEvent("result", report) != nil {
		return 1
	}
	if err != nil {
		return 1
	}
	return 0
}

type inputError struct{ code string }

func (e inputError) Error() string { return e.code }
func (e inputError) Code() string  { return e.code }

func readInput(ctx context.Context, input io.Reader, gui bool, cancel context.CancelFunc) ([]byte, error) {
	type readResult struct {
		raw    []byte
		err    error
		reader *bufio.Reader
	}
	done := make(chan readResult, 1)
	go func() {
		if gui {
			reader := bufio.NewReaderSize(input, preparation.MaxInputBytes+2)
			line, err := reader.ReadSlice('\n')
			if errors.Is(err, bufio.ErrBufferFull) || len(line) > preparation.MaxInputBytes+1 {
				done <- readResult{err: inputError{"PREPARATION_INPUT_TOO_LARGE"}}
				return
			}
			if err != nil {
				done <- readResult{err: inputError{"PREPARATION_INPUT_INVALID"}}
				return
			}
			done <- readResult{raw: bytes.Clone(bytes.TrimSuffix(line, []byte{'\n'})), reader: reader}
			return
		}
		raw, err := io.ReadAll(io.LimitReader(input, preparation.MaxInputBytes+1))
		if len(raw) > preparation.MaxInputBytes {
			err = inputError{"PREPARATION_INPUT_TOO_LARGE"}
		} else if err != nil {
			err = inputError{"PREPARATION_INPUT_INVALID"}
		}
		done <- readResult{raw: raw, err: err}
	}()
	select {
	case <-ctx.Done():
		if closer, ok := input.(io.Closer); ok {
			_ = closer.Close()
		}
		return nil, ctx.Err()
	case read := <-done:
		if ctx.Err() != nil {
			clear(read.raw)
			return nil, ctx.Err()
		}
		if read.err != nil {
			clear(read.raw)
			return nil, read.err
		}
		if gui {
			go func() {
				// There is no positive confirmation command. An attempted pay,
				// cancel, extra JSON or a disconnected parent all cancel safely.
				_, _ = read.reader.ReadByte()
				cancel()
			}()
		}
		return read.raw, nil
	}
}

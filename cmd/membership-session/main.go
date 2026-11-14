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
	"strings"
	"syscall"
	"time"

	"kwmembership/internal/sessionrestore"
)

const usage = `membership-session [--inspect | --restore] < session.json

Default --inspect checks supplied Session locally. It never opens a browser.
--restore requires --chrome-path PATH and opens a fresh disposable profile.
It restores existing cookies, checks exact readback, and verifies user ID/email.
No checkout, card, payment, renewal operation or existing profile attachment exists.
Options: --show-browser [--hold-open 2m] [--timeout 90s]
--hold-open is limited to 5m and requires --show-browser.
Credentials are accepted through stdin only, never command-line arguments.
Output is a redacted JSON report. SESSION_VERIFIED requires a remote identity match.
Browser display is optional and is unrelated to whether credentials are valid.
--gui-protocol reads one compact JSON line, then accepts cancel or EOF on stdin.
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := runCLI(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr, sessionrestore.NewChrome, time.Now)
	stop()
	os.Exit(code)
}

func runCLI(parent context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, factory sessionrestore.Factory, now func() time.Time) int {
	flags := flag.NewFlagSet("membership-session", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	inspect := flags.Bool("inspect", false, "inspect local material")
	restore := flags.Bool("restore", false, "restore and verify identity")
	chrome := flags.String("chrome-path", "", "Chrome executable")
	show := flags.Bool("show-browser", false, "show disposable browser")
	hold := flags.Duration("hold-open", 0, "keep verified browser open for observation")
	timeout := flags.Duration("timeout", 90*time.Second, "browser operation budget")
	gui := flags.Bool("gui-protocol", false, "private GUI stdin cancellation protocol")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, _ = io.WriteString(stderr, usage)
			return 0
		}
		return output(stdout, sessionrestore.Failure("inspect", "SESSION_ARGUMENTS_INVALID"), 2)
	}
	operation := "inspect"
	if *restore {
		operation = "restore"
	}
	options := sessionrestore.Options{ChromePath: *chrome, ShowBrowser: *show, Timeout: *timeout, HoldOpen: *hold}
	if flags.NArg() != 0 || *inspect && *restore || options.Validate() != nil ||
		!(*restore) && (*show || *hold != 0) || *restore && strings.TrimSpace(*chrome) == "" {
		return output(stdout, sessionrestore.Failure(operation, "SESSION_ARGUMENTS_INVALID"), 2)
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	raw, err := readInput(ctx, stdin, *gui, cancel)
	if err != nil {
		return output(stdout, sessionrestore.Failure(operation, sessionrestore.SafeCode(err)), 2)
	}
	prepared, err := sessionrestore.Parse(raw, now())
	clear(raw)
	if err != nil {
		return output(stdout, sessionrestore.Failure(operation, sessionrestore.SafeCode(err)), 2)
	}
	if !*restore {
		return output(stdout, sessionrestore.Inspect(prepared), 0)
	}
	report, err := sessionrestore.Run(ctx, prepared, options, factory, func(stage string) {
		_, _ = io.WriteString(stderr, "SESSION_STAGE "+stage+"\n")
	})
	if err != nil {
		return output(stdout, report, 1)
	}
	return output(stdout, report, 0)
}

type inputError struct{ code string }

func (e inputError) Error() string { return e.code }
func (e inputError) Code() string  { return e.code }

func readInput(ctx context.Context, input io.Reader, gui bool, cancel context.CancelFunc) ([]byte, error) {
	type result struct {
		data   []byte
		err    error
		reader *bufio.Reader
	}
	done := make(chan result, 1)
	go func() {
		if gui {
			reader := bufio.NewReaderSize(input, sessionrestore.MaxSessionBytes+2)
			line, err := reader.ReadSlice('\n')
			if errors.Is(err, bufio.ErrBufferFull) || len(line) > sessionrestore.MaxSessionBytes+1 {
				done <- result{err: inputError{"SESSION_INPUT_TOO_LARGE"}}
				return
			}
			if err != nil {
				done <- result{err: inputError{"SESSION_INPUT_INVALID"}}
				return
			}
			done <- result{data: bytes.Clone(bytes.TrimSuffix(line, []byte{'\n'})), reader: reader}
			return
		}
		raw, err := io.ReadAll(io.LimitReader(input, sessionrestore.MaxSessionBytes+1))
		if len(raw) > sessionrestore.MaxSessionBytes {
			err = inputError{"SESSION_INPUT_TOO_LARGE"}
		}
		if err != nil && len(raw) <= sessionrestore.MaxSessionBytes {
			err = inputError{"SESSION_INPUT_INVALID"}
		}
		done <- result{data: raw, err: err}
	}()
	select {
	case <-ctx.Done():
		if closer, ok := input.(io.Closer); ok {
			_ = closer.Close()
		}
		return nil, ctx.Err()
	case read := <-done:
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if read.err != nil {
			return nil, read.err
		}
		if gui {
			go func() {
				// Only cancellation is accepted after the first line. EOF means
				// the controlling GUI disappeared; always close its browser.
				_, _ = read.reader.ReadByte()
				cancel()
			}()
		}
		return read.data, nil
	}
}

func output(writer io.Writer, report sessionrestore.Report, code int) int {
	if json.NewEncoder(writer).Encode(report) != nil {
		return 1
	}
	return code
}

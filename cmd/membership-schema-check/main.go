package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"kwmembership/internal/schemacheck"
)

const usage = `membership-schema-check --database PATH [--timeout 10s]

Inspect schema metadata in an existing quiescent SQLite snapshot only.
The connection uses mode=ro, immutable=1, query_only=1. WAL/SHM/journal sidecars
are refused. No tables, rows, migrations, checkpoints, or worker are created.
Output contains known missing/incompatible requirements and unresolved unknowns.
Exit 0: help; 1: known schema gaps; 2: input/inspection failure; 3: no known gap
found but unresolved schema requirements remain. No result enables payment.
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	status := runCLI(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(status)
}

func runCLI(ctx context.Context, arguments []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("membership-schema-check", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	path := flags.String("database", "", "existing offline SQLite snapshot")
	timeout := flags.Duration("timeout", 10*time.Second, "inspection deadline")
	if err := flags.Parse(arguments); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, _ = io.WriteString(stderr, usage)
			return 0
		}
		return writeReport(stdout, schemacheck.Failure("SCHEMA_ARGUMENTS_INVALID"), 2)
	}
	if *path == "" || flags.NArg() != 0 || *timeout < time.Millisecond || *timeout > time.Minute {
		return writeReport(stdout, schemacheck.Failure("SCHEMA_ARGUMENTS_INVALID"), 2)
	}
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	report, err := schemacheck.Inspect(ctx, *path)
	if err != nil {
		return writeReport(stdout, schemacheck.Failure(schemacheck.SafeCode(err)), 2)
	}
	status := 1
	if report.Status == "unknown" {
		status = 3
	}
	return writeReport(stdout, report, status)
}

func writeReport(output io.Writer, report schemacheck.Report, status int) int {
	if json.NewEncoder(output).Encode(report) != nil {
		return 2
	}
	return status
}

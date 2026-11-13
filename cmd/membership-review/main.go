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

	"kwmembership/internal/checkout"
	"kwmembership/internal/preflight"
)

const usage = `membership-review [--example] < quote.json

Default: validate locally supplied quote metadata and print a JSON review.
All monetary inputs are decimal STRINGS. PHP and EGP use two minor-unit digits.
Every subtotal, tax, discount, total and taxIncluded value must be explicit.
This command never opens a browser, account, database or card platform.
reviewReady means arithmetic/target/budget are complete; paymentAllowed and
remoteVerified are always false. It is not proof of a live merchant quote.
--example prints a synthetic Go/EG request to stdout, not a current price.
SIGINT/SIGTERM cancel an unfinished stdin read. Maximum input: 16 KiB.
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := runCLI(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

func runCLI(ctx context.Context, arguments []string, stdin io.Reader, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("membership-review", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	example := flags.Bool("example", false, "print a synthetic example")
	if err := flags.Parse(arguments); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, _ = io.WriteString(stderr, usage)
			return 0
		}
		return writeReport(stdout, checkout.QuoteFailure("QUOTE_ARGUMENTS_INVALID"), 2)
	}
	if flags.NArg() != 0 {
		return writeReport(stdout, checkout.QuoteFailure("QUOTE_ARGUMENTS_INVALID"), 2)
	}
	if err := ctx.Err(); err != nil {
		return writeReport(stdout, checkout.QuoteFailure(inputCode(err)), 2)
	}
	if *example {
		_, _ = io.WriteString(stderr, "SYNTHETIC_NOT_A_LIVE_PRICE: example Go/EGP numbers are invented for local testing.\n")
		if json.NewEncoder(stdout).Encode(exampleRequest()) != nil {
			return 1
		}
		return 0
	}
	raw, err := preflight.ReadInput(ctx, stdin, checkout.MaxQuoteInputBytes)
	if err != nil {
		return writeReport(stdout, checkout.QuoteFailure(inputCode(err)), 2)
	}
	defer clear(raw)
	request, err := checkout.DecodeQuoteReview(bytes.NewReader(raw))
	if err != nil {
		return writeReport(stdout, checkout.QuoteFailure(checkout.SafeQuoteCode(err)), 2)
	}
	report := checkout.ReviewQuote(request)
	if !report.ReviewReady {
		return writeReport(stdout, report, 2)
	}
	return writeReport(stdout, report, 0)
}

func inputCode(err error) string {
	switch preflight.SafeCode(err) {
	case "PREFLIGHT_CANCELLED":
		return "QUOTE_CANCELLED"
	case "PREFLIGHT_TIMEOUT":
		return "QUOTE_TIMEOUT"
	case "PREFLIGHT_INPUT_TOO_LARGE":
		return "QUOTE_INPUT_TOO_LARGE"
	default:
		return "QUOTE_INPUT_INVALID"
	}
}

func writeReport(output io.Writer, report checkout.QuoteReviewReport, code int) int {
	if json.NewEncoder(output).Encode(report) != nil {
		return 1
	}
	return code
}

func exampleRequest() checkout.QuoteReviewRequest {
	subtotal, tax, discount, total := "200.00", "28.00", "0.00", "228.00"
	taxIncluded := false
	return checkout.QuoteReviewRequest{
		Expected: checkout.QuoteTarget{Plan: "go", Country: "EG", Currency: "EGP"},
		Evidence: checkout.QuoteEvidence{
			Plan: "go", Country: "EG", Currency: "EGP",
			Subtotal: &subtotal, Tax: &tax, Discount: &discount, Total: &total, TaxIncluded: &taxIncluded,
		},
		Budget: checkout.QuoteBudget{Min: "220.00", Max: "230.00"},
	}
}

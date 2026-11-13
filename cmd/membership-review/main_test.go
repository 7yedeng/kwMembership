package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"kwmembership/internal/checkout"
)

func requestJSON(t *testing.T) string {
	t.Helper()
	raw, err := json.Marshal(exampleRequest())
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
func decodeReport(t *testing.T, output *bytes.Buffer) checkout.QuoteReviewReport {
	t.Helper()
	var report checkout.QuoteReviewReport
	if err := json.Unmarshal(output.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.PaymentAllowed || report.RemoteVerified {
		t.Fatal("review must never authorize payment or remote proof")
	}
	return report
}

func TestReviewCLIExampleIsSyntheticAndRoundTripsWithoutNetwork(t *testing.T) {
	var example, note bytes.Buffer
	if code := runCLI(context.Background(), []string{"--example"}, strings.NewReader("PRIVATE_INPUT"), &example, &note); code != 0 {
		t.Fatal(code)
	}
	if !strings.Contains(note.String(), "SYNTHETIC_NOT_A_LIVE_PRICE") || strings.Contains(example.String(), "PRIVATE") {
		t.Fatal("missing example disclaimer or unexpected input read")
	}
	var output, stderr bytes.Buffer
	if code := runCLI(context.Background(), nil, strings.NewReader(example.String()), &output, &stderr); code != 0 {
		t.Fatal(code, output.String())
	}
	report := decodeReport(t, &output)
	if !report.ReviewReady || report.EvidenceSource != "local_input" || report.Quote.TotalMinor != 22800 ||
		report.Quote.Plan != "go" || report.Quote.Country != "EG" || report.Quote.Currency != "EGP" {
		t.Fatal(report)
	}
	if stderr.Len() != 0 {
		t.Fatal(stderr.String())
	}
}

func TestReviewCLIRejectsUnknownSecretFieldsWithoutEcho(t *testing.T) {
	raw := requestJSON(t)
	for _, input := range []string{
		raw[:len(raw)-1] + `,"session":"PRIVATE_SESSION"}`,
		raw[:len(raw)-1] + `,"checkoutURL":"https://PRIVATE_CHECKOUT"}`,
		strings.Replace(raw, `"tax":"28.00"`, `"tax":28.00`, 1),
	} {
		var output, stderr bytes.Buffer
		if code := runCLI(context.Background(), nil, strings.NewReader(input), &output, &stderr); code != 2 {
			t.Fatal(code)
		}
		report := decodeReport(t, &output)
		if report.Code != "QUOTE_INPUT_INVALID" || report.ReviewReady || report.Quote != nil {
			t.Fatal(report)
		}
		if strings.Contains(output.String()+stderr.String(), "PRIVATE") {
			t.Fatal("input leak")
		}
	}
}

func TestReviewCLIRejectsIncorrectTaxAndMissingTax(t *testing.T) {
	raw := requestJSON(t)
	for _, fixture := range []struct{ input, code string }{
		{strings.Replace(raw, `"tax":"28.00"`, `"tax":"27.00"`, 1), "QUOTE_TOTAL_MISMATCH"},
		{strings.Replace(raw, `"tax":"28.00"`, `"tax":null`, 1), "QUOTE_INCOMPLETE"},
	} {
		var output, stderr bytes.Buffer
		if code := runCLI(context.Background(), nil, strings.NewReader(fixture.input), &output, &stderr); code != 2 {
			t.Fatal(code)
		}
		if report := decodeReport(t, &output); report.Code != fixture.code || report.ReviewReady {
			t.Fatal(report)
		}
	}
}

func TestReviewCLIBoundsInputAndMasksArguments(t *testing.T) {
	for _, fixture := range []struct {
		arguments   []string
		input, code string
	}{
		{nil, strings.Repeat(" ", checkout.MaxQuoteInputBytes+1), "QUOTE_INPUT_TOO_LARGE"},
		{[]string{"--session=PRIVATE_ARGUMENT"}, "", "QUOTE_ARGUMENTS_INVALID"},
		{[]string{"PRIVATE_POSITIONAL"}, "", "QUOTE_ARGUMENTS_INVALID"},
	} {
		var output, stderr bytes.Buffer
		if code := runCLI(context.Background(), fixture.arguments, strings.NewReader(fixture.input), &output, &stderr); code != 2 {
			t.Fatal(code)
		}
		if report := decodeReport(t, &output); report.Code != fixture.code {
			t.Fatal(report)
		}
		if strings.Contains(output.String()+stderr.String(), "PRIVATE") {
			t.Fatal("argument leak")
		}
	}
}

func TestReviewCLICancelsOpenStdinPipe(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	reader, writer := io.Pipe()
	defer writer.Close()
	var output, stderr bytes.Buffer
	done := make(chan int, 1)
	go func() { done <- runCLI(ctx, nil, reader, &output, &stderr) }()
	cancel()
	select {
	case code := <-done:
		if code != 2 {
			t.Fatal(code)
		}
		if report := decodeReport(t, &output); report.Code != "QUOTE_CANCELLED" {
			t.Fatal(report)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation did not unblock stdin")
	}
}

type privateFailureReader struct{}

func (privateFailureReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestReviewCLIReadFailureIsSafe(t *testing.T) {
	var output, stderr bytes.Buffer
	if code := runCLI(context.Background(), nil, privateFailureReader{}, &output, &stderr); code != 2 {
		t.Fatal(code)
	}
	if report := decodeReport(t, &output); report.Code != "QUOTE_INPUT_INVALID" {
		t.Fatal(report)
	}
}

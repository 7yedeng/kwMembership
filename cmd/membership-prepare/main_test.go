package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"kwmembership/internal/checkout"
	"kwmembership/internal/preparation"
	"kwmembership/internal/provider"
	"kwmembership/internal/sessionrestore"
)

func rawInput() string {
	return `{"session":{"sessionToken":"synthetic-session-token-never-valid-anywhere-0123456789","user":{"id":"fixture-user","email":"fixture-owner@example.invalid"}},"target":{"plan":"plus","country":"PH","currency":"PHP"},"budget":{"min":"1000.00","max":"1200.00"},"card":{"platformKey":"zovo-fixture","kind":"zovocard","cardId":7,"credential":{"appSecret":"synthetic-api-key"}}}`
}

func TestCheckDefaultsToOfflineWithoutConstructingClients(t *testing.T) {
	var out, diagnostics bytes.Buffer
	called := false
	code := runCLI(context.Background(), nil, strings.NewReader(rawInput()), &out, &diagnostics,
		func(preparation.Options) (checkout.PrepaymentExecutor, error) { called = true; return nil, nil },
		func(provider.CardPlatformConfig) (provider.CardSnapshotReader, error) { called = true; return nil, nil }, time.Now)
	if code != 0 || called || diagnostics.Len() != 0 {
		t.Fatal("default invoked a client", code)
	}
	var result event
	if json.Unmarshal(out.Bytes(), &result) != nil || result.Event != "result" || result.Report.Code != "PREPARATION_INPUT_CHECKED" ||
		result.Report.ReadyForConfirmation || result.Report.PaymentAllowed || result.Report.FundingActionsTaken != 0 {
		t.Fatal("invalid check report")
	}
	for _, secret := range []string{"synthetic-api", "synthetic-session", "fixture-owner"} {
		if strings.Contains(out.String()+diagnostics.String(), secret) {
			t.Fatal("credential leaked")
		}
	}
}

func TestArgumentsCannotEnablePaymentOrEchoSecrets(t *testing.T) {
	for _, args := range [][]string{{"--pay"}, {"--submit=true"}, {"--secret=synthetic-api-key"}, {"--check", "--run"}, {"--check", "--show-browser"}, {"--run", "--chrome-path", "fixture", "--hold-open", "1m"}} {
		var out, diagnostics bytes.Buffer
		code := runCLI(context.Background(), args, strings.NewReader(rawInput()), &out, &diagnostics, nil, nil, time.Now)
		if code != 2 || diagnostics.Len() != 0 || strings.Contains(out.String(), "synthetic-api-key") {
			t.Fatal("unsafe argument handling", code)
		}
	}
}

type fixtureReader struct{ calls int }

func (reader *fixtureReader) GetCardSnapshot(context.Context, int64, time.Time) (provider.CardSnapshot, error) {
	reader.calls++
	return provider.CardSnapshot{UpstreamCardID: 7, CardBindingVerified: true, AvailableAmount: 30,
		Material: provider.CardMaterial{Number: "4242424242424242", CVV: "123", ExpiryMonth: "12", ExpiryYear: "2095", Status: "ACTIVE", HolderName: "Fixture Owner",
			Billing: &provider.BillingAddress{Name: "Fixture Owner", Line1: "1 Fixture Road", City: "Fixture City", Country: "US"}}}, nil
}

type executorStub func(context.Context, checkout.PrepaymentRequest) (checkout.PrepaymentReport, error)

func (fn executorStub) PreparePrepayment(ctx context.Context, request checkout.PrepaymentRequest) (checkout.PrepaymentReport, error) {
	return fn(ctx, request)
}

func TestGUIReadyThenCancelOrEOFCleansUpWithoutConfirmationCommand(t *testing.T) {
	for _, control := range []string{"cancel\n", "pay\n", ""} {
		t.Run(control, func(t *testing.T) {
			inputR, inputW := io.Pipe()
			defer inputR.Close()
			defer inputW.Close()
			outputR, outputW := io.Pipe()
			defer outputR.Close()
			defer outputW.Close()
			reader := &fixtureReader{}
			closed := false
			var diagnostics bytes.Buffer
			done := make(chan int, 1)
			go func() {
				done <- runCLI(context.Background(), []string{"--run", "--chrome-path", "fixture", "--gui-protocol"}, inputR, outputW, &diagnostics,
					func(preparation.Options) (checkout.PrepaymentExecutor, error) {
						return executorStub(func(ctx context.Context, request checkout.PrepaymentRequest) (checkout.PrepaymentReport, error) {
							prepared, err := sessionrestore.Parse(request.Session, time.Now())
							if err != nil {
								return checkout.PrepaymentReport{}, err
							}
							if _, err = request.ResolveBilling(ctx); err != nil {
								return checkout.PrepaymentReport{}, err
							}
							quote := checkout.ReviewedQuote{Plan: "plus", Country: "PH", Currency: "PHP", MinorUnitScale: 2, SubtotalMinor: 98214, TaxMinor: 11786, TotalMinor: 110000, BudgetMinMinor: 100000, BudgetMaxMinor: 120000}
							if _, err = request.ResolveMaterial(ctx, quote); err != nil {
								return checkout.PrepaymentReport{}, err
							}
							report := checkout.PrepaymentReport{Operation: "prepare-payment", Code: "PREPAYMENT_REVIEW_READY", Stage: "review_ready", TargetFingerprint: sessionrestore.Inspect(prepared).TargetFingerprint,
								CookieReadbackVerified: true, IdentityMatch: true, BillingVerified: true, FieldsVerified: true, ReadyForConfirmation: true, BrowserOpened: true, SubmissionFenceVerified: true, Quote: &quote, EvidenceSource: "visible_dom", CountrySource: "requested_checkout_market"}
							request.OnReady(report)
							<-ctx.Done()
							closed = true
							report.BrowserClosed = true
							report.ReadyForConfirmation = false
							report.Code = "PREPAYMENT_CANCELLED"
							return report, ctx.Err()
						}), nil
					}, func(provider.CardPlatformConfig) (provider.CardSnapshotReader, error) { return reader, nil }, time.Now)
			}()
			if _, err := io.WriteString(inputW, rawInput()+"\n"); err != nil {
				t.Fatal(err)
			}
			output := bufio.NewReader(outputR)
			line, err := output.ReadBytes('\n')
			if err != nil {
				t.Fatal(err)
			}
			var ready event
			if json.Unmarshal(line, &ready) != nil || ready.Event != "ready" || !ready.Report.ReadyForConfirmation || ready.Report.BrowserClosed || ready.Report.PaymentAllowed {
				t.Fatal("ready event missing")
			}
			if control == "" {
				_ = inputW.Close()
			} else if _, err := io.WriteString(inputW, control); err != nil {
				t.Fatal(err)
			}
			line, err = output.ReadBytes('\n')
			if err != nil {
				t.Fatal(err)
			}
			var result event
			if json.Unmarshal(line, &result) != nil || result.Event != "result" || result.Report.Code != "PREPAYMENT_CANCELLED" || result.Report.ReadyForConfirmation || !result.Report.BrowserClosed || result.Report.SubmitActivated {
				t.Fatal("cancel became confirmation")
			}
			select {
			case code := <-done:
				if code != 1 || !closed || reader.calls != 1 {
					t.Fatal(code, closed, reader.calls)
				}
			case <-time.After(time.Second):
				t.Fatal("cancellation did not finish")
			}
		})
	}
}

func TestCancelledInputReadDoesNotWaitForCredentials(t *testing.T) {
	reader, writer := io.Pipe()
	defer writer.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := readInput(ctx, reader, false, cancel); done <- err }()
	cancel()
	select {
	case err := <-done:
		if preparation.SafeCode(err) != "PREPAYMENT_CANCELLED" {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("input cancellation blocked")
	}
}

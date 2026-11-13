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
	"kwmembership/internal/preflight"
)

const syntheticInput = `{"targetTier":"plus","expectedEmail":"buyer@example.invalid","session":{"sessionToken":"PRIVATE-TOKEN","user":{"email":"buyer@example.invalid"},"expires":"2099-01-01T00:00:00Z"},"priceContract":{"id":"fixture","version":1,"tier":"plus","currency":"PHP","minAmount":100,"maxAmount":200}}`

func cliNow() time.Time { return time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC) }

func noBrowserFactory(t *testing.T) executorFactory {
	t.Helper()
	return func(preflight.BrowserOptions) (checkout.Executor, error) {
		t.Fatal("local-only command constructed a browser executor")
		return nil, nil
	}
}

func TestDefaultCLIWorksWithoutWorkerEnvironmentOrBrowser(t *testing.T) {
	t.Setenv("KAWANG_PROJECT_ROOT", "/PRIVATE-NONEXISTENT-PROJECT")
	t.Setenv("JWT_SECRET", "replace-with-a-long-random-string")
	t.Setenv("KWMEMBERSHIP_EXECUTOR_SECRET", "")
	var stdout, stderr bytes.Buffer
	status := runCLI(context.Background(), nil, strings.NewReader(syntheticInput), &stdout, &stderr, noBrowserFactory(t), cliNow)
	report := decodeReport(t, stdout.String())
	if status != 0 || report.Operation != "check" || !report.InputValid || report.RemoteVerified || report.PaymentAllowed || stderr.Len() != 0 {
		t.Fatalf("default command: status=%d report=%+v", status, report)
	}
	assertSafeOutput(t, stdout.String()+stderr.String())
}

func TestInspectSessionCLIDoesNotValidateRemoteOrConstructBrowser(t *testing.T) {
	var stdout, stderr bytes.Buffer
	status := runCLI(context.Background(), []string{"--inspect-session"}, strings.NewReader(`{"accessToken":"PRIVATE-TOKEN","user":{"email":"buyer@example.invalid"}}`), &stdout, &stderr, noBrowserFactory(t), cliNow)
	report := decodeReport(t, stdout.String())
	if status != 0 || report.RemoteVerified || report.InputValid || report.SessionShape == nil || report.SessionShape.Fields["sessionToken"].Present {
		t.Fatalf("inspection overclaimed account validity: status=%d report=%+v", status, report)
	}
	assertSafeOutput(t, stdout.String()+stderr.String())
}

func TestCLIRejectsUnknownArgumentsAndTrailingJSONWithoutEcho(t *testing.T) {
	for _, arguments := range [][]string{
		{"--session=PRIVATE-TOKEN"}, {"--timeout=PRIVATE-INVALID-DURATION"},
		{"--check", "--run"}, {"PRIVATE-POSITIONAL"},
	} {
		var stdout, stderr bytes.Buffer
		status := runCLI(context.Background(), arguments, strings.NewReader(syntheticInput), &stdout, &stderr, noBrowserFactory(t), cliNow)
		if status != 2 || decodeReport(t, stdout.String()).Code != "PREFLIGHT_ARGUMENTS_INVALID" {
			t.Fatal("invalid arguments accepted")
		}
		assertSafeOutput(t, stdout.String()+stderr.String())
	}
	var stdout, stderr bytes.Buffer
	status := runCLI(context.Background(), []string{"--check"}, strings.NewReader(syntheticInput+` {"private":"PRIVATE-TAIL"}`), &stdout, &stderr, noBrowserFactory(t), cliNow)
	if status != 2 || decodeReport(t, stdout.String()).Code != "PREFLIGHT_INPUT_INVALID" {
		t.Fatal("trailing JSON accepted")
	}
	assertSafeOutput(t, stdout.String()+stderr.String())
}

type cliExecutor func(context.Context, checkout.Request) (checkout.Result, error)

func (f cliExecutor) Execute(ctx context.Context, request checkout.Request) (checkout.Result, error) {
	return f(ctx, request)
}

func TestRunCLIUsesExplicitFactoryAndNeverRequestsPaymentMaterial(t *testing.T) {
	called := false
	factory := func(options preflight.BrowserOptions) (checkout.Executor, error) {
		if options.ChromePath != "synthetic-browser" || options.Timeout != 90*time.Second {
			t.Fatal("explicit run configuration was lost")
		}
		return cliExecutor(func(_ context.Context, request checkout.Request) (checkout.Result, error) {
			called = true
			if request.Mode != checkout.ModeSessionPreflight || request.Material != nil || request.Guard != nil || request.TargetTier != "plus" {
				t.Fatal("CLI admitted a payment request")
			}
			amount := 150.0
			return checkout.Result{Page: checkout.PageFacts{StateID: "PAYMENT_FINAL_READY", Origin: "https://chatgpt.com", RouteTemplate: "/checkout/{id}", Plan: "plus", Country: "PH", Currency: "PHP", DisplayedAmount: &amount}}, nil
		}), nil
	}
	var stdout, stderr bytes.Buffer
	status := runCLI(context.Background(), []string{"--run", "--chrome-path", "synthetic-browser"}, strings.NewReader(syntheticInput), &stdout, &stderr, factory, cliNow)
	report := decodeReport(t, stdout.String())
	if status != 0 || !called || !report.PreflightPassed || !report.RemoteVerified || report.PaymentAllowed {
		t.Fatalf("fake preflight did not finish safely: status=%d report=%+v", status, report)
	}
	assertSafeOutput(t, stdout.String()+stderr.String())
}

func TestRunConfigurationFailureDoesNotEchoPath(t *testing.T) {
	var stdout, stderr bytes.Buffer
	status := runCLI(context.Background(), []string{"--run", "--chrome-path", "/PRIVATE-NONEXISTENT-CHROME"}, strings.NewReader(syntheticInput), &stdout, &stderr, preflight.NewBrowserExecutor, cliNow)
	if status != 2 || decodeReport(t, stdout.String()).Code != "PREFLIGHT_BROWSER_CONFIG_INVALID" {
		t.Fatal("invalid explicit browser path was not rejected")
	}
	assertSafeOutput(t, stdout.String()+stderr.String())
}

func TestInteractiveCLIForcesFreshHeadedTokenlessPreflight(t *testing.T) {
	const metadata = `{"expectedEmail":"buyer@example.invalid","targetTier":"plus","priceContract":{"id":"fixture","version":1,"tier":"plus","currency":"PHP","minAmount":100,"maxAmount":200}}`
	factory := func(options preflight.BrowserOptions) (checkout.Executor, error) {
		if !options.Headed {
			t.Fatal("interactive CLI did not require a headed browser")
		}
		return cliExecutor(func(ctx context.Context, request checkout.Request) (checkout.Result, error) {
			if request.Mode != checkout.ModeInteractivePreflight || request.OnHandoff == nil || len(request.Session) != 0 || len(request.Cookies) != 0 ||
				request.Material != nil || request.Guard != nil || request.Binding != (checkout.ExecutionBinding{}) {
				t.Fatal("interactive CLI acquired tokens or payment capabilities")
			}
			if err := request.OnHandoff(ctx, checkout.Handoff{Type: "PRIVATE", Page: checkout.PageFacts{Origin: "https://PRIVATE.invalid"}}); err != nil {
				return checkout.Result{}, err
			}
			amount := 150.0
			return checkout.Result{Page: checkout.PageFacts{StateID: "PAYMENT_FINAL_READY", Origin: "https://chatgpt.com", RouteTemplate: "/checkout/{id}", Plan: "plus", Country: "PH", Currency: "PHP", DisplayedAmount: &amount}}, nil
		}), nil
	}
	var stdout, stderr bytes.Buffer
	status := runCLI(context.Background(), []string{"--interactive-preflight", "--headed=false", "--chrome-path", "synthetic-browser"}, strings.NewReader(metadata), &stdout, &stderr, factory, cliNow)
	report := decodeReport(t, stdout.String())
	if status != 0 || report.Operation != "interactive-preflight" || !report.PreflightPassed || report.PaymentAllowed || stderr.String() != "INTERACTIVE_LOGIN_REQUIRED\n" {
		t.Fatalf("interactive CLI boundary failed: status=%d report=%+v", status, report)
	}
	assertSafeOutput(t, stdout.String()+stderr.String())
	stdout.Reset()
	stderr.Reset()
	status = runCLI(context.Background(), []string{"--interactive-preflight"}, strings.NewReader(syntheticInput), &stdout, &stderr, noBrowserFactory(t), cliNow)
	if status != 2 || decodeReport(t, stdout.String()).Code != "PREFLIGHT_INPUT_INVALID" {
		t.Fatal("interactive CLI accepted a Session-bearing request")
	}
}

func TestCLICancellationExitsWithStdinPipeOpen(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	ctx, cancel := context.WithCancel(context.Background())
	var stdout, stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- runCLI(ctx, nil, reader, &stdout, &stderr, noBrowserFactory(t), cliNow)
	}()
	cancel()
	select {
	case status := <-done:
		if status != 2 || decodeReport(t, stdout.String()).Code != "PREFLIGHT_CANCELLED" {
			t.Fatal("cancelled CLI did not return a safe cancellation result")
		}
	case <-time.After(time.Second):
		t.Fatal("CLI blocked on unfinished stdin after cancellation")
	}
	assertSafeOutput(t, stdout.String()+stderr.String())
}

func decodeReport(t *testing.T, output string) preflight.Report {
	t.Helper()
	var report preflight.Report
	if err := json.Unmarshal([]byte(output), &report); err != nil {
		t.Fatal("CLI output was not a JSON report")
	}
	return report
}

func assertSafeOutput(t *testing.T, output string) {
	t.Helper()
	if strings.Contains(output, "PRIVATE") || strings.Contains(output, "buyer@example.invalid") {
		t.Fatal("CLI output exposed a private argument or Session value")
	}
}

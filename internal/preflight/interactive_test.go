package preflight

import (
	"context"
	"errors"
	"strings"
	"testing"

	"kwmembership/internal/checkout"
)

const interactiveMetadata = `{"expectedEmail":"buyer@example.invalid","targetTier":"plus","priceContract":{"id":"synthetic-contract","version":1,"tier":"plus","currency":"PHP","minAmount":100,"maxAmount":200}}`

func TestInteractiveMetadataRejectsCredentialsAndPaymentFields(t *testing.T) {
	for _, field := range []string{
		`"session":{"sessionToken":"PRIVATE"}`, `"sessionToken":"PRIVATE"`,
		`"accessToken":"PRIVATE"`, `"card":{"number":"PRIVATE"}`, `"cookies":[]`,
		`"checkoutURL":"https://PRIVATE.invalid"`, `"profilePath":"PRIVATE"`,
	} {
		raw := strings.TrimSuffix(interactiveMetadata, "}") + "," + field + "}"
		if _, err := DecodeInteractive(strings.NewReader(raw)); SafeCode(err) != "PREFLIGHT_INPUT_INVALID" {
			t.Fatal("interactive metadata accepted credentials or payment configuration")
		}
	}
	if _, err := DecodeInteractive(strings.NewReader(strings.Replace(interactiveMetadata, `"targetTier":"plus"`, `"targetTier":"x5"`, 1))); SafeCode(err) != "PREFLIGHT_TARGET_UNSUPPORTED" {
		t.Fatal("interactive mode accepted an upgrade target")
	}
	if _, err := DecodeInteractive(strings.NewReader(interactiveMetadata + " {}")); SafeCode(err) != "PREFLIGHT_INPUT_INVALID" {
		t.Fatal("interactive metadata accepted trailing JSON")
	}
	if _, err := Decode(strings.NewReader(interactiveMetadata), testNow); err == nil {
		t.Fatal("normal Session check silently accepted interactive metadata")
	}
}

func TestInteractivePreflightHasNoTokensMaterialGuardOrWorkflowBinding(t *testing.T) {
	prepared, err := DecodeInteractive(strings.NewReader(interactiveMetadata))
	if err != nil {
		t.Fatal(err)
	}
	handoffs := 0
	executor := executorFunc(func(ctx context.Context, request checkout.Request) (checkout.Result, error) {
		if request.Mode != checkout.ModeInteractivePreflight || request.Stage != "plus" || request.TargetTier != "plus" ||
			len(request.Session) != 0 || len(request.Cookies) != 0 || request.Material != nil || request.Guard != nil ||
			request.OnHandoff == nil || request.CheckoutURL != "" || request.PlanURL != "" || request.Binding != (checkout.ExecutionBinding{}) {
			t.Fatal("interactive request contains credentials, payment capabilities or a workflow binding")
		}
		if err := request.OnHandoff(ctx, checkout.Handoff{Type: "PRIVATE-TYPE", Page: checkout.PageFacts{Origin: "https://PRIVATE.invalid"}}); err != nil {
			return checkout.Result{}, err
		}
		return checkout.Result{Page: testPage()}, nil
	})
	report, err := RunInteractive(context.Background(), prepared, executor, func() error { handoffs++; return nil })
	if err != nil || handoffs != 1 || report.Operation != "interactive-preflight" || !report.PreflightPassed || !report.RemoteVerified || report.PaymentAllowed {
		t.Fatalf("interactive preflight did not finish within its boundary: report=%+v err=%v", report, err)
	}
	assertReportSanitized(t, report)
	if _, err := Run(context.Background(), prepared, executor); SafeCode(err) != "PREFLIGHT_INPUT_INVALID" {
		t.Fatal("Session execution entry accepted an interactive request")
	}
}

func TestInteractiveHandoffFailureDoesNotExposeWriterError(t *testing.T) {
	prepared, err := DecodeInteractive(strings.NewReader(interactiveMetadata))
	if err != nil {
		t.Fatal(err)
	}
	_, err = RunInteractive(context.Background(), prepared, executorFunc(func(ctx context.Context, request checkout.Request) (checkout.Result, error) {
		return checkout.Result{}, request.OnHandoff(ctx, checkout.Handoff{})
	}), func() error { return errors.New("PRIVATE-WRITER-DESTINATION") })
	if SafeCode(err) != "INTERACTIVE_LOGIN_HANDOFF_FAILED" || err.Error() != "INTERACTIVE_LOGIN_HANDOFF_FAILED" {
		t.Fatalf("interactive handoff error was not sanitized: %v", err)
	}
}

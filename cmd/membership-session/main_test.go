package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"kwmembership/internal/provider"
	"kwmembership/internal/sessionrestore"
)

func rawSession() string {
	return `{"sessionToken":"synthetic-only-session-token-not-valid-anywhere-0123456789","user":{"id":"synthetic-id","email":"owner@example.invalid"}}`
}

func TestInspectNeverConstructsBrowserAndRedacts(t *testing.T) {
	var out, diagnostics bytes.Buffer
	called := false
	factory := func(context.Context, sessionrestore.Options) (sessionrestore.Browser, error) {
		called = true
		return nil, nil
	}
	code := runCLI(context.Background(), nil, strings.NewReader(rawSession()), &out, &diagnostics, factory, time.Now)
	if code != 0 || called || diagnostics.Len() != 0 {
		t.Fatal("inspection started browser or failed")
	}
	if strings.Contains(out.String(), "owner@example.invalid") || strings.Contains(out.String(), "synthetic-only") {
		t.Fatal("secret leaked")
	}
	var report sessionrestore.Report
	if json.Unmarshal(out.Bytes(), &report) != nil || report.RemoteVerified || report.PaymentAllowed || report.Code != "SESSION_INSPECTED" {
		t.Fatal(out.String())
	}
}

func TestArgumentErrorsNeverEchoArgument(t *testing.T) {
	var out, diagnostics bytes.Buffer
	code := runCLI(context.Background(), []string{"--secret=do-not-print-me"}, strings.NewReader(""), &out, &diagnostics, nil, time.Now)
	if code != 2 || diagnostics.Len() != 0 || strings.Contains(out.String(), "do-not-print") {
		t.Fatal("unsafe argument error")
	}
}

type cancellationBrowser struct {
	closed  bool
	cookies []sessionrestore.Cookie
	started chan struct{}
}

func (b *cancellationBrowser) WriteCookies(_ context.Context, cookies []provider.SessionCookie) error {
	for _, c := range cookies {
		b.cookies = append(b.cookies, sessionrestore.Cookie{Name: c.Name, Value: c.Value, Domain: c.Domain, Path: c.Path, Secure: true, HTTPOnly: true, SameSite: "lax", Session: true})
	}
	return nil
}
func (b *cancellationBrowser) ReadCookies(context.Context) ([]sessionrestore.Cookie, error) {
	return b.cookies, nil
}
func (b *cancellationBrowser) Navigate(context.Context) error { return nil }
func (b *cancellationBrowser) Observe(ctx context.Context) (sessionrestore.Observation, error) {
	close(b.started)
	<-ctx.Done()
	return sessionrestore.Observation{}, ctx.Err()
}
func (b *cancellationBrowser) Close() error { b.closed = true; return nil }

func TestGUIProtocolCancelsWithoutKillingBrowserProcess(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	browser := &cancellationBrowser{started: make(chan struct{})}
	var out, diagnostic bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- runCLI(context.Background(), []string{"--restore", "--chrome-path", "synthetic", "--gui-protocol"}, reader, &out, &diagnostic,
			func(context.Context, sessionrestore.Options) (sessionrestore.Browser, error) { return browser, nil }, time.Now)
	}()
	if _, err := io.WriteString(writer, rawSession()+"\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-browser.started:
	case <-time.After(time.Second):
		t.Fatal("browser observation did not start")
	}
	if _, err := io.WriteString(writer, "cancel\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-done:
		if code != 1 || !browser.closed || !strings.Contains(out.String(), "SESSION_CANCELLED") {
			t.Fatal(code, browser.closed, out.String())
		}
	case <-time.After(time.Second):
		t.Fatal("GUI cancellation did not finish")
	}
}

func TestCancelDuringStdinRead(t *testing.T) {
	reader, writer := io.Pipe()
	defer writer.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := readInput(ctx, reader, false, cancel); done <- err }()
	cancel()
	select {
	case err := <-done:
		if sessionrestore.SafeCode(err) != "SESSION_CANCELLED" {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("stdin cancellation blocked")
	}
}

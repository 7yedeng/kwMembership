package checkout

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
)

// This is a local mock CDP transport, not a browser or a connection to a site.
// It exercises chromedp's real asynchronous malformed-message diagnostic path.
func exerciseMalformedCDPMessage(t *testing.T, logging ChromeLogging) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connection, _, _, err := ws.UpgradeHTTP(r, w)
		if err != nil {
			t.Error("mock CDP handshake failed")
			return
		}
		defer connection.Close()
		// Wait for a client request so the upgrade response cannot coalesce
		// with the mock event in the websocket dialer's handshake buffer.
		_ = connection.SetReadDeadline(time.Now().Add(time.Second))
		if _, _, err := wsutil.ReadClientData(connection); err != nil {
			t.Error("mock CDP request read failed")
			return
		}
		if err := wsutil.WriteServerText(connection, []byte(`{"params":{"secret":"SYNTHETIC_CDP_MARKER"}}`)); err != nil {
			t.Error("mock CDP event write failed")
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	allocator, cancelAllocator := chromedp.NewRemoteAllocator(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/devtools/browser/synthetic", chromedp.NoModifyURL)
	defer cancelAllocator()
	executor := NewChromeExecutorWithLogging("unused", "", false, time.Second, time.Second, logging)
	browser, cancelBrowser := executor.newBrowserContext(allocator)
	defer cancelBrowser()
	_ = chromedp.Run(browser)
}

func TestChromeContextRoutesAsyncDiagnosticsThroughExplicitCallbacks(t *testing.T) {
	var errorsSeen, debugSeen atomic.Int32
	exerciseMalformedCDPMessage(t, ChromeLogging{
		Logf:   func(string, ...any) {},
		Errorf: func(string, ...any) { errorsSeen.Add(1) },
		Debugf: func(string, ...any) { debugSeen.Add(1) },
	})
	if errorsSeen.Load() == 0 || debugSeen.Load() == 0 {
		t.Fatal("browser context did not apply explicit asynchronous log callbacks")
	}
}

func TestQuietChromeContextDoesNotLeakAsyncCDPDiagnostics(t *testing.T) {
	const helperEnv = "KWMEMBERSHIP_QUIET_CDP_TEST_HELPER"
	if os.Getenv(helperEnv) == "1" {
		exerciseMalformedCDPMessage(t, ChromeLogging{})
		return
	}
	// Capture an isolated test process's streams; do not redirect the product's
	// global os.Stderr or process-wide logger to make the test pass.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestQuietChromeContextDoesNotLeakAsyncCDPDiagnostics$")
	command.Env = append(os.Environ(), helperEnv+"=1")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatal("isolated quiet-browser diagnostic test failed")
	}
	for _, forbidden := range [][]byte{[]byte("SYNTHETIC_CDP_MARKER"), []byte("malformed incoming message"), []byte("ERROR:")} {
		if bytes.Contains(output, forbidden) {
			t.Fatal("raw asynchronous CDP diagnostic escaped the explicit quiet policy")
		}
	}
}

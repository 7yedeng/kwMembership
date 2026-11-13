package preflight

import (
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"kwmembership/internal/checkout"
)

// BrowserOptions is independent of the shared worker's config.Load/.env rules.
// Headed means a private display such as Xvfb. The selected request mode, not
// this display option, determines whether an interactive handoff is allowed.
type BrowserOptions struct {
	ChromePath  string
	ProxyServer string
	Headed      bool
	Timeout     time.Duration
}

// NewBrowserExecutor validates local configuration but does not launch Chrome.
// The caller must explicitly enter Run or RunInteractive before network
// activity can begin. Every execution uses a new temporary browser profile.
func NewBrowserExecutor(options BrowserOptions) (checkout.Executor, error) {
	if options.Timeout < time.Second || options.Timeout > 5*time.Minute || strings.TrimSpace(options.ChromePath) == "" {
		return nil, fail("PREFLIGHT_BROWSER_CONFIG_INVALID")
	}
	path, err := filepath.Abs(options.ChromePath)
	if err != nil {
		return nil, fail("PREFLIGHT_BROWSER_CONFIG_INVALID")
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
		return nil, fail("PREFLIGHT_BROWSER_CONFIG_INVALID")
	}
	if options.ProxyServer != "" {
		proxy, err := url.Parse(options.ProxyServer)
		if err != nil {
			return nil, fail("PREFLIGHT_BROWSER_CONFIG_INVALID")
		}
		port, portErr := strconv.Atoi(proxy.Port())
		if (proxy.Scheme != "http" && proxy.Scheme != "socks5") || proxy.Hostname() == "" ||
			portErr != nil || port < 1 || port > 65535 || proxy.User != nil || proxy.Path != "" ||
			proxy.RawQuery != "" || proxy.ForceQuery || proxy.Fragment != "" || strings.Contains(options.ProxyServer, "#") {
			return nil, fail("PREFLIGHT_BROWSER_CONFIG_INVALID")
		}
	}
	// Browser CDP diagnostics may contain raw events, URLs or credentials.
	// Disable every asynchronous log channel for this diagnostic CLI only;
	// returned errors still pass through its explicit SafeCode allowlist.
	return checkout.NewChromeExecutorWithLogging(path, options.ProxyServer, options.Headed, options.Timeout, 5*time.Minute, checkout.ChromeLogging{}), nil
}

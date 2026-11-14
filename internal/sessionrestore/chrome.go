package sessionrestore

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/network"
	cdpruntime "github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/cdproto/storage"
	"github.com/chromedp/chromedp"

	"kwmembership/internal/provider"
)

const loginOrigin = "https://chatgpt.com"

type chromeBrowser struct {
	*chromeSession
	cancelBrowser   context.CancelFunc
	cancelAllocator context.CancelFunc
	profile         string
	once            sync.Once
	closeErr        error
}

// chromeSession deliberately has no Close method. A facade returned to a task
// can perform authentication actions without acquiring browser ownership.
type chromeSession struct{ ctx context.Context }

// UseChromeContext wraps a Chrome target already started by the calling task.
// It never launches/attaches a browser, opens a debugging port, or changes its
// lifecycle. Requiring an initialized target avoids starting the allocator
// under a short-lived authentication action context.
func UseChromeContext(ctx context.Context) (SessionBrowser, error) {
	if ctx == nil {
		return nil, failure("SESSION_BROWSER_CONFIG_INVALID")
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	owned := chromedp.FromContext(ctx)
	if owned == nil || owned.Browser == nil || owned.Target == nil {
		return nil, failure("SESSION_BROWSER_CONFIG_INVALID")
	}
	return &chromeSession{ctx: ctx}, nil
}

// NewChrome creates only a fresh, disposable profile. It has no attach/CDP URL,
// cookie-export, proxy-switch or sandbox-disabling option.
func NewChrome(parent context.Context, options Options) (Browser, error) {
	if options.Validate() != nil || strings.TrimSpace(options.ChromePath) == "" {
		return nil, failure("SESSION_BROWSER_CONFIG_INVALID")
	}
	path, err := filepath.Abs(options.ChromePath)
	if err != nil {
		return nil, failure("SESSION_BROWSER_CONFIG_INVALID")
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || runtime.GOOS != "windows" && info.Mode()&0o111 == 0 {
		return nil, failure("SESSION_BROWSER_CONFIG_INVALID")
	}
	profile, err := os.MkdirTemp("", "membership-session-")
	if err != nil {
		return nil, failure("SESSION_BROWSER_UNAVAILABLE")
	}
	allocatorOptions := []chromedp.ExecAllocatorOption{
		chromedp.ExecPath(path), chromedp.UserDataDir(profile), chromedp.NoFirstRun,
		chromedp.NoDefaultBrowserCheck, chromedp.Flag("headless", !options.ShowBrowser),
		chromedp.Flag("disable-extensions", true), chromedp.Flag("disable-sync", true),
		chromedp.Flag("disable-background-networking", true), chromedp.WindowSize(1200, 850),
		chromedp.Flag("no-sandbox", false),
	}
	allocator, cancelAllocator := chromedp.NewExecAllocator(parent, allocatorOptions...)
	quiet := func(string, ...any) {}
	browserContext, cancelBrowser := chromedp.NewContext(allocator,
		chromedp.WithLogf(quiet), chromedp.WithErrorf(quiet), chromedp.WithDebugf(quiet))
	browser := &chromeBrowser{chromeSession: &chromeSession{ctx: browserContext}, cancelBrowser: cancelBrowser,
		cancelAllocator: cancelAllocator, profile: profile}
	if err = startPersistentBrowser(browserContext, options.Timeout, cancelBrowser, func(ctx context.Context) error {
		return chromedp.Run(ctx, network.Enable(), chromedp.Navigate("about:blank"))
	}); err != nil {
		_ = browser.Close()
		return nil, failure("SESSION_BROWSER_UNAVAILABLE")
	}
	return browser, nil
}

// The first chromedp.Run passes its context to the process allocator. Starting
// through action's short-lived child would kill Chrome when that action ends.
// Keep the allocator on the owned browser context; only an actual startup
// timeout cancels it. Later actions may safely have shorter lifetimes.
func startPersistentBrowser(ctx context.Context, timeout time.Duration, abort context.CancelFunc, start func(context.Context) error) error {
	timer := time.AfterFunc(timeout, abort)
	err := start(ctx)
	stopped := timer.Stop()
	if err != nil {
		return err
	}
	if !stopped {
		return context.DeadlineExceeded
	}
	return ctx.Err()
}

// action uses the caller's cancellation/deadline while retaining the allocated
// browser's chromedp context values.
func (browser *chromeSession) action(ctx context.Context, actions ...chromedp.Action) error {
	operation, cancel := context.WithCancel(browser.ctx)
	stop := context.AfterFunc(ctx, cancel)
	defer func() { stop(); cancel() }()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return chromedp.Run(operation, actions...)
}

func (browser *chromeSession) WriteCookies(ctx context.Context, cookies []provider.SessionCookie) error {
	return browser.action(ctx, chromedp.ActionFunc(func(target context.Context) error {
		for _, cookie := range cookies {
			if err := network.SetCookie(cookie.Name, cookie.Value).WithURL(loginOrigin + "/").
				WithDomain(cookie.Domain).WithPath(cookie.Path).WithSecure(true).WithHTTPOnly(true).
				WithSameSite(network.CookieSameSiteLax).Do(target); err != nil {
				return err
			}
		}
		return nil
	}))
}

func sessionCookieName(name string) bool {
	const base = "__Secure-next-auth.session-token"
	return name == base || strings.HasPrefix(name, base+".")
}

func (browser *chromeSession) ReadCookies(ctx context.Context) ([]Cookie, error) {
	var cookies []Cookie
	err := browser.action(ctx, chromedp.ActionFunc(func(target context.Context) error {
		all, err := storage.GetCookies().Do(target)
		if err != nil {
			return err
		}
		for _, cookie := range all {
			if strings.TrimPrefix(cookie.Domain, ".") != "chatgpt.com" || !sessionCookieName(cookie.Name) {
				continue
			}
			cookies = append(cookies, cookieFact(cookie))
		}
		return nil
	}))
	return cookies, err
}

func cookieFact(cookie *network.Cookie) Cookie {
	return Cookie{Name: cookie.Name, Value: cookie.Value, Domain: cookie.Domain,
		Path: cookie.Path, Secure: cookie.Secure, HTTPOnly: cookie.HTTPOnly,
		SameSite: strings.ToLower(string(cookie.SameSite)), Session: cookie.Session,
		Partitioned: cookie.PartitionKey != nil || cookie.PartitionKeyOpaque}
}

func (browser *chromeSession) Navigate(ctx context.Context) error {
	return browser.action(ctx, chromedp.Navigate(loginOrigin+"/"))
}

// The complete API response remains in the page. Only bounded identity facts
// cross CDP; tokens, email-bearing URLs and arbitrary error text never do.
const observeIdentityJS = `(async () => {
  if (location.origin !== 'https://chatgpt.com') return {code:'SESSION_ORIGIN_MISMATCH'};
  try {
    const response = await fetch('/api/auth/session', {credentials:'same-origin',cache:'no-store',redirect:'error'});
    if (response.status === 401) return {code:'SESSION_REJECTED'};
    if (response.status === 403) return {code:'SESSION_SITE_DENIED'};
    if (!response.ok) return {code:'SESSION_IDENTITY_UNAVAILABLE'};
    const payload = await response.json();
    if (!payload || typeof payload !== 'object' || Array.isArray(payload)) return {code:'SESSION_IDENTITY_UNAVAILABLE'};
    if (payload.error) return {code:'SESSION_REJECTED'};
    if (payload.expires != null) {
      if (typeof payload.expires !== 'string' || !Number.isFinite(Date.parse(payload.expires))) return {code:'SESSION_IDENTITY_UNAVAILABLE'};
      if (Date.parse(payload.expires) <= Date.now()) return {code:'SESSION_EXPIRED'};
    }
    const user = payload.user;
    if (!user || typeof user !== 'object') return {code:'SESSION_NOT_LOGGED_IN'};
    const email = typeof user.email === 'string' && user.email.length <= 254 ? user.email : '';
    const userID = typeof user.id === 'string' && user.id.length <= 512 ? user.id : '';
    return {email, userID};
  } catch (_) { return {code:'SESSION_IDENTITY_UNAVAILABLE'}; }
})()`

func (browser *chromeSession) Observe(ctx context.Context) (Observation, error) {
	var observed struct {
		Code   string `json:"code"`
		UserID string `json:"userID"`
		Email  string `json:"email"`
	}
	err := browser.action(ctx, chromedp.Evaluate(observeIdentityJS, &observed,
		func(params *cdpruntime.EvaluateParams) *cdpruntime.EvaluateParams {
			return params.WithAwaitPromise(true)
		}))
	return Observation{Code: observed.Code, UserID: observed.UserID, Email: observed.Email}, err
}

func (browser *chromeBrowser) Close() error {
	browser.once.Do(func() {
		browser.cancelBrowser()
		browser.cancelAllocator()
		if err := os.RemoveAll(browser.profile); err != nil {
			browser.closeErr = failure("SESSION_PROFILE_CLEANUP_FAILED")
		}
	})
	return browser.closeErr
}

package checkout

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chromedp/cdproto/storage"
	"github.com/chromedp/chromedp"

	"kwmembership/internal/sessionrestore"
)

// These runtime substitutions exist only in a _test.go file. The browser visits
// actual loopback URLs, never a spoofed or proxied ChatGPT/Stripe hostname.
type fixturePrepaymentChrome struct {
	*chromePrepaymentDriver
	mainURL, cardURL, foreignURL string
	resetOnFinalVerification     bool
	verifyCount                  int
	closedSnapshot               struct {
		Click, Submit, RequestSubmit int
		ForeignEmpty                 bool
		TaxReady                     bool
		StorageWasEmpty              bool
	}
	fixtureErr error
	oopifCount int
}

func (fixture *fixturePrepaymentChrome) localScript(script string) string {
	return strings.NewReplacer("https://chatgpt.com", fixture.mainURL, "https://pay.openai.com", fixture.mainURL, "https://js.stripe.com", fixture.cardURL).Replace(script)
}

// The local session facade below delegates real Cookie writes/readbacks, while
// replacing only navigation/identity observation with a loopback account page.
type localSessionBrowser struct {
	sessionrestore.SessionBrowser
	fixture *fixturePrepaymentChrome
}

func (session localSessionBrowser) Navigate(ctx context.Context) error {
	return session.fixture.withContext(ctx, func(target context.Context) error {
		return chromedp.Run(target, chromedp.Navigate(session.fixture.mainURL+"/account"))
	})
}
func (session localSessionBrowser) Observe(ctx context.Context) (sessionrestore.Observation, error) {
	var value sessionrestore.Observation
	err := session.fixture.withContext(ctx, func(target context.Context) error {
		return chromedp.Run(target, chromedp.Evaluate(`(async()=>{const r=await fetch('/identity');return await r.json()})()`, &value, authenticatedIdentityEvalOptions...))
	})
	return value, err
}
func (fixture *fixturePrepaymentChrome) Restore(ctx context.Context, prepared sessionrestore.Prepared) (sessionrestore.Report, error) {
	return sessionrestore.Apply(ctx, prepared, localSessionBrowser{SessionBrowser: fixture.sessionBrowser, fixture: fixture}, nil)
}
func (fixture *fixturePrepaymentChrome) OpenPlus(ctx context.Context, identity sessionrestore.Identity) error {
	expression, err := bindPlusPreparation(identity)
	if err != nil {
		return err
	}
	return fixture.withContext(ctx, func(target context.Context) error {
		var entry plusCheckoutEntry
		if err := chromedp.Run(target, chromedp.Evaluate(fixture.localScript(expression), &entry, authenticatedIdentityEvalOptions...)); err != nil {
			return err
		}
		if entry.ErrorKind != "" || entry.ResponseTag != "hosted_checkout_session" || entry.CheckoutURL != fixture.mainURL+"/checkout" {
			return prepaymentFail("PREPAYMENT_CHECKOUT_CONTRACT_CHANGED")
		}
		return chromedp.Run(target, chromedp.Navigate(entry.CheckoutURL))
	})
}
func (fixture *fixturePrepaymentChrome) observeOnceLocal(ctx context.Context) (prepaymentObservation, error) {
	state := fixture.SubmissionState()
	if state.Blocked {
		return prepaymentObservation{}, prepaymentFail("PREPAYMENT_SUBMISSION_BLOCKED")
	}
	if state.Failed {
		return prepaymentObservation{}, prepaymentFail("PREPAYMENT_FENCE_UNAVAILABLE")
	}
	if !state.Ready {
		return prepaymentObservation{}, prepaymentFail("PREPAYMENT_FENCE_PENDING")
	}
	var observed prepaymentObservation
	err := fixture.withContext(ctx, func(target context.Context) error {
		var location string
		var dom prepaymentDOM
		if err := chromedp.Run(target, chromedp.Location(&location), chromedp.Evaluate(fixture.localScript(observePrepaymentDOMJS), &dom)); err != nil {
			return err
		}
		if location != fixture.mainURL+"/checkout" || dom.Origin != fixture.mainURL {
			return prepaymentFail("PREPAYMENT_PAGE_UNSUPPORTED")
		}
		dom.Origin = "https://pay.openai.com"
		frames, err := fixture.inspectPrepaymentFramesWithScript(target, fixture.localScript(inspectFrameJS))
		if err != nil {
			return err
		}
		for index := range frames {
			switch frames[index].Origin {
			case fixture.mainURL:
				frames[index].Origin = "https://pay.openai.com"
			case fixture.cardURL:
				frames[index].Origin = "https://js.stripe.com"
			}
		}
		observed, err = observationFromDOM(dom, frames, "https://pay.openai.com", "/pay/{id}")
		return err
	})
	return observed, err
}
func (fixture *fixturePrepaymentChrome) Observe(ctx context.Context) (prepaymentObservation, error) {
	return waitPrepaymentObservation(ctx, 15*time.Second, func() bool { return fixture.billingApplied }, fixture.observeOnceLocal)
}
func (fixture *fixturePrepaymentChrome) FillBilling(ctx context.Context, page prepaymentObservation, billing BillingAddress) error {
	return fixture.withContext(ctx, func(target context.Context) error {
		filled, err := processPrepaymentFields(target, page.Page, Material{Billing: billing}, fixture.localScript(fillBillingFrameFunction), true)
		if err != nil {
			return err
		}
		if !filled["billingName"] || !filled["billingCountry"] {
			return prepaymentFail("PREPAYMENT_FIELDS_NOT_VERIFIED")
		}
		fixture.billingApplied = true
		return nil
	})
}
func (fixture *fixturePrepaymentChrome) Fill(ctx context.Context, page prepaymentObservation, material Material) error {
	return fixture.withContext(ctx, func(target context.Context) error {
		filled, err := processPrepaymentFields(target, page.Page, material, fixture.localScript(fillFrameFunction), false)
		if err != nil {
			return err
		}
		return validateFilledMaterial(material, filled)
	})
}
func (fixture *fixturePrepaymentChrome) Verify(ctx context.Context, page prepaymentObservation, material Material) error {
	fixture.verifyCount++
	return fixture.withContext(ctx, func(target context.Context) error {
		if fixture.resetOnFinalVerification && fixture.verifyCount == 2 {
			for _, frame := range page.Page.fieldFrames {
				if frame.Fields["cvc"] {
					var reset bool
					if err := evaluatePrepaymentFrame(target, frame, `(()=>{document.querySelector('input[name="cvc"]').value='';return true})()`, &reset, false); err != nil {
						return err
					}
				}
			}
		}
		filled, err := processPrepaymentFields(target, page.Page, material, fixture.localScript(verifyFrameFunction), false)
		if err != nil {
			return err
		}
		return validateFilledMaterial(material, filled)
	})
}
func (fixture *fixturePrepaymentChrome) Close() error {
	fixture.fence.mu.Lock()
	for _, entry := range fixture.fence.targets {
		if entry.iframe && entry.ready {
			fixture.oopifCount++
		}
	}
	fixture.fence.mu.Unlock()
	if fixture.ctx.Err() == nil {
		fixture.fixtureErr = chromedp.Run(fixture.ctx, chromedp.Evaluate(`(()=>({...window.fixtureCounters,TaxReady:window.fixtureTaxReady,StorageWasEmpty:window.fixtureStorageWasEmpty,ForeignEmpty:window.fixtureForeignEmpty}))()`, &fixture.closedSnapshot))
	}
	return fixture.chromePrepaymentDriver.Close()
}

func TestPrepaymentChromeRuntimeLocal(t *testing.T) {
	path := os.Getenv("KWMEMBERSHIP_CHROME_RUNTIME_PATH")
	if path == "" {
		if os.Getenv("KWMEMBERSHIP_REQUIRE_PREPAYMENT_RUNTIME_TESTS") == "1" {
			t.Fatal("full Chrome runtime path is required")
		}
		t.Skip("prepayment browser runtime not configured")
	}
	if runtime.GOOS == "linux" && (os.Getuid() == 0 || os.Geteuid() == 0 || os.Getenv("DISPLAY") == "") {
		t.Fatal("prepayment runtime requires non-root full Chrome and an isolated display")
	}
	var checkoutCreates atomic.Int32
	var selectedCycles atomic.Int32
	var unexpected atomic.Int32
	var mainServer, cardServer, foreignServer *httptest.Server
	var variant atomic.Int32
	writeHTML := func(w http.ResponseWriter, html string) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'; frame-src "+cardServer.URL+" "+foreignServer.URL+"; form-action 'none'")
		fmt.Fprint(w, html)
	}
	foreignServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/foreign" {
			unexpected.Add(1)
			http.Error(w, "not found", 404)
			return
		}
		writeHTML(w, `<!doctype html><input name="number"><input name="cvc"><script>setInterval(()=>parent.postMessage({fixtureForeignEmpty:[...document.querySelectorAll('input')].every(e=>e.value==='')},`+strconvQuote(mainServer.URL)+`),100)</script>`)
	}))
	defer foreignServer.Close()
	cardServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/card" {
			unexpected.Add(1)
			http.Error(w, "not found", 404)
			return
		}
		cycle := variant.Load()
		expiry := `<input name="expiry" autocomplete="cc-exp" required>`
		if cycle == 2 {
			expiry = `<select name="exp-month" required><option value=""></option><option value="12">12</option></select><select name="exp-year" required><option value=""></option><option value="2035">2035</option></select>`
		}
		writeHTML(w, `<!doctype html><style>input,select{display:block;height:25px}</style><input name="number" autocomplete="cc-number" required>`+expiry+`<input name="cvc" autocomplete="cc-csc" required><script>
const variant=`+fmt.Sprint(cycle)+`;const notify=()=>parent.postMessage({fixtureCardReady:[...document.querySelectorAll('input,select')].every(e=>e.value&&e.validity.valid)},`+strconvQuote(mainServer.URL)+`);
document.addEventListener('input',e=>{if(variant===1&&e.target.name==='number')e.target.value=e.target.value.replace(/\D/g,'').replace(/(.{4})/g,'$1 ').trim();if(variant===5&&e.target.name==='number')fetch('/v1/payment_intents/pi_fixture/confirm',{method:'POST',body:'synthetic'}).catch(()=>{});notify()});document.addEventListener('change',notify);
</script>`)
	}))
	defer cardServer.Close()
	// localhost and 127.0.0.1 are different sites while both are loopback.
	// Chrome's normal site isolation must produce an actual iframe target.
	cardServer.URL = strings.Replace(cardServer.URL, "127.0.0.1", "localhost", 1)
	mainServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && r.URL.Path == "/account":
			writeHTML(w, `<!doctype html><title>Local fixture account</title>`)
		case r.Method == "GET" && r.URL.Path == "/identity":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"UserID":"synthetic-user","Email":"fixture@example.invalid"}`)
		case r.Method == "GET" && r.URL.Path == "/api/auth/session":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"user":{"id":"synthetic-user","email":"fixture@example.invalid"},"account":{"id":"fixture-account"},"accessToken":"fixture-access-token"}`)
		case r.Method == "GET" && r.URL.Path == "/backend-api/subscriptions":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"plan_type":"free"}`)
		case r.Method == "POST" && r.URL.Path == "/backend-api/payments/checkout":
			var body struct {
				Plan    string                             `json:"plan_name"`
				Billing struct{ Country, Currency string } `json:"billing_details"`
			}
			if json.NewDecoder(r.Body).Decode(&body) != nil || body.Plan != "chatgptplusplan" || body.Billing.Country != "PH" || body.Billing.Currency != "PHP" {
				unexpected.Add(1)
				http.Error(w, "bad fixture contract", 400)
				return
			}
			checkoutCreates.Add(1)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"tag": "hosted_checkout_session", "url": mainServer.URL + "/checkout"})
		case r.Method == "GET" && r.URL.Path == "/checkout":
			writeHTML(w, prepaymentCheckoutFixture(cardServer.URL, foreignServer.URL, int(variant.Load())))
		case r.Method == "GET" && r.URL.Path == "/favicon.ico":
			w.WriteHeader(http.StatusNoContent)
		default:
			unexpected.Add(1)
			http.Error(w, "not found", 404)
		}
	}))
	defer mainServer.Close()

	for cycle := 0; cycle <= 5; cycle++ {
		variant.Store(int32(cycle))
		t.Run(fmt.Sprintf("fresh_profile_%d", cycle), func(t *testing.T) {
			selectedCycles.Add(1)
			request, _ := prepaymentFixture(t)
			request.Material.Billing.PostalCode = "10001"
			var driver *fixturePrepaymentChrome
			var diagnosticMu sync.Mutex
			var diagnostics []string
			t.Cleanup(func() {
				diagnosticMu.Lock()
				defer diagnosticMu.Unlock()
				for _, diagnostic := range diagnostics {
					t.Log(diagnostic)
				}
			})
			executor := NewChromeExecutorWithLogging(path, "http://127.0.0.1:1", true, 35*time.Second, 0, ChromeLogging{})
			factory := func(ctx context.Context) (prepaymentDriver, error) {
				owned, err := executor.newPrepaymentDriver(ctx)
				if err != nil {
					return nil, err
				}
				native := owned.(*chromePrepaymentDriver)
				native.fence.mu.Lock()
				native.fence.diagnostic = func(phase string, code int64, classification string) {
					diagnosticMu.Lock()
					diagnostics = append(diagnostics, fmt.Sprintf("loopback fence: phase=%s code=%d class=%s", phase, code, classification))
					diagnosticMu.Unlock()
				}
				native.fence.mu.Unlock()
				var count int
				if err := chromedp.Run(native.ctx, chromedp.ActionFunc(func(target context.Context) error {
					cookies, err := storage.GetCookies().Do(target)
					count = len(cookies)
					return err
				})); err != nil || count != 0 {
					owned.Close()
					t.Fatal("fresh profile retained cookies")
				}
				driver = &fixturePrepaymentChrome{chromePrepaymentDriver: native, mainURL: mainServer.URL, cardURL: cardServer.URL, foreignURL: foreignServer.URL, resetOnFinalVerification: cycle == 3}
				return driver, nil
			}
			report, err := runPrepayment(context.Background(), request, 35*time.Second, factory)
			if cycle < 3 {
				if err != nil || !report.ReadyForConfirmation || report.Quote == nil || report.Quote.TotalMinor != 112000 {
					t.Fatalf("native prepayment failed: %v %+v", err, report)
				}
			} else if cycle == 3 && (err == nil || report.Code != "PREPAYMENT_FIELDS_NOT_VERIFIED" || report.ReadyForConfirmation) {
				t.Fatalf("reset card field was silently repaired: %v %+v", err, report)
			} else if cycle > 3 && (err == nil || report.Code != "PREPAYMENT_SUBMISSION_BLOCKED" || !report.SubmissionBlocked || report.ReadyForConfirmation) {
				t.Fatalf("automatic submission was not blocked: %v %+v", err, report)
			}
			if driver == nil || driver.fixtureErr != nil || !report.BrowserClosed || report.PaymentAllowed || report.SubmitActivated {
				t.Fatalf("native lifecycle failed: %+v", report)
			}
			snapshot := driver.closedSnapshot
			if snapshot.Click != 0 || snapshot.Submit != 0 || snapshot.RequestSubmit != 0 || !snapshot.ForeignEmpty || cycle != 4 && !snapshot.TaxReady || !snapshot.StorageWasEmpty || driver.oopifCount < 1 {
				t.Fatalf("unsafe page side effect: %+v", snapshot)
			}
			if _, err := os.Stat(driver.profile); !os.IsNotExist(err) {
				t.Fatal("temporary prepayment profile was not removed")
			}
		})
	}
	if checkoutCreates.Load() != selectedCycles.Load() || unexpected.Load() != 0 {
		t.Fatalf("fixture requests: checkout=%d unexpected=%d", checkoutCreates.Load(), unexpected.Load())
	}
}

func strconvQuote(value string) string { encoded, _ := json.Marshal(value); return string(encoded) }

func prepaymentCheckoutFixture(cardURL, foreignURL string, variant int) string {
	return `<!doctype html><html><head><style>body{font:16px sans-serif}form{display:block}label{display:block}input,select{height:24px}iframe{width:450px;height:140px}.row{display:flex;gap:24px}</style></head><body>
<h1>Subscribe to ChatGPT Plus</h1><div class="row"><span>Subtotal</span><span>PHP 1,000.00</span></div><div class="row"><span>Tax (exclusive)</span><span id="tax">Calculating</span></div><div class="row"><span>Discount</span><span>PHP 0.00</span></div><div class="row"><span>Total due today</span><span id="total">Calculating</span></div>
<div hidden><span>Total</span><span>PHP 999,999.00</span></div><div data-kw-plan="go" data-kw-amount="1"></div>
<form><label>Name<input name="name" required></label><label>Address<input name="addressLine1" required></label><label>City<input name="city" required></label><label>State<input autocomplete="billing address-level1"></label><label>Postal code<input name="postalCode" required></label><label>Country<select name="country" required><option value=""></option><option value="US">United States</option></select></label><iframe src="` + cardURL + `/card"></iframe><iframe src="` + foreignURL + `/foreign"></iframe><button type="submit" disabled>Subscribe</button></form>
<script>
window.fixtureCounters={Click:0,Submit:0,RequestSubmit:0};window.fixtureTaxReady=false;window.fixtureForeignEmpty=false;
window.fixtureStorageWasEmpty=localStorage.getItem('prior-task')===null&&sessionStorage.getItem('prior-task')===null&&!document.cookie;
localStorage.setItem('prior-task','old');sessionStorage.setItem('prior-task','old');document.cookie='prior_task=old;path=/';
let cardReady=false,billing='',timer;const button=document.querySelector('button');
const refresh=()=>{button.disabled=!(cardReady&&window.fixtureTaxReady&&[...document.querySelectorAll('input[required],select[required]')].every(e=>e.value&&e.validity.valid))};
const bill=()=>{const next=JSON.stringify([...document.querySelectorAll('input,select')].map(e=>e.value));if(next===billing)return;billing=next;clearTimeout(timer);window.fixtureTaxReady=false;document.querySelector('#tax').textContent='Calculating';document.querySelector('#total').textContent='Calculating';refresh();timer=setTimeout(()=>{if([...document.querySelectorAll('input[required],select[required]')].every(e=>e.value)){document.querySelector('#tax').textContent='PHP 120.00';document.querySelector('#total').textContent='PHP 1,120.00';window.fixtureTaxReady=true;refresh()}},1400)};
document.addEventListener('input',event=>{if(` + fmt.Sprint(variant) + `===4)document.querySelector('form').requestSubmit();bill()});document.addEventListener('change',bill);
window.addEventListener('message',event=>{if(event.origin===` + strconvQuote(cardURL) + `&&typeof event.data?.fixtureCardReady==='boolean'){cardReady=event.data.fixtureCardReady;refresh()}if(event.origin===` + strconvQuote(foreignURL) + `&&typeof event.data?.fixtureForeignEmpty==='boolean')window.fixtureForeignEmpty=event.data.fixtureForeignEmpty});
button.addEventListener('click',()=>window.fixtureCounters.Click++);document.querySelector('form').addEventListener('submit',event=>{window.fixtureCounters.Submit++;event.preventDefault()});HTMLFormElement.prototype.requestSubmit=function(){window.fixtureCounters.RequestSubmit++};
</script></body></html>`
}

"""Fail-closed Playwright adapter for the Go-owned checkout protocol.

This module deliberately has no retry or business-state transitions.  It only
turns a leased command into browser observations and permit-bound clicks.
"""

from __future__ import annotations

import hashlib
import json
import os
import shutil
import tempfile
import time
from datetime import datetime
from typing import Any
from urllib.parse import urlsplit

from .client import ExecutorAPIError, ExecutorClient, ExecutorLease


CHATGPT_ORIGIN = "https://chatgpt.com"
PAY_ORIGIN = "https://pay.openai.com"
SESSION_COOKIE = "__Secure-next-auth.session-token"
SESSION_CHUNK_SIZE = 3936
MAX_TRANSITIONS = 6
POLL_SECONDS = 0.25
HEARTBEAT_SECONDS = 5.0

INSPECT_FRAME_JS = r"""
() => {
  const allowed = new Set(['https://chatgpt.com', 'https://pay.openai.com', 'https://js.stripe.com']);
  const origin = allowed.has(location.origin) ? location.origin : null;
  const path = String(location.pathname || '');
  let route = null;
  const safeID = value => /^[A-Za-z0-9_-]{1,200}$/.test(value);
  const match = (prefix) => {
    const trimmed = path.replace(/\/$/, '');
    const value = trimmed.startsWith(prefix) ? trimmed.slice(prefix.length) : '';
    return safeID(value) ? '/{id}' : null;
  };
  if (origin === 'https://chatgpt.com') {
    if (path === '/checkout' || path === '/checkout/') route = '/checkout';
    else if (/^\/checkout\/openai_llc\/(?:oaics_|cs_)[A-Za-z0-9_-]+\/?$/.test(path) || /^\/checkout\/[A-Za-z0-9_-]+\/?$/.test(path)) route = '/checkout/{id}';
    else if (path === '/settings/subscription' || path === '/settings/subscription/') route = '/settings/subscription';
    else if (path === '/settings/billing' || path === '/settings/billing/') route = '/settings/billing';
    else if (path === '/account/billing/overview' || path === '/account/billing/overview/') route = '/account/billing/overview';
  } else if (origin === 'https://pay.openai.com') {
    if (/^\/checkout\/[A-Za-z0-9_-]+\/?$/.test(path)) route = '/checkout/{id}';
    else if (/^\/(?:c\/)?pay\/[A-Za-z0-9_-]+\/?$/.test(path)) route = '/pay/{id}';
  }
  const find = selectors => selectors.map(selector => document.querySelector(selector)).find(Boolean) || null;
  const attr = (selectors, names) => {
    const element = find(selectors); if (!element) return null;
    for (const name of names) { const value = element.getAttribute(name); if (value && value.trim()) return value.trim(); }
    return null;
  };
  const has = selectors => Boolean(find(selectors));
  const control = entries => { for (const [id, selectors] of entries) if (has(selectors)) return id; return null; };
  const planRaw = attr(['[data-kw-plan]', '[data-testid="plan-name"][data-plan]', 'meta[name="openai-plan"]'], ['data-kw-plan', 'data-plan', 'content']);
  const countryRaw = attr(['[data-kw-country]', '[data-testid="checkout-price"][data-country]', 'meta[name="openai-country"]'], ['data-kw-country', 'data-country', 'content']);
  const currencyRaw = attr(['[data-kw-currency]', '[data-testid="checkout-price"][data-currency]', 'meta[name="openai-currency"]'], ['data-kw-currency', 'data-currency', 'content']);
  const amountRaw = attr(['[data-kw-amount]', '[data-testid="checkout-price"][data-amount]', 'meta[name="openai-amount"]'], ['data-kw-amount', 'data-amount', 'content']);
  const stateRaw = attr(['[data-kw-checkout-state]', '[data-testid="checkout-state"][data-state]'], ['data-kw-checkout-state', 'data-state']);
  const amount = amountRaw !== null && /^\d+(?:\.\d{1,2})?$/.test(amountRaw) ? Number(amountRaw) : null;
  return {
    origin, routeTemplate: route,
    plan: ['plus', 'prolite', 'pro'].includes(planRaw) ? planRaw : null,
    country: countryRaw === 'PH' ? 'PH' : null,
    currency: currencyRaw === 'PHP' ? 'PHP' : null,
    displayedAmount: Number.isFinite(amount) && amount > 0 ? amount : null,
    stateMarker: ['card-entry', 'billing-entry', 'review', 'upgrade-selection', 'challenge', 'complete'].includes(stateRaw) ? stateRaw : null,
    fields: {
      cardNumber: has(['#payment-numberInput', 'input[name="number"]', 'input[autocomplete="cc-number"]']),
      expiry: has(['#payment-expiryInput', 'input[name="expiry"]', 'input[autocomplete="cc-exp"]']),
      expiryMonth: has(['#payment-expiryMonthInput', 'select[name="exp-month"]']),
      expiryYear: has(['#payment-expiryYearInput', 'select[name="exp-year"]']),
      cvc: has(['#payment-cvcInput', 'input[name="cvc"]', 'input[autocomplete="cc-csc"]']),
      billingName: has(['#billingAddress-nameInput', 'input[name="name"]', 'input[autocomplete="billing name"]']),
      billingLine1: has(['#billingAddress-line1Input', 'input[name="addressLine1"]', 'input[autocomplete="billing address-line1"]']),
      billingCity: has(['#billingAddress-cityInput', 'input[name="city"]', 'input[autocomplete="billing address-level2"]']),
      billingState: has(['#billingAddress-stateInput', 'select[name="state"]', 'input[autocomplete="billing address-level1"]']),
      billingCountry: has(['#billingAddress-countryInput', 'select[name="country"]', 'select[autocomplete="billing country"]']),
      billingPostal: has(['#billingAddress-postalCodeInput', 'input[name="postalCode"]', 'input[autocomplete="billing postal-code"]'])
    },
    controls: {
      progression: control([['payment-next', ['#payment-next', '[data-testid="checkout-next"]']], ['hosted-payment-next', ['[data-testid="hosted-payment-next-button"]', 'button[name="continue-payment"]']]]),
      submit: control([['payment-submit', ['#payment-submit', '[data-testid="checkout-submit"]']], ['hosted-payment-submit', ['[data-testid="hosted-payment-submit-button"]', 'button[name="submit-payment"]']]]),
      upgradeX5: control([['upgrade-x5', ['[data-testid="upgrade-to-prolite"]', 'button[data-plan="prolite"][data-action="upgrade"]']]]),
      upgradeX20: control([['upgrade-x20', ['[data-testid="upgrade-to-pro"]', 'button[data-plan="pro"][data-action="upgrade"]']]]),
      challenge: control([
        ['challenge-cloudflare', ['#challenge-form', '#challenge-running', 'input[name="cf-turnstile-response"]', 'iframe[src*="challenges.cloudflare.com"]', 'script[src*="/cdn-cgi/challenge-platform/"]']],
        ['challenge-3ds', ['[data-testid="three-d-secure-challenge"]', 'iframe[title="3D Secure authentication"]']],
        ['challenge-captcha', ['[data-testid="captcha-challenge"]']],
        ['challenge-sms', ['[data-testid="sms-verification"]']],
        ['challenge-bank', ['[data-testid="bank-verification"]']]
      ])
    }
  };
}
"""

FILL_FRAME_JS = r"""
(fragment) => {
  if (!['https://chatgpt.com', 'https://pay.openai.com', 'https://js.stripe.com'].includes(location.origin)) return {accepted:false,filled:[]};
  const selectors = {
    cardNumber: ['#payment-numberInput', 'input[name="number"]', 'input[autocomplete="cc-number"]'], expiry: ['#payment-expiryInput', 'input[name="expiry"]', 'input[autocomplete="cc-exp"]'],
    expiryMonth: ['#payment-expiryMonthInput', 'select[name="exp-month"]'], expiryYear: ['#payment-expiryYearInput', 'select[name="exp-year"]'], cvc: ['#payment-cvcInput', 'input[name="cvc"]', 'input[autocomplete="cc-csc"]'],
    billingName: ['#billingAddress-nameInput', 'input[name="name"]', 'input[autocomplete="billing name"]'], billingLine1: ['#billingAddress-line1Input', 'input[name="addressLine1"]', 'input[autocomplete="billing address-line1"]'],
    billingCity: ['#billingAddress-cityInput', 'input[name="city"]', 'input[autocomplete="billing address-level2"]'], billingState: ['#billingAddress-stateInput', 'select[name="state"]', 'input[autocomplete="billing address-level1"]'],
    billingCountry: ['#billingAddress-countryInput', 'select[name="country"]', 'select[autocomplete="billing country"]'], billingPostal: ['#billingAddress-postalCodeInput', 'input[name="postalCode"]', 'input[autocomplete="billing postal-code"]']
  };
  const find = key => (selectors[key] || []).map(selector => document.querySelector(selector)).find(Boolean) || null;
  const filled = [];
  for (const [key, value] of Object.entries(fragment || {})) {
    if (typeof value !== 'string') continue;
    const element = find(key); if (!element) continue;
    const prototype = element instanceof HTMLSelectElement ? HTMLSelectElement.prototype : HTMLInputElement.prototype;
    const setter = Object.getOwnPropertyDescriptor(prototype, 'value')?.set;
    if (setter) setter.call(element, value); else element.value = value;
    for (const type of ['input', 'change', 'blur']) element.dispatchEvent(new Event(type, {bubbles:true}));
    filled.push(key);
  }
  return {accepted:true,filled};
}
"""

CONTROL_SELECTORS = {
    "payment-next": ("#payment-next", '[data-testid="checkout-next"]'),
    "hosted-payment-next": ('[data-testid="hosted-payment-next-button"]', 'button[name="continue-payment"]'),
    "payment-submit": ("#payment-submit", '[data-testid="checkout-submit"]'),
    "hosted-payment-submit": ('[data-testid="hosted-payment-submit-button"]', 'button[name="submit-payment"]'),
    "upgrade-x5": ('[data-testid="upgrade-to-prolite"]', 'button[data-plan="prolite"][data-action="upgrade"]'),
    "upgrade-x20": ('[data-testid="upgrade-to-pro"]', 'button[data-plan="pro"][data-action="upgrade"]'),
}

PREPARE_PLUS_JS = r"""
async () => {
  const result = (overrides = {}) => ({responseTag:'',checkoutURL:'',processorEntity:'',checkoutSessionID:'',errorKind:'',httpStatus:0,...overrides});
  if (location.origin !== 'https://chatgpt.com') return result({errorKind:'context_invalid'});
  const controller = new AbortController(); const abortTimer = setTimeout(() => controller.abort(), 12000);
  try {
    const sessionResponse = await fetch('/api/auth/session', {method:'GET', credentials:'include', cache:'no-store', redirect:'error', signal:controller.signal});
    if (!sessionResponse.ok) return result({errorKind:'session_unavailable',httpStatus:sessionResponse.status});
    const session = await sessionResponse.json(); const accessToken = typeof session?.accessToken === 'string' ? session.accessToken : '';
    if (!accessToken) return result({errorKind:'access_token_unavailable'});
    let claims = {};
    try { const encoded = accessToken.split('.')[1] || ''; claims = JSON.parse(atob(encoded.replace(/-/g, '+').replace(/_/g, '/'))); } catch {}
    const authClaims = claims?.['https://api.openai.com/auth'];
    const candidates = [session?.account?.id, session?.account?.account_id, session?.accountId, session?.account_id, session?.user?.accountId, session?.user?.account_id, claims?.chatgpt_account_id, claims?.account_id, authClaims?.chatgpt_account_id, authClaims?.account_id];
    const accountID = candidates.find(value => typeof value === 'string' && value.length > 0) || '';
    const headers = {authorization:'Bearer '+accessToken};
    if (accountID) {
      const subscriptionResponse = await fetch('/backend-api/subscriptions?account_id='+encodeURIComponent(accountID), {method:'GET',credentials:'include',cache:'no-store',redirect:'error',headers,signal:controller.signal});
      if (subscriptionResponse.ok) {
        const subscription = await subscriptionResponse.json();
        const plan = typeof subscription?.plan_type === 'string' ? subscription.plan_type.toLowerCase() : '';
        const activeUntil = Date.parse(typeof subscription?.active_until === 'string' ? subscription.active_until : '');
        if (['plus','prolite','pro','team','business','enterprise'].includes(plan) && Number.isFinite(activeUntil) && activeUntil > Date.now() && subscription?.is_delinquent !== true) return result({errorKind:'already_subscribed'});
      }
    }
    const response = await fetch('/backend-api/payments/checkout', {method:'POST', credentials:'include', cache:'no-store', redirect:'error', signal:controller.signal, headers:{...headers,'content-type':'application/json'}, body:JSON.stringify({billing_details:{country:'PH',currency:'PHP'},checkout_ui_mode:'hosted',entry_point:'all_plans_pricing_modal',plan_name:'chatgptplusplan'})});
    if (!response.ok) return result({errorKind:response.status===401||response.status===403?'checkout_unauthorized':'checkout_rejected',httpStatus:response.status});
    const payload = await response.json(); return result({responseTag:typeof payload?.tag==='string'?payload.tag:'',checkoutURL:typeof payload?.url==='string'?payload.url:'',processorEntity:typeof payload?.processor_entity==='string'?payload.processor_entity:'',checkoutSessionID:typeof payload?.checkout_session_id==='string'?payload.checkout_session_id:''});
  } catch { return result({errorKind:'checkout_unavailable'}); } finally { clearTimeout(abortTimer); }
}
"""


def normalize_email(value: Any) -> str:
    value = str(value or "").strip().lower()
    if len(value) > 254 or value.count("@") != 1 or value.startswith("@") or value.endswith("@"):
        return ""
    return value


def session_cookies(session: dict[str, Any], now: float | None = None) -> list[dict[str, Any]]:
    token = session.get("sessionToken")
    if not isinstance(token, str) or not token or token.strip() != token or len(token) > 65536:
        raise ExecutorAPIError("SESSION_COOKIE_MISSING", 409)
    expires = session.get("expires")
    expiry = None
    if expires:
        try:
            expiry = datetime.fromisoformat(str(expires).replace("Z", "+00:00")).timestamp()
        except ValueError as error:
            raise ExecutorAPIError("SESSION_INVALID", 409) from error
        if now is not None and expiry <= now:
            raise ExecutorAPIError("CHATGPT_SESSION_UNAUTHORIZED", 409)
    chunks = [token[index : index + SESSION_CHUNK_SIZE] for index in range(0, len(token), SESSION_CHUNK_SIZE)]
    return [
        {"name": SESSION_COOKIE if len(chunks) == 1 else f"{SESSION_COOKIE}.{index}", "value": value,
         "domain": ".chatgpt.com", "path": "/", "secure": True, "httpOnly": True, "sameSite": "Lax",
         **({"expires": expiry} if expiry is not None else {})}
        for index, value in enumerate(chunks)
    ]


def validate_checkout_url(value: str) -> str:
    parsed = urlsplit(value)
    if parsed.scheme != "https" or parsed.username or parsed.fragment:
        raise ExecutorAPIError("CHECKOUT_URL_INVALID", 409)
    origin = f"{parsed.scheme}://{parsed.netloc}"
    path = parsed.path.rstrip("/")
    if origin == CHATGPT_ORIGIN and (path == "/checkout" or path == "/settings/subscription"):
        return value
    if origin == CHATGPT_ORIGIN and path.startswith("/checkout/"):
        route_id = path.removeprefix("/checkout/")
        if route_id.startswith("openai_llc/"):
            route_id = route_id.removeprefix("openai_llc/")
            if not (route_id.startswith("oaics_") or route_id.startswith("cs_")):
                route_id = ""
        if route_id and all(char.isalnum() or char in "_-" for char in route_id):
            return value
    if origin == PAY_ORIGIN and (path.startswith("/checkout/") or path.startswith("/pay/") or path.startswith("/c/pay/")):
        route_id = path.split("/")[-1]
        if route_id and all(char.isalnum() or char in "_-" for char in route_id):
            return value
    raise ExecutorAPIError("CHECKOUT_URL_INVALID", 409)


def route_template(value: str) -> str:
    parsed = urlsplit(value)
    origin = f"{parsed.scheme}://{parsed.netloc}"
    path = parsed.path.rstrip("/")
    if origin == CHATGPT_ORIGIN:
        if path == "/checkout": return "/checkout"
        if path.startswith("/checkout/") and path.removeprefix("/checkout/"):
            route_id = path.removeprefix("/checkout/")
            if route_id.startswith("openai_llc/"):
                route_id = route_id.removeprefix("openai_llc/")
                if not (route_id.startswith("oaics_") or route_id.startswith("cs_")):
                    return ""
            if all(char.isalnum() or char in "_-" for char in route_id): return "/checkout/{id}"
        if path in {"/settings/subscription", "/settings/billing", "/account/billing/overview"}: return path
    if origin == PAY_ORIGIN:
        if path.startswith("/checkout/"):
            route_id = path.removeprefix("/checkout/")
            if route_id and all(char.isalnum() or char in "_-" for char in route_id): return "/checkout/{id}"
        if path.startswith("/pay/") or path.startswith("/c/pay/"):
            route_id = path.split("/")[-1]
            if route_id and all(char.isalnum() or char in "_-" for char in route_id): return "/pay/{id}"
    return ""


def structural_hash(page: dict[str, Any]) -> str:
    value = dict(page)
    value["structuralHash"] = ""
    return hashlib.sha256(json.dumps(value, separators=(",", ":"), sort_keys=True).encode()).hexdigest()


class LiveExecutor:
    def execute(self, client: ExecutorClient, lease: ExecutorLease) -> None:
        try:
            from playwright.sync_api import TimeoutError as PlaywrightTimeoutError
            from playwright.sync_api import sync_playwright
        except ImportError as error:
            raise ExecutorAPIError("LIVE_BROWSER_UNAVAILABLE", 503) from error

        material = client.material(lease)
        if lease.adapter_version != "python-session-card-checkout-v1":
            raise ExecutorAPIError("EXECUTOR_ADAPTER_MISMATCH", 409)
        session = material.get("session")
        if not isinstance(session, dict):
            raise ExecutorAPIError("SESSION_INVALID", 409)
        expected = normalize_email(material.get("expectedEmail"))
        actual = normalize_email(session.get("user", {}).get("email") if isinstance(session.get("user"), dict) else "")
        if not actual:
            actual = normalize_email(session.get("email"))
        if not expected or not actual:
            raise ExecutorAPIError("EXPECTED_IDENTITY_MISSING", 409)
        if actual != expected:
            raise ExecutorAPIError("CHATGPT_SESSION_IDENTITY_MISMATCH", 409)
        if lease.command_kind == "payment" and not material.get("card"):
            raise ExecutorAPIError("CHECKOUT_MATERIAL_INVALID", 409)

        deadline = _deadline(lease.hard_deadline_at)
        client.heartbeat(lease)
        user_data_dir = tempfile.mkdtemp(prefix="kwmembership-python-")
        try:
            with sync_playwright() as playwright:
                launch_kwargs: dict[str, Any] = {"headless": os.environ.get("KWMEMBERSHIP_VISIBLE_BROWSER", "false") != "true"}
                executable = os.environ.get("KWMEMBERSHIP_CHROME_PATH", "").strip()
                if executable:
                    launch_kwargs["executable_path"] = executable
                proxy = os.environ.get("KWMEMBERSHIP_CHROME_PROXY_SERVER", "").strip()
                if proxy:
                    launch_kwargs["proxy"] = {"server": proxy}
                context = playwright.chromium.launch_persistent_context(
                    user_data_dir,
                    viewport={"width": 1280, "height": 900},
                    service_workers="block",
                    **launch_kwargs,
                )
                try:
                    context.add_cookies(session_cookies(session, time.time()))
                    page = context.pages[0] if context.pages else context.new_page()
                    client.heartbeat(lease)
                    self._run(client, lease, material, page, deadline)
                finally:
                    context.close()
        except ExecutorAPIError:
            raise
        except PlaywrightTimeoutError as error:
            raise ExecutorAPIError("CHECKOUT_PAGE_TIMEOUT", 409) from error
        except Exception as error:
            raise ExecutorAPIError("HEADLESS_BROWSER_UNAVAILABLE", 503) from error
        finally:
            shutil.rmtree(user_data_dir, ignore_errors=True)

    def _run(self, client: ExecutorClient, lease: ExecutorLease, material: dict[str, Any], page: Any, deadline: float) -> None:
        _navigate(client, lease, page, CHATGPT_ORIGIN + "/", deadline)
        identity = page.evaluate("""async () => { try { const response = await fetch('/api/auth/session', {credentials:'include',cache:'no-store',redirect:'error',signal:AbortSignal.timeout(10000)}); if (!response.ok) return {email:'',errorKind:'present'}; const session = await response.json(); return {email: session?.user?.email || session?.email || '', errorKind: session?.error || ''}; } catch { return {email:'',errorKind:'present'}; } }""")
        expected = normalize_email(material.get("expectedEmail")); actual = normalize_email(identity.get("email"))
        if identity.get("errorKind"): raise ExecutorAPIError("CHATGPT_SESSION_REFRESH_FAILED", 409)
        if not actual: raise ExecutorAPIError("CHATGPT_SESSION_UNAUTHORIZED", 409)
        if actual != expected: raise ExecutorAPIError("CHATGPT_SESSION_IDENTITY_MISMATCH", 409)
        client.heartbeat(lease)

        if lease.command_kind == "preflight":
            entry = page.evaluate(PREPARE_PLUS_JS)
            checkout_url = _resolve_checkout_entry(entry)
            _navigate(client, lease, page, checkout_url, deadline)
            page_facts = self._wait_facts(client, lease, page, deadline, "checkout")
            if page_facts["stateId"] == "PAYMENT_ACTION_REQUIRED":
                self._handoff(client, lease, page_facts)
                page_facts = self._wait_challenge_clear(client, lease, page, deadline, "checkout")
            client.report(lease, "success", page_facts, diagnostic={"phase": "live-preflight", "stateId": page_facts["stateId"], "status": "passed"})
            return

        if lease.stage == "plus":
            entry = page.evaluate(PREPARE_PLUS_JS)
            checkout_url = _resolve_checkout_entry(entry)
            _navigate(client, lease, page, checkout_url, deadline)
        else:
            _navigate(client, lease, page, CHATGPT_ORIGIN + "/settings/subscription", deadline)
            selection = self._wait_facts(client, lease, page, deadline, "selection")
            if selection["stateId"] == "PAYMENT_ACTION_REQUIRED":
                self._handoff(client, lease, selection)
                selection = self._wait_challenge_clear(client, lease, page, deadline, "selection")
            control = "upgradeX5" if lease.target_tier == "x5" else "upgradeX20"
            outcome = self._activate(client, lease, page, selection, "progression", selection["controls"].get(control, ""))
            if not outcome.get("continue", True):
                client.report(lease, "success", selection, diagnostic={"phase": "live", "stateId": selection["stateId"], "status": "stopped"})
                return
            page_facts = self._wait_facts(client, lease, page, deadline, "checkout", selection.get("structuralHash", ""))
            self._checkout(client, lease, material, page, deadline, page_facts)
            return

        self._checkout(client, lease, material, page, deadline)

    def _checkout(self, client: ExecutorClient, lease: ExecutorLease, material: dict[str, Any], page: Any, deadline: float, initial: dict[str, Any] | None = None) -> None:
        seen: set[str] = set()
        pending = initial
        for _ in range(MAX_TRANSITIONS):
            facts = pending or self._wait_facts(client, lease, page, deadline, "checkout")
            pending = None
            if facts["stateId"] == "PAYMENT_ACTION_REQUIRED":
                self._handoff(client, lease, facts)
                facts = self._wait_challenge_clear(client, lease, page, deadline, "checkout")
            key = facts["stateId"] + ":" + facts["structuralHash"]
            if key in seen: raise ExecutorAPIError("PAYMENT_REPEATED_STATE", 409)
            seen.add(key)
            if facts["stateId"] not in {"PAYMENT_PROGRESSION_READY", "PAYMENT_FINAL_READY"}:
                raise ExecutorAPIError("CHECKOUT_UI_UNSUPPORTED", 409)
            _fill_frames(page, material)
            refreshed = self._wait_facts(client, lease, page, deadline, "checkout")
            if refreshed["stateId"] == "PAYMENT_ACTION_REQUIRED":
                self._handoff(client, lease, refreshed)
                pending = self._wait_challenge_clear(client, lease, page, deadline, "checkout")
                continue
            if refreshed["stateId"] == "PAYMENT_PROGRESSION_READY":
                outcome = self._activate(client, lease, page, refreshed, "progression", refreshed["controls"].get("progression", ""))
                if not outcome.get("continue", True):
                    client.report(lease, "success", refreshed, diagnostic={"phase": "live", "stateId": refreshed["stateId"], "status": "stopped"})
                    return
                pending = self._wait_facts(client, lease, page, deadline, "checkout", refreshed["structuralHash"])
                continue
            self._activate(client, lease, page, refreshed, "submit", refreshed["controls"].get("submit", ""))
            # Submit activation is the financial boundary. Return the last
            # validated page immediately; Go owns post-submit reconciliation
            # and will classify any later challenge or unknown outcome.
            client.report(lease, "success", refreshed, diagnostic={"phase": "live", "stateId": refreshed["stateId"], "status": "submitted"})
            return
        raise ExecutorAPIError("PAYMENT_TRANSITION_LIMIT", 409)

    def _wait_facts(self, client: ExecutorClient, lease: ExecutorLease, page: Any, deadline: float, purpose: str, previous_hash: str = "") -> dict[str, Any]:
        last_heartbeat = 0.0
        last: dict[str, Any] | None = None
        while time.time() < deadline:
            if time.monotonic() - last_heartbeat >= HEARTBEAT_SECONDS:
                client.heartbeat(lease); last_heartbeat = time.monotonic()
            raw = _inspect(page, lease, purpose)
            if raw.get("stateId") == "PAYMENT_ACTION_REQUIRED":
                return raw
            if raw.get("origin") in {CHATGPT_ORIGIN, PAY_ORIGIN} and raw.get("routeTemplate"):
                try:
                    validated = client.page_facts(lease, raw)["page"]
                    last = validated
                    if validated["stateId"] != "UNKNOWN_PAYMENT_STATE" and (not previous_hash or validated["structuralHash"] != previous_hash or validated["stateId"] == "PAYMENT_ACTION_REQUIRED"):
                        return validated
                except ExecutorAPIError as error:
                    if error.code not in {"CHECKOUT_PAGE_CONTRACT_INVALID", "CHECKOUT_CONTEXT_INVALID"}:
                        raise
            time.sleep(POLL_SECONDS)
        if last and last.get("stateId") == "PAYMENT_ACTION_REQUIRED":
            raise ExecutorAPIError("SECURITY_CHALLENGE_TIMEOUT", 409)
        raise ExecutorAPIError("CHECKOUT_PAGE_TIMEOUT", 409)

    def _wait_challenge_clear(self, client: ExecutorClient, lease: ExecutorLease, page: Any, deadline: float, purpose: str) -> dict[str, Any]:
        last_heartbeat = 0.0
        while time.time() < deadline:
            if time.monotonic() - last_heartbeat >= HEARTBEAT_SECONDS:
                client.heartbeat(lease)
                last_heartbeat = time.monotonic()
            raw = _inspect(page, lease, purpose)
            if raw.get("stateId") == "PAYMENT_ACTION_REQUIRED":
                time.sleep(POLL_SECONDS)
                continue
            if raw.get("origin") in {CHATGPT_ORIGIN, PAY_ORIGIN} and raw.get("routeTemplate"):
                try:
                    return client.page_facts(lease, raw)["page"]
                except ExecutorAPIError as error:
                    if error.code not in {"CHECKOUT_PAGE_CONTRACT_INVALID", "CHECKOUT_CONTEXT_INVALID"}:
                        raise
            time.sleep(POLL_SECONDS)
        raise ExecutorAPIError("SECURITY_CHALLENGE_TIMEOUT", 409)

    def _activate(self, client: ExecutorClient, lease: ExecutorLease, page: Any, facts: dict[str, Any], kind: str, control_id: str) -> dict[str, Any]:
        if not control_id: raise ExecutorAPIError("CHECKOUT_ACTION_INVALID", 409)
        permit = client.prepare_action(lease, kind, control_id, facts).get("permitId")
        if not isinstance(permit, str) or not permit: raise ExecutorAPIError("CHECKOUT_ACTION_INVALID", 409)
        client.activate_action(lease, permit)
        activated = _activate_frames(page, control_id)
        if not activated:
            client.action_result(lease, permit, "not-clicked")
            raise ExecutorAPIError("PERMIT_ACTIVATION_UNCERTAIN", 409)
        return client.action_result(lease, permit, "clicked")

    def _handoff(self, client: ExecutorClient, lease: ExecutorLease, facts: dict[str, Any]) -> None:
        challenge = facts.get("controls", {}).get("challenge") or "challenge-captcha"
        handoff_type = {"challenge-cloudflare": "cloudflare", "challenge-3ds": "3ds", "challenge-sms": "sms", "challenge-bank": "bank"}.get(challenge, "captcha")
        client.handoff(lease, handoff_type, facts, {"phase": "challenge", "stateId": facts.get("stateId", ""), "status": "action_required"})


def _deadline(value: str) -> float:
    return datetime.fromisoformat(value.replace("Z", "+00:00")).timestamp()


def _remaining_ms(deadline: float) -> int:
    remaining = int((deadline - time.time()) * 1000)
    if remaining <= 0: raise ExecutorAPIError("EXECUTOR_LEASE_EXPIRED", 409)
    return max(1000, remaining)


def _navigate(client: ExecutorClient, lease: ExecutorLease, page: Any, value: str, deadline: float) -> None:
    client.heartbeat(lease)
    page.goto(value, wait_until="commit", timeout=min(15000, _remaining_ms(deadline)))
    client.heartbeat(lease)


def _resolve_checkout_entry(entry: Any) -> str:
    if not isinstance(entry, dict): raise ExecutorAPIError("CHECKOUT_API_CONTRACT_DRIFT", 409)
    error_kind = entry.get("errorKind")
    if error_kind:
        code = {
            "already_subscribed": "CHATGPT_ACCOUNT_ALREADY_SUBSCRIBED",
            "session_unavailable": "CHATGPT_SESSION_REFRESH_FAILED",
            "access_token_unavailable": "CHATGPT_SESSION_REFRESH_FAILED",
            "checkout_unauthorized": "CHECKOUT_API_AUTH_FAILED",
            "context_invalid": "CHECKOUT_CONTEXT_INVALID",
            "checkout_rejected": "CHECKOUT_ENTRY_UNAVAILABLE",
            "checkout_unavailable": "CHECKOUT_ENTRY_UNAVAILABLE",
        }.get(str(error_kind), "CHECKOUT_API_CONTRACT_DRIFT")
        raise ExecutorAPIError(code, 409)
    tag = entry.get("responseTag")
    if tag == "hosted_checkout_session":
        if not isinstance(entry.get("checkoutURL"), str) or entry.get("processorEntity") or entry.get("checkoutSessionID"): raise ExecutorAPIError("CHECKOUT_API_CONTRACT_DRIFT", 409)
        return validate_checkout_url(entry["checkoutURL"])
    if tag == "custom_checkout_session":
        session_id = entry.get("checkoutSessionID", "")
        safe = isinstance(session_id, str) and bool(session_id) and all(char.isalnum() or char in "_-" for char in session_id)
        if entry.get("checkoutURL") or entry.get("processorEntity") != "openai_llc" or not safe or not (session_id.startswith("oaics_") or session_id.startswith("cs_")):
            raise ExecutorAPIError("CHECKOUT_API_CONTRACT_DRIFT", 409)
        return validate_checkout_url(CHATGPT_ORIGIN + "/checkout/openai_llc/" + session_id)
    raise ExecutorAPIError("CHECKOUT_API_CONTRACT_DRIFT", 409)


def _inspect(page: Any, lease: ExecutorLease, purpose: str) -> dict[str, Any]:
    frames = []
    for frame in page.frames:
        try:
            fact = frame.evaluate(INSPECT_FRAME_JS)
        except Exception:
            continue
        if isinstance(fact, dict) and fact.get("origin"):
            frames.append(fact)
    top_origin = f"{urlsplit(page.url).scheme}://{urlsplit(page.url).netloc}"
    top_route = route_template(page.url)
    top = [fact for fact in frames if fact.get("origin") == top_origin]
    page_data: dict[str, Any] = {"origin": top_origin, "routeTemplate": top_route, "plan": _unique(top, "plan"), "country": _unique(top, "country"), "currency": _unique(top, "currency"), "displayedAmount": _unique_amount(top), "stateMarker": _unique(top, "stateMarker"), "fields": {}, "controls": {}}
    for fact in frames:
        for key, value in fact.get("fields", {}).items(): page_data["fields"][key] = page_data["fields"].get(key, False) or bool(value)
    for key in ("progression", "submit", "upgradeX5", "upgradeX20", "challenge"):
        values = [fact.get("controls", {}).get(key) for fact in top if fact.get("controls", {}).get(key)]
        if len(set(values)) == 1: page_data["controls"][key] = values[0]
    page_data["stateId"] = _classify(page_data, lease, purpose)
    page_data["structuralHash"] = structural_hash(page_data)
    return page_data


def _classify(page: dict[str, Any], lease: ExecutorLease, purpose: str) -> str:
    if page.get("controls", {}).get("challenge") == "challenge-cloudflare": return "PAYMENT_ACTION_REQUIRED"
    expected_plan = "plus" if lease.stage != "upgrade" else ({"x5": "prolite", "x20": "pro"}.get(lease.target_tier, ""))
    contract = lease.price_contract
    amount = page.get("displayedAmount")
    base = page.get("plan") == expected_plan and page.get("country") == "PH" and page.get("currency") == "PHP" and isinstance(amount, (int, float)) and float(contract["MinAmount"]) <= amount <= float(contract["MaxAmount"])
    if purpose == "selection":
        if not base or page.get("origin") != CHATGPT_ORIGIN or page.get("routeTemplate") != "/settings/subscription": return "UNKNOWN_PAYMENT_STATE"
        if page.get("controls", {}).get("challenge"): return "PAYMENT_ACTION_REQUIRED"
        control = "upgradeX5" if lease.target_tier == "x5" else "upgradeX20"
        return "UPGRADE_SELECTION_READY" if page.get("controls", {}).get(control) == ("upgrade-x5" if control == "upgradeX5" else "upgrade-x20") else "UNKNOWN_PAYMENT_STATE"
    if not base or page.get("routeTemplate") not in {"/checkout", "/checkout/{id}", "/pay/{id}"}: return "UNKNOWN_PAYMENT_STATE"
    if page.get("controls", {}).get("challenge"): return "PAYMENT_ACTION_REQUIRED"
    fields = page.get("fields", {}); controls = page.get("controls", {})
    card = fields.get("cardNumber") and fields.get("cvc") and (fields.get("expiry") or fields.get("expiryMonth") and fields.get("expiryYear"))
    billing = fields.get("billingName") and fields.get("billingCountry") and fields.get("billingPostal")
    address = sum(bool(fields.get(key)) for key in ("billingLine1", "billingCity", "billingState"))
    if not card or not billing or address not in {0, 3} or (bool(controls.get("progression")) == bool(controls.get("submit"))): return "UNKNOWN_PAYMENT_STATE"
    return "PAYMENT_PROGRESSION_READY" if controls.get("progression") else "PAYMENT_FINAL_READY"


def _unique(items: list[dict[str, Any]], key: str) -> Any:
    values = {item.get(key) for item in items if item.get(key) not in (None, "")}
    return values.pop() if len(values) == 1 else ""


def _unique_amount(items: list[dict[str, Any]]) -> float | None:
    values = {item.get("displayedAmount") for item in items if item.get("displayedAmount") is not None}
    return float(values.pop()) if len(values) == 1 else None


def _fill_frames(page: Any, material: dict[str, Any]) -> None:
    card = material.get("card", {}); billing = material.get("billing", {})
    year = str(card.get("ExpiryYear", card.get("expiryYear", "")))
    fragment = {"cardNumber": str(card.get("Number", card.get("number", ""))), "expiry": str(card.get("ExpiryMonth", card.get("expiryMonth", ""))) + "/" + year[-2:], "expiryMonth": str(card.get("ExpiryMonth", card.get("expiryMonth", ""))), "expiryYear": year, "cvc": str(card.get("CVV", card.get("cvv", ""))), "billingName": str(billing.get("Name", billing.get("name", ""))), "billingLine1": str(billing.get("Line1", billing.get("line1", ""))), "billingCity": str(billing.get("City", billing.get("city", ""))), "billingState": str(billing.get("State", billing.get("state", ""))), "billingCountry": str(billing.get("Country", billing.get("country", ""))), "billingPostal": str(billing.get("PostalCode", billing.get("postalCode", "")))}
    filled: set[str] = set()
    for frame in page.frames:
        try:
            result = frame.evaluate(FILL_FRAME_JS, fragment)
            filled.update(result.get("filled", []))
        except Exception:
            continue
    if not ("cardNumber" in filled and "cvc" in filled and ("expiry" in filled or {"expiryMonth", "expiryYear"} <= filled) and "billingName" in filled and "billingCountry" in filled and "billingPostal" in filled):
        raise ExecutorAPIError("PAYMENT_FIELDS_NOT_FILLED", 409)


def _activate_frames(page: Any, control_id: str) -> bool:
    selectors = CONTROL_SELECTORS.get(control_id, ())
    for frame in page.frames:
        if urlsplit(frame.url).scheme + "://" + urlsplit(frame.url).netloc not in {CHATGPT_ORIGIN, PAY_ORIGIN}:
            continue
        for selector in selectors:
            try:
                locator = frame.locator(selector)
                if locator.count() != 1 or not locator.is_visible() or not locator.is_enabled():
                    continue
                locator.click(timeout=3000)
                return True
            except Exception:
                continue
    return False

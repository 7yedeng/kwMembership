"""Offline real-browser input isolation regression for userscript v0.1.2.

Usage:
  python session-login.browser.test.py --chrome-path <installed-chrome.exe>

Only a locally fulfilled loopback page and blank child frames are used.
All other requests are aborted. No real Session, cookies, account, system
clipboard or live ChatGPT page is accessed. Native typing/Enter and synthetic
composed keyboard/clipboard events are tested separately.
"""

from __future__ import annotations

import argparse
import json
from pathlib import Path
import sys

ORIGIN = "http://127.0.0.1:8765"
FIXTURE_URL = ORIGIN + "/userscript-input-fixture"
SYNTHETIC_TEXT = "OFFLINE_SYNTHETIC_INPUT_ONLY"

HTML = """<!doctype html><html><head><meta charset="utf-8"></head><body>
<textarea id="chat" aria-label="Synthetic chat editor"></textarea>
<script>
window.fixture = {parentKeys:0,parentPaste:0,parentPointers:0,parentBubbleKeys:0,submits:0,cookieOps:0,fetchOps:0};
window.addEventListener('pointerdown', function() { fixture.parentPointers++; }, true);
window.addEventListener('keydown', function(event) {
  fixture.parentKeys++;
  document.getElementById('chat').focus();
  if (event.key === 'Enter') fixture.submits++;
}, true);
document.addEventListener('keydown', function() { fixture.parentBubbleKeys++; });
window.addEventListener('paste', function(event) {
  fixture.parentPaste++;
  document.getElementById('chat').value += event.clipboardData ? event.clipboardData.getData('text/plain') : '';
}, true);
const realShadow = Element.prototype.attachShadow;
Element.prototype.attachShadow = function(options) {
  const root = realShadow.call(this, options);
  window.fixtureRoot = root; return root;
};
window.GM_cookie = {
  list: function() { fixture.cookieOps++; throw new Error('TEST_COOKIE_ACCESS_FORBIDDEN'); },
  set: function() { fixture.cookieOps++; throw new Error('TEST_COOKIE_ACCESS_FORBIDDEN'); },
  delete: function() { fixture.cookieOps++; throw new Error('TEST_COOKIE_ACCESS_FORBIDDEN'); }
};
window.GM_getTab = function(callback) { callback({}); };
window.GM_saveTab = function(_tab, callback) { callback(); };
window.GM_getTabs = function(callback) { callback({}); };
window.GM_registerMenuCommand = function(_label, callback) { window.fixtureOpen = callback; };
window.fetch = function() { fixture.fetchOps++; throw new Error('TEST_NETWORK_ACCESS_FORBIDDEN'); };
document.getElementById('chat').focus();
</script></body></html>"""


def check(condition, code):
    if not condition:
        raise AssertionError(code)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--chrome-path")
    parser.add_argument("--script", default=str(Path(__file__).with_name("session-login.user.js")))
    arguments = parser.parse_args()
    source = Path(arguments.script).read_text(encoding="utf-8")
    original = "const ORIGIN = 'https://chatgpt.com';"
    check(source.count(original) == 1, "TEST_ORIGIN_PATCH_MUST_MATCH_ONCE")
    source = source.replace(original, "const ORIGIN = '" + ORIGIN + "';", 1)

    from playwright.sync_api import sync_playwright

    passed = []
    state = {"stage": "launch", "blocked_requests": 0, "unmarked_responses": 0}
    with sync_playwright() as playwright:
        options = {
            "headless": True,
            "timeout": 15000,
            "proxy": {"server": "http://127.0.0.1:9"},
            "args": ["--disable-background-networking"],
        }
        if arguments.chrome_path:
            options["executable_path"] = arguments.chrome_path
        browser = playwright.chromium.launch(**options)

        def context_for(strict_policy=False):
            context = browser.new_context(service_workers="block")
            context.set_default_timeout(5000)
            def route_local(route):
                if route.request.url == FIXTURE_URL:
                    headers = {"X-Offline-Input-Fixture": "1"}
                    if strict_policy:
                        headers["Content-Security-Policy"] = "require-trusted-types-for 'script'"
                    route.fulfill(status=200, content_type="text/html", headers=headers, body=HTML)
                elif route.request.url == ORIGIN + "/favicon.ico":
                    route.fulfill(status=204, headers={"X-Offline-Input-Fixture": "1"}, body="")
                else:
                    state["blocked_requests"] += 1
                    route.abort()
            context.route("**/*", route_local)
            def check_response(response):
                if response.headers.get("x-offline-input-fixture") != "1":
                    state["unmarked_responses"] += 1
            context.on("response", check_response)
            page = context.new_page()
            page.goto(FIXTURE_URL, wait_until="load")
            page.bring_to_front()
            return context, page

        try:
            state["stage"] = "closed-shadow-control"
            context, page = context_for()
            control = page.evaluate("""() => {
              const host=document.createElement('div');document.body.append(host);
              const root=host.attachShadow({mode:'closed'});
              const input=document.createElement('textarea');root.append(input);input.focus();
              input.dispatchEvent(new KeyboardEvent('keydown',{key:'v',ctrlKey:true,bubbles:true,composed:true}));
              return {captured:fixture.parentKeys,chatFocused:document.activeElement===document.getElementById('chat')};
            }""")
            check(control["captured"] == 1 and control["chatFocused"], "SHADOW_CONTROL_DID_NOT_REPRODUCE_CAPTURE")
            passed.append("closed-shadow-alone-allows-parent-capture")
            context.close()

            state["stage"] = "isolated-input"
            context, page = context_for()
            page.evaluate(source)
            page.wait_for_function("""() => window.fixtureRoot &&
              fixtureRoot.querySelector('iframe')?.contentDocument?.querySelector('textarea') &&
              !fixtureRoot.querySelector('iframe').contentDocument.querySelector('textarea').disabled""")
            page.evaluate("fixtureRoot.querySelector('.focus-control').click()")
            frame_element = page.evaluate_handle("fixtureRoot.querySelector('iframe')").as_element()
            check(frame_element is not None, "INPUT_FRAME_NOT_FOUND")
            frame = frame_element.content_frame()
            check(frame is not None, "INPUT_FRAME_CONTEXT_MISSING")
            editor = frame.locator("textarea")
            editor.click()
            check(editor.evaluate("(input) => input.ownerDocument.activeElement===input && input.ownerDocument.hasFocus()"),
                  "FOCUS_NOT_IN_CHILD_DOCUMENT")
            page.keyboard.type(SYNTHETIC_TEXT)
            page.keyboard.press("Enter")
            paste = editor.evaluate("""input => {
              const data=new DataTransfer();data.setData('text/plain','SYNTHETIC_CLIPBOARD_EVENT_ONLY');
              const event=new ClipboardEvent('paste',{bubbles:true,composed:true,cancelable:true,clipboardData:data});
              const allowed=input.dispatchEvent(event);
              input.dispatchEvent(new KeyboardEvent('keydown',{key:'v',ctrlKey:true,bubbles:true,composed:true}));
              return {allowed,prevented:event.defaultPrevented};
            }""")
            check(paste["allowed"] and not paste["prevented"], "CHILD_PASTE_DEFAULT_WAS_BLOCKED")
            check(editor.input_value() == SYNTHETIC_TEXT + "\n", "NATIVE_TYPING_OR_ENTER_MISROUTED")
            observed = page.evaluate("""() => ({
              ...fixture,chat:document.getElementById('chat').value,
              ready:fixtureRoot.querySelector('.input-focus').classList.contains('ready'),
              sandbox:fixtureRoot.querySelector('iframe').getAttribute('sandbox'),
              pointer:getComputedStyle(fixtureRoot.querySelector('iframe')).pointerEvents,
              overscroll:getComputedStyle(fixtureRoot.querySelector('.panel')).overscrollBehaviorY,
              topLevelInputs:fixtureRoot.querySelectorAll('textarea').length
            })""")
            check(observed["parentKeys"] == 0 and observed["parentBubbleKeys"] == 0 and observed["parentPaste"] == 0 and observed["parentPointers"] == 0,
                  "CHILD_EVENTS_REACHED_PARENT_LISTENERS")
            check(observed["chat"] == "" and observed["submits"] == 0, "CHAT_CHANGED_OR_SUBMITTED")
            check(observed["cookieOps"] == 0 and observed["fetchOps"] == 0, "ACCOUNT_OPERATION_ATTEMPTED")
            check(observed["ready"] and observed["sandbox"] == "allow-same-origin", "INPUT_BOUNDARY_OR_FOCUS_INVALID")
            check(observed["pointer"] == "auto" and observed["overscroll"] == "contain", "POINTER_OR_SCROLL_BOUNDARY_INVALID")
            check(observed["topLevelInputs"] == 0, "PARENT_TEXTAREA_FALLBACK_FOUND")
            passed.append("native-typing-and-enter-stay-in-child")
            passed.append("composed-keyboard-and-paste-do-not-reach-earlier-parent-capture")
            passed.append("focus-pointer-scroll-and-no-parent-input")
            page.evaluate("fixtureRoot.querySelector('.close').click()")
            check(editor.input_value() == "", "CLOSE_DID_NOT_CLEAR_INPUT")
            page.evaluate("fixtureOpen()")
            check(editor.evaluate("(input) => input.ownerDocument.hasFocus()"), "MENU_DID_NOT_REFOCUS_CHILD")
            passed.append("close-clears-and-menu-refocuses")
            context.close()

            state["stage"] = "frame-access-failure"
            context, page = context_for()
            page.evaluate("""Object.defineProperty(HTMLIFrameElement.prototype, 'contentDocument',
              {configurable:true,get(){throw new Error('SYNTHETIC_FRAME_ACCESS_DENIED')}})""")
            page.evaluate(source)
            page.wait_for_function("fixtureRoot.querySelector('.status').textContent.includes('INPUT_FRAME_UNAVAILABLE')")
            failure = page.evaluate("""() => ({
              disabled:fixtureRoot.querySelector('.start').disabled,
              parentInputs:fixtureRoot.querySelectorAll('textarea').length,
              status:fixtureRoot.querySelector('.status').textContent,
              cookieOps:fixture.cookieOps,fetchOps:fixture.fetchOps
            })""")
            check(failure["disabled"] and failure["parentInputs"] == 0, "FRAME_FAILURE_DID_NOT_DISABLE_RECOVERY")
            check("SYNTHETIC_FRAME_ACCESS_DENIED" not in failure["status"], "RAW_ERROR_EXPOSED")
            check(failure["cookieOps"] == 0 and failure["fetchOps"] == 0, "FAILURE_ATTEMPTED_ACCOUNT_ACCESS")
            passed.append("blocked-frame-stops-without-parent-input-fallback")
            context.close()

            state["stage"] = "trusted-types-policy"
            context, page = context_for(strict_policy=True)
            page.evaluate(source)
            page.wait_for_function("""() => fixtureRoot.querySelector('iframe')?.contentDocument?.querySelector('textarea')
              && !fixtureRoot.querySelector('.start').disabled""")
            passed.append("trusted-types-without-srcdoc-or-innerHTML")
            context.close()

            check(state["blocked_requests"] == 0 and state["unmarked_responses"] == 0, "UNEXPECTED_NETWORK_ATTEMPT")
            print(json.dumps({
                "status": "passed", "checks": passed, "network": "local_fulfillment_only",
                "realAccountUsed": False, "systemClipboardUsed": False, "liveChatGPTVisited": False,
                "scope": "synthetic DOM regression, not live userscript-manager acceptance",
            }, ensure_ascii=False))
            return 0
        except Exception as error:
            # These tests contain no private input, but fixed diagnostics avoid
            # copying browser launch commands or raw page errors into reports.
            print(json.dumps({"status": "failed", "stage": state["stage"],
                              "errorType": type(error).__name__,
                              "check": str(error) if isinstance(error, AssertionError) else "BROWSER_TEST_FAILED",
                              "completedChecks": passed}, ensure_ascii=False))
            return 1
        finally:
            browser.close()


if __name__ == "__main__":
    try:
        sys.exit(main())
    except Exception as error:
        message = str(error)
        if "Executable doesn't exist" in message:
            code = "BROWSER_EXECUTABLE_MISSING"
        elif "socket() failed: Operation not permitted" in message:
            code = "BROWSER_UNIX_SOCKET_DENIED"
        else:
            code = "BROWSER_TEST_SETUP_FAILED"
        print(json.dumps({"status": "unavailable", "code": code, "errorType": type(error).__name__}))
        sys.exit(2)


'use strict';

// Offline UI boot regression tests. This VM has no real browser, Cookie store,
// account, or network. DOM doubles exercise startup and visible diagnostics only.
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const { webcrypto } = require('node:crypto');

const source = fs.readFileSync(require.resolve('./session-login.user.js'), 'utf8');
const UI_ID = 'anagnoraxis-session-restore-ui';
const tick = () => new Promise(resolve => setImmediate(resolve));

function harness(options = {}) {
  const nodes = [];
  const documentEvents = new Map();
  const windowEvents = new Map();
  const menus = [];
  const calls = { cookie: 0, fetch: 0, navigate: 0 };

  class Element {
    constructor(tag) {
      this.tagName = tag;
      this.children = [];
      this.listeners = new Map();
      this.style = {};
      this.dataset = {};
      this.attributes = {};
      this.className = '';
      this.textContent = '';
      this.value = '';
      this.hidden = false;
      this.disabled = false;
      this.parentNode = null;
      this.ownerDocument = null;
      this.isConnected = false;
      this.classList = {
        add: value => { this.className = [...new Set([...this.className.split(' ').filter(Boolean), value])].join(' '); },
        remove: value => { this.className = this.className.split(' ').filter(item => item !== value).join(' '); },
        contains: value => this.className.split(' ').includes(value)
      };
      nodes.push(this);
      if (tag === 'iframe') {
        const frame = this;
        const childDocument = {
          body: new Element('body'), activeElement: null,
          hasFocus: () => document.activeElement === frame,
          createElement: name => { const node = new Element(name); node.ownerDocument = childDocument; return node; }
        };
        childDocument.frame = frame;
        childDocument.body.ownerDocument = childDocument;
        this.contentDocument = options.inputFrameFailure ? null : childDocument;
      }
    }
    append(...items) {
      for (const item of items) {
        item.parentNode = this; item.isConnected = true;
        if (this.ownerDocument) item.ownerDocument = this.ownerDocument;
        this.children.push(item);
      }
    }
    remove() {
      if (this.parentNode) this.parentNode.children = this.parentNode.children.filter(item => item !== this);
      this.parentNode = null;
    }
    setAttribute(name, value) { this.attributes[name] = value; }
    addEventListener(type, listener) { this.listeners.set(type, listener); }
    attachShadow() {
      if (options.shadowFailure) throw new Error('synthetic-initialization-failure');
      this.shadow = new Element('shadow-root');
      this.shadow.ownerDocument = this.ownerDocument;
      return this.shadow;
    }
    focus() {
      this.focused = true;
      if (this.ownerDocument) {
        this.ownerDocument.activeElement = this;
        document.activeElement = this.ownerDocument.frame || this;
      }
      if (this.listeners.has('focus')) this.listeners.get('focus')();
    }
    async click() { if (!this.disabled && this.listeners.has('click')) await this.listeners.get('click')(); }
  }

  const document = {
    readyState: options.loading ? 'loading' : 'complete',
    documentElement: new Element('html'),
    activeElement: null,
    createElement: tag => { const node = new Element(tag); node.ownerDocument = document; return node; },
    getElementById(id) {
      const visit = node => node.id === id ? node : node.children.map(visit).find(Boolean);
      return visit(this.documentElement) || null;
    },
    addEventListener(type, listener, settings) { documentEvents.set(type, { listener, once: settings && settings.once }); }
  };
  const window = { addEventListener: (type, listener) => windowEvents.set(type, listener) };
  window.self = window;
  window.top = options.frame ? {} : window;
  const context = {
    window, document,
    location: { origin: options.origin || 'https://chatgpt.com', replace() { calls.navigate++; } },
    navigator: { locks: { request: async (_name, _settings, callback) => callback({}) } },
    crypto: options.noCrypto ? undefined : webcrypto,
    TextEncoder, Uint8Array, AbortController, setTimeout, clearTimeout,
    fetch: async () => { calls.fetch++; throw new Error('Network forbidden in UI boot test'); },
    GM_getTab: callback => callback({}),
    GM_saveTab: (_tab, callback) => callback(),
    GM_getTabs: callback => callback({}),
    GM_registerMenuCommand: (name, callback) => {
      if (options.menuFailure) throw new Error('synthetic-menu-failure');
      menus.push({ name, callback });
    }
  };
  if (!options.noCookieAPI) context.GM_cookie = {
    list: (_details, callback) => { calls.cookie++; callback([]); },
    set: (_details, callback) => { calls.cookie++; callback(); },
    delete: (_details, callback) => { calls.cookie++; callback(); }
  };
  if (options.moduleObject) context.module = options.moduleObject;
  vm.createContext(context);
  const run = () => vm.runInContext(source, context, { timeout: 2000 });
  function fireDOMContentLoaded() {
    document.readyState = 'complete';
    const event = documentEvents.get('DOMContentLoaded');
    if (event) {
      if (event.once) documentEvents.delete('DOMContentLoaded');
      event.listener();
    }
  }
  return {
    run, context, document, nodes, menus, calls, fireDOMContentLoaded, windowEvents,
    host: () => document.getElementById(UI_ID),
    find: predicate => nodes.find(predicate),
    assertNoAccountOperations() { assert.deepEqual(calls, { cookie: 0, fetch: 0, navigate: 0 }); }
  };
}

test('browser with a global module.exports still mounts and does not expose flow helpers', async () => {
  const existing = { sentinel: 'page-owned' };
  const moduleObject = { exports: existing };
  const h = harness({ moduleObject });
  h.run();
  await tick();
  assert.ok(h.host());
  assert.equal(h.host().attributes['data-version'], '0.1.2');
  assert.equal(moduleObject.exports, existing);
  assert.ok(h.find(node => node.tagName === 'section').classList.contains('open'));
  assert.equal(h.find(node => node.className === 'start').disabled, false);
  assert.match(h.find(node => node.className === 'status').textContent, /界面已运行/);
  h.assertNoAccountOperations();
});

test('missing Cookie API shows a visible fixed error and disables recovery without account access', async () => {
  const h = harness({ noCookieAPI: true });
  h.run();
  await tick();
  assert.ok(h.host());
  assert.ok(h.find(node => node.tagName === 'section').classList.contains('open'));
  assert.match(h.find(node => node.className === 'status error').textContent, /GM_UNAVAILABLE/);
  assert.equal(h.find(node => node.className === 'start').disabled, true);
  h.assertNoAccountOperations();
});

test('DOMContentLoaded mounts once and duplicate script execution does not duplicate the UI', async () => {
  const h = harness({ loading: true });
  h.run();
  assert.equal(h.host(), null);
  h.fireDOMContentLoaded();
  await tick();
  assert.ok(h.host());
  h.fireDOMContentLoaded();
  h.run();
  await tick();
  assert.equal(h.document.documentElement.children.length, 1);
  h.assertNoAccountOperations();
});

test('menu registration failure does not prevent the visible panel or page toggle', async () => {
  const h = harness({ menuFailure: true });
  h.run();
  await tick();
  const input = h.find(node => node.tagName === 'textarea');
  input.value = 'synthetic-private-input';
  await h.find(node => node.className === 'close').click();
  assert.equal(input.value, '');
  assert.equal(h.find(node => node.className === 'toggle').hidden, false);
  await h.find(node => node.className === 'toggle').click();
  assert.ok(h.find(node => node.tagName === 'section').classList.contains('open'));
  assert.equal(h.find(node => node.className === 'start').disabled, false);
  h.assertNoAccountOperations();
});

test('an unavailable crypto runtime leaves a visible diagnostic and no recovery action', async () => {
  const h = harness({ noCrypto: true });
  h.run();
  await tick();
  assert.match(h.find(node => node.className === 'status error').textContent, /RUNTIME_UNAVAILABLE/);
  assert.equal(h.find(node => node.className === 'start').disabled, true);
  h.assertNoAccountOperations();
});

test('a UI initialization failure produces a fixed fallback notice without leaking its raw error', async () => {
  const h = harness({ shadowFailure: true });
  h.run();
  await tick();
  assert.equal(h.host().attributes.role, 'alert');
  assert.match(h.host().textContent, /UI_BOOT_FAILED/);
  assert.ok(!h.host().textContent.includes('synthetic-initialization-failure'));
  h.assertNoAccountOperations();
});

test('unmatched origins and nested frames do not mount or touch account state', async () => {
  for (const options of [{ frame: true }, { origin: 'https://example.test' }]) {
    const h = harness(options);
    h.run();
    await tick();
    assert.equal(h.host(), null);
    h.assertNoAccountOperations();
  }
});


test('sensitive input is adopted by the sandboxed child document and explicitly focuses there', async () => {
  const h = harness();
  h.run();
  await tick();
  const frame = h.find(node => node.tagName === 'iframe');
  const input = h.find(node => node.tagName === 'textarea');
  assert.equal(frame.attributes.sandbox, 'allow-same-origin');
  assert.equal(input.ownerDocument, frame.contentDocument);
  assert.notEqual(input.ownerDocument, h.document);
  await h.find(node => node.className === 'focus-control').click();
  assert.equal(frame.contentDocument.activeElement, input);
  assert.match(h.find(node => node.className === 'input-focus ready').textContent, /输入焦点已在蓝框内/);
  h.assertNoAccountOperations();
});

test('misplaced parent paste is cancelled without reading or rerouting clipboard data', async () => {
  const h = harness();
  h.run();
  await tick();
  let prevented = false;
  let stopped = false;
  const event = {
    preventDefault() { prevented = true; },
    stopImmediatePropagation() { stopped = true; },
    get clipboardData() { throw new Error('clipboard must never be read'); }
  };
  h.windowEvents.get('paste')(event);
  assert.equal(prevented, true);
  assert.equal(stopped, true);
  assert.match(h.find(node => node.className === 'input-focus').textContent, /粘贴焦点不在专用输入区/);
  assert.equal(h.find(node => node.tagName === 'textarea').value, '');
  await h.find(node => node.className === 'close').click();
  prevented = false; stopped = false;
  h.windowEvents.get('paste')(event);
  assert.equal(prevented, false);
  assert.equal(stopped, false);
  h.assertNoAccountOperations();
});

test('blocked child document disables recovery and never falls back to a parent textarea', async () => {
  const h = harness({ inputFrameFailure: true });
  h.run();
  await tick();
  h.find(node => node.tagName === 'iframe').listeners.get('error')();
  assert.equal(h.find(node => node.className === 'start').disabled, true);
  assert.equal(h.find(node => node.tagName === 'textarea').parentNode, null);
  assert.match(h.find(node => node.className === 'status error').textContent, /INPUT_FRAME_UNAVAILABLE/);
  h.assertNoAccountOperations();
});

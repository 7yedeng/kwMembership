'use strict';

// OFFLINE ONLY. Fixtures are invented strings, not real credentials.
// The userscript is loaded through its Node export branch. No DOM or browser is created.
const test = require('node:test');
const assert = require('node:assert/strict');
const { createHash } = require('node:crypto');
const flow = require('./session-login.user.js');

// Any accidental use of the real fetch API fails immediately.
globalThis.fetch = async () => { throw new Error('Real network is forbidden in this test suite'); };

const NOW = Date.parse('2026-11-13T08:00:00.000Z');
const TOKEN = 'synthetic-session-for-offline-testing-' + 'abcDEF0123456789._~-'.repeat(8);
const OLD_TOKEN = 'previous-synthetic-offline-session-' + 'z'.repeat(80);
const BASE_SESSION = {
  sessionToken: TOKEN,
  accessToken: 'synthetic-access-token-not-used',
  user: { id: 'offline-user-001', email: 'Tester@Example.test' },
  account: { id: 'offline-account-001' },
  expires: new Date(NOW + 3600000).toISOString()
};
const copy = value => structuredClone(value);
const raw = changes => JSON.stringify({ ...copy(BASE_SESSION), ...changes });
const codeIs = code => error => error instanceof flow.FlowError && error.code === code;
const plainDomain = value => value.replace(/^\./, '').toLowerCase();

test('matching identity cannot hide a server-reported session error', async () => {
  const fixture = makeEnv({ response: { ...copy(BASE_SESSION), error: 'SyntheticSessionRefreshError' } });
  await flow.prepareFlow(raw(), fixture.env);
  await assert.rejects(() => flow.resumeFlow(fixture.env), codeIs('SESSION_REJECTED'));
});

test('matching identity cannot make an expired server session count as logged in', async () => {
  const fixture = makeEnv({ response: { ...copy(BASE_SESSION), expires: new Date(NOW - 1000).toISOString() } });
  await flow.prepareFlow(raw(), fixture.env);
  await assert.rejects(() => flow.resumeFlow(fixture.env), codeIs('SESSION_REJECTED'));
});

function browserCookie(details) {
  const c = {
    name: details.name,
    value: details.value,
    domain: details.domain ?? flow.HOST,
    path: details.path ?? '/',
    secure: details.secure === true,
    httpOnly: details.httpOnly === true,
    hostOnly: details.domain == null,
    sameSite: details.sameSite ?? 'unspecified',
    session: details.expirationDate == null
  };
  if (details.expirationDate != null) c.expirationDate = details.expirationDate;
  if (details.partitionKey != null) c.partitionKey = copy(details.partitionKey);
  return c;
}
function oldCookie(changes = {}) {
  return browserCookie({ name: flow.BASE, value: OLD_TOKEN, domain: '.' + flow.HOST,
    path: '/', secure: true, httpOnly: true, sameSite: 'lax', ...changes });
}
function matchesUrl(cookie, url) {
  const parsed = new URL(url);
  const domain = plainDomain(cookie.domain);
  const hostMatches = cookie.hostOnly ? parsed.hostname === domain :
    parsed.hostname === domain || parsed.hostname.endsWith('.' + domain);
  return hostMatches && parsed.pathname.startsWith(cookie.path);
}

function makeEnv(options = {}) {
  let jar = copy(options.cookies ?? [oldCookie()]);
  let state = copy(options.state ?? null);
  let sequence = 0;
  let clock = NOW;
  let navigationCount = 0;
  let sessionReads = 0;
  const events = [];
  const stateWrites = [];
  const phases = [];
  const env = {
    now: () => clock,
    nonce: () => 'offline_nonce_' + String(++sequence).padStart(8, '0'),
    hash: async value => createHash('sha256').update(value).digest('hex'),
    phase: (number, status) => phases.push([number, status]),
    navigate() { navigationCount++; if (options.navigationError) throw options.navigationError; },
    async getSession() {
      sessionReads++;
      if (options.sessionError) throw options.sessionError;
      return copy(options.response ?? BASE_SESSION);
    },
    state: {
      async get() { return copy(state); },
      async set(value) { state = copy(value); stateWrites.push(copy(value)); }
    },
    cookies: {
      async list(details) {
        events.push({ action: 'list', details: copy(details) });
        let selected = jar.filter(c => {
          if (details.name != null && c.name !== details.name) return false;
          if (details.domain != null) {
            const domain = plainDomain(c.domain);
            if (domain !== details.domain && !domain.endsWith('.' + details.domain)) return false;
          }
          if (details.url && !matchesUrl(c, details.url)) return false;
          return true;
        });
        selected = copy(selected);
        return options.onList ? options.onList(selected, copy(details), events) : selected;
      },
      async set(details) {
        const isolated = copy(details);
        events.push({ action: 'set', details: isolated });
        let cookie = browserCookie(isolated);
        if (options.onSet) cookie = await options.onSet(copy(cookie), copy(isolated), events);
        if (cookie === null) return;
        const index = jar.findIndex(c => c.name === cookie.name && c.domain === cookie.domain && c.path === cookie.path);
        if (index >= 0) jar[index] = copy(cookie); else jar.push(copy(cookie));
        if (options.afterSet) await options.afterSet(copy(cookie), copy(isolated), events);
      },
      async delete(details) {
        events.push({ action: 'delete', details: copy(details) });
        if (options.noDelete?.(details)) return;
        const candidates = jar.map((cookie, index) => ({ cookie, index })).filter(({ cookie }) =>
          cookie.name === details.name && matchesUrl(cookie, details.url));
        candidates.sort((a, b) => b.cookie.path.length - a.cookie.path.length);
        if (candidates.length) jar.splice(candidates[0].index, 1);
      }
    }
  };
  return { env, events, stateWrites, phases,
    jar: () => copy(jar), state: () => copy(state), navigationCount: () => navigationCount,
    sessionReads: () => sessionReads, setTime: value => { clock = value; },
    replaceJar: value => { jar = copy(value); }, setState: value => { state = copy(value); } };
}
function authMutations(events) {
  return events.filter(event => ['set', 'delete'].includes(event.action) && flow.sessionName(event.details.name));
}
async function pendingFor(env, phase = 'awaiting_refresh') {
  return { v: 1, nonce: 'offline_pending_nonce_001', phase,
    created: NOW, deadline: NOW + flow.STATE_TTL,
    expected: await flow.identityDigests(flow.identityOf(BASE_SESSION), env) };
}

test('parses a complete synthetic Session without changing its token', () => {
  const model = flow.parseSession(raw(), NOW);
  assert.equal(model.token, TOKEN);
  assert.deepEqual(model.identity, { userId: 'offline-user-001', accountId: 'offline-account-001', email: 'tester@example.test' });
});

test('access-token-only input is rejected before any browser operation', async () => {
  const h = makeEnv();
  await assert.rejects(flow.prepareFlow(JSON.stringify({ accessToken: 'fake.jwt.value', user: BASE_SESSION.user }), h.env), codeIs('TOKEN_MISSING'));
  assert.equal(h.events.length, 0);
  assert.equal(h.stateWrites.length, 0);
});

test('malformed JSON, missing identity, expired and invalid expiry inputs are rejected', () => {
  assert.throws(() => flow.parseSession('not json', NOW), codeIs('INPUT_JSON'));
  assert.throws(() => flow.parseSession('[]', NOW), codeIs('INPUT_JSON'));
  assert.throws(() => flow.parseSession(raw({ user: {}, account: {} }), NOW), codeIs('IDENTITY_MISSING'));
  assert.throws(() => flow.parseSession(raw({ expires: new Date(NOW - 1).toISOString() }), NOW), codeIs('EXPIRED'));
  assert.throws(() => flow.parseSession(raw({ expires: 'not-a-date' }), NOW), codeIs('EXPIRY_INVALID'));
});

test('token validation refuses whitespace, delimiters and excessive length without silently trimming', () => {
  for (const value of [' ' + TOKEN, TOKEN + ';', TOKEN + '\n', 'a'.repeat(65537)]) {
    assert.throws(() => flow.parseSession(raw({ sessionToken: value }), NOW), codeIs('TOKEN_INVALID'));
  }
});

test('long token chunks reconstruct the exact original bytes at relevant boundaries', () => {
  for (const length of [3933, 3934, 7866, 65536]) {
    const token = 'A'.repeat(length - 5) + '._~-Z';
    const cookies = flow.buildCookies(flow.parseSession(raw({ sessionToken: token }), NOW));
    assert.equal(cookies.map(c => c.value).join(''), token);
    assert.equal(cookies.length, Math.ceil(length / 3933));
    assert.ok(cookies.every(c => c.value.length <= 3933 && c.httpOnly && c.secure && !('expirationDate' in c)));
    assert.equal(cookies[0].name, length <= 3933 ? flow.BASE : flow.BASE + '.0');
  }
});

test('six-step success waits for post-refresh identity and stores no token or cleartext identity', async () => {
  const unrelated = browserCookie({ name: 'unrelated-preference', value: 'keep', domain: '.' + flow.HOST, path: '/' });
  const h = makeEnv({ cookies: [oldCookie(), unrelated] });
  const prepared = await flow.prepareFlow(raw(), h.env);
  assert.deepEqual(prepared, { status: 'awaiting_refresh' });
  assert.equal(h.navigationCount(), 1);
  assert.equal(h.sessionReads(), 0);
  assert.ok(!h.phases.some(([n, s]) => n === 6 && s === 'done'));
  const saved = JSON.stringify(h.stateWrites);
  for (const forbidden of [TOKEN, OLD_TOKEN, BASE_SESSION.accessToken, BASE_SESSION.user.id, BASE_SESSION.account.id, 'tester@example.test']) {
    assert.ok(!saved.includes(forbidden));
  }
  assert.equal(h.jar().find(c => c.name === flow.BASE).value, TOKEN);
  assert.deepEqual(h.jar().find(c => c.name === unrelated.name), unrelated);
  const verified = await flow.resumeFlow(h.env);
  assert.deepEqual(verified, { status: 'verified', email: 'tester@example.test' });
  for (let n = 1; n <= 6; n++) assert.ok(h.phases.some(([phase, status]) => phase === n && status === 'done'));
  assert.equal(h.sessionReads(), 1);
  assert.equal(h.state(), null);
});

test('HttpOnly capability failure leaves old authentication cookies untouched', async () => {
  const initial = [oldCookie()];
  const h = makeEnv({ cookies: initial, onSet: cookie => ({ ...cookie, httpOnly: false }) });
  await assert.rejects(flow.prepareFlow(raw(), h.env), codeIs('HTTPONLY_UNAVAILABLE'));
  assert.deepEqual(h.jar(), initial);
  assert.equal(authMutations(h.events).length, 0);
  assert.equal(h.navigationCount(), 0);
  assert.equal(h.stateWrites.length, 0);
});

test('failure to remove a probe stops before modifying the old account', async () => {
  const h = makeEnv({ noDelete: details => details.name.startsWith('__Host-anagnoraxis-probe-') });
  await assert.rejects(flow.prepareFlow(raw(), h.env), codeIs('PROBE_CLEANUP'));
  assert.equal(authMutations(h.events).length, 0);
  assert.equal(h.jar().find(c => c.name === flow.BASE).value, OLD_TOKEN);
});

test('unexpected session path or partition scope stops before deleting old authentication', async () => {
  for (const extra of [{ path: '/restricted' }, { partitionKey: { topLevelSite: 'https://example.test' } }]) {
    const h = makeEnv({ cookies: [oldCookie(extra)] });
    await assert.rejects(flow.prepareFlow(raw(), h.env), codeIs('COOKIE_SCOPE'));
    assert.equal(authMutations(h.events).length, 0);
    assert.equal(h.navigationCount(), 0);
  }
});

test('only the exact account-cookie group is replaced; similarly named, unrelated and subdomain cookies survive', async () => {
  const preserved = [
    browserCookie({ name: flow.BASE + '.invalid', value: 'keep-1', domain: '.' + flow.HOST }),
    browserCookie({ name: 'unrelated', value: 'keep-2', domain: '.' + flow.HOST }),
    browserCookie({ name: flow.BASE, value: 'keep-3', domain: '.sub.' + flow.HOST })
  ];
  const h = makeEnv({ cookies: [oldCookie(), ...preserved] });
  await flow.prepareFlow(raw(), h.env);
  for (const cookie of preserved) assert.ok(h.jar().some(actual => JSON.stringify(actual) === JSON.stringify(cookie)));
  assert.ok(authMutations(h.events).every(event => event.details.name === flow.BASE));
});

test('same-name host and domain cookies are both removed through bounded re-enumeration', async () => {
  const hostOnly = browserCookie({ name: flow.BASE, value: 'old-host-only-offline-token', path: '/', secure: true, httpOnly: true, sameSite: 'lax' });
  const h = makeEnv({ cookies: [oldCookie(), hostOnly] });
  await flow.prepareFlow(raw(), h.env);
  const current = h.jar().filter(c => c.name === flow.BASE);
  assert.equal(current.length, 1);
  assert.equal(current[0].value, TOKEN);
  assert.equal(authMutations(h.events).filter(e => e.action === 'delete').length, 2);
});

test('write-readback attribute mismatch restores the pre-refresh account snapshot', async () => {
  const original = [oldCookie()];
  const h = makeEnv({ cookies: original, onSet: cookie =>
    cookie.name === flow.BASE && cookie.value === TOKEN ? { ...cookie, httpOnly: false } : cookie });
  await assert.rejects(flow.prepareFlow(raw(), h.env), codeIs('COOKIE_READBACK'));
  assert.deepEqual(h.jar(), original);
  assert.equal(h.navigationCount(), 0);
  assert.equal(h.state(), null);
});

test('unknown cookie-write timeout does not perform a blind rollback', async () => {
  const h = makeEnv({ afterSet(cookie) {
    if (cookie.name === flow.BASE && cookie.value === TOKEN) throw new flow.FlowError('COOKIE_TIMEOUT');
  } });
  await assert.rejects(flow.prepareFlow(raw(), h.env), codeIs('COOKIE_TIMEOUT'));
  assert.equal(h.jar().find(c => c.name === flow.BASE).value, TOKEN);
  assert.equal(h.state().phase, 'uncertain');
  assert.equal(h.navigationCount(), 0);
  const auth = authMutations(h.events);
  assert.deepEqual(auth.map(e => e.action), ['delete', 'set']);
});

test('an externally replaced credential is not deleted or rolled back', async () => {
  const foreign = 'external-offline-test-session-' + 'q'.repeat(50);
  const h = makeEnv({ onSet: cookie => cookie.name === flow.BASE && cookie.value === TOKEN ?
    { ...cookie, value: foreign } : cookie });
  await assert.rejects(flow.prepareFlow(raw(), h.env), codeIs('COOKIE_CHANGED'));
  assert.equal(h.jar().find(c => c.name === flow.BASE).value, foreign);
  assert.equal(h.state().phase, 'uncertain');
  assert.deepEqual(authMutations(h.events).map(e => e.action), ['delete', 'set']);
});

test('a concurrent account change between snapshot validation and deletion is preserved', async () => {
  const foreign = oldCookie({ value: 'concurrent-offline-account-' + 'r'.repeat(60) });
  let domainReads = 0;
  let h;
  h = makeEnv({ onList(selected, details) {
    if (details.domain === flow.HOST && ++domainReads === 2) {
      // Return the snapshot just read, but let a separate actor replace the account
      // immediately afterwards, before the following delete routine enumerates it.
      h.replaceJar([foreign]);
    }
    return selected;
  } });
  await assert.rejects(flow.prepareFlow(raw(), h.env), codeIs('COOKIE_CHANGED'));
  assert.deepEqual(h.jar(), [foreign]);
  assert.equal(authMutations(h.events).length, 0);
  assert.equal(h.navigationCount(), 0);
});

test('post-refresh identity mismatch is not success and never clears the current cookies', async () => {
  const h = makeEnv({ response: { user: { id: 'different-offline-user', email: 'different@example.test' }, account: BASE_SESSION.account } });
  await flow.prepareFlow(raw(), h.env);
  const before = h.jar(); const mutations = authMutations(h.events).length;
  await assert.rejects(flow.resumeFlow(h.env), codeIs('IDENTITY_MISMATCH'));
  assert.deepEqual(h.jar(), before);
  assert.equal(authMutations(h.events).length, mutations);
  assert.ok(!h.phases.some(([n, status]) => n === 6 && status === 'done'));
  assert.equal(h.state(), null);
});

test('missing expected account identity fails without falling back to any logged-in email', async () => {
  const h = makeEnv({ response: { user: BASE_SESSION.user } });
  h.setState(await pendingFor(h.env));
  await assert.rejects(flow.resumeFlow(h.env), codeIs('IDENTITY_UNCONFIRMED'));
  assert.equal(h.events.length, 0);
});

test('a response with accessToken alone cannot prove the requested login', async () => {
  const h = makeEnv({ response: { accessToken: 'invented-value' } });
  h.setState(await pendingFor(h.env));
  await assert.rejects(flow.resumeFlow(h.env), codeIs('NOT_LOGGED_IN'));
});

test('pending state is consumed once and a repeated refresh makes no extra session request', async () => {
  const h = makeEnv(); h.setState(await pendingFor(h.env));
  assert.equal((await flow.resumeFlow(h.env)).status, 'verified');
  assert.equal((await flow.resumeFlow(h.env)).status, 'idle');
  assert.equal(h.sessionReads(), 1);
  assert.equal(h.events.length, 0);
});

test('expired or malformed pending state is removed without a request or credential write', async () => {
  for (const mutation of [state => ({ ...state, deadline: NOW - 1 }), state => ({ ...state, expected: { email: TOKEN } })]) {
    const h = makeEnv(); h.setState(mutation(await pendingFor(h.env)));
    await assert.rejects(flow.resumeFlow(h.env), codeIs('STATE_INVALID'));
    assert.equal(h.sessionReads(), 0);
    assert.equal(h.events.length, 0);
    assert.equal(h.state(), null);
  }
});

test('preparing and uncertain task markers never trigger automatic requests or new cookie writes', async () => {
  for (const phase of ['preparing', 'uncertain']) {
    const h = makeEnv(); h.setState(await pendingFor(h.env, phase));
    await assert.rejects(flow.resumeFlow(h.env), codeIs('UNCERTAIN'));
    assert.equal(h.sessionReads(), 0);
    assert.equal(h.events.length, 0);
  }
});

test('a navigation failure preserves one pending identity check instead of rewriting credentials', async () => {
  const h = makeEnv({ navigationError: new Error('simulated navigation failure') });
  await assert.rejects(flow.prepareFlow(raw(), h.env), codeIs('NAVIGATION'));
  assert.equal(h.state().phase, 'awaiting_refresh');
  assert.equal(h.jar().find(c => c.name === flow.BASE).value, TOKEN);
  assert.equal((await flow.resumeFlow(h.env)).status, 'verified');
});

test('session fetch uses the fixed destination with same-origin credentials and no redirect follow', async () => {
  const calls = [];
  const result = await flow.fetchCurrentSession(async (url, options) => {
    calls.push({ url, options });
    return new Response(JSON.stringify({ user: BASE_SESSION.user }), { headers: { 'content-type': 'application/json' } });
  });
  assert.equal(result.user.id, BASE_SESSION.user.id);
  assert.equal(calls.length, 1);
  assert.equal(calls[0].url, flow.SESSION_URL);
  assert.equal(calls[0].options.credentials, 'same-origin');
  assert.equal(calls[0].options.redirect, 'error');
  assert.equal(calls[0].options.method, 'GET');
});

test('HTTP failure is reported without reading a response body or retrying', async () => {
  let calls = 0; let bodyRead = false;
  await assert.rejects(flow.fetchCurrentSession(async () => {
    calls++;
    return { ok: false, text() { bodyRead = true; throw new Error('must not read'); } };
  }), codeIs('HTTP'));
  assert.equal(calls, 1); assert.equal(bodyRead, false);
});

test('non-JSON, invalid JSON and JSON arrays cannot be treated as authenticated sessions', async () => {
  const responses = [
    () => new Response('<html>verification</html>', { headers: { 'content-type': 'text/html' } }),
    () => new Response('{invalid', { headers: { 'content-type': 'application/json' } }),
    () => new Response('[]', { headers: { 'content-type': 'application/json' } })
  ];
  for (const response of responses) await assert.rejects(flow.fetchCurrentSession(async () => response()), codeIs('RESPONSE'));
});

test('network failures are translated to a safe result and do not expose raw messages', async () => {
  const secretLike = 'synthetic-secret-that-must-not-appear-in-the-error';
  let error;
  try { await flow.fetchCurrentSession(async () => { throw new Error(secretLike); }); } catch (caught) { error = caught; }
  assert.equal(error.code, 'NETWORK');
  assert.ok(!JSON.stringify(flow.safeError(error)).includes(secretLike));
  assert.ok(!JSON.stringify(flow.safeError(new Error(secretLike))).includes(secretLike));
});

test('GM callback adapter correctly treats set/delete success as undefined and masks raw errors', async () => {
  const adapter = flow.gmCookieAdapter({
    list(_details, callback) { callback([oldCookie()], null); },
    set(_details, callback) { callback(undefined); },
    delete(_details, callback) { callback(undefined); }
  });
  assert.equal((await adapter.list({})).length, 1);
  assert.equal(await adapter.set({}), undefined);
  assert.equal(await adapter.delete({}), undefined);
  const broken = flow.gmCookieAdapter({
    list(_details, callback) { callback(null, TOKEN); },
    set(_details, callback) { callback(TOKEN); },
    delete(_details, callback) { callback(TOKEN); }
  });
  for (const call of [() => broken.list({}), () => broken.set({}), () => broken.delete({})]) {
    await assert.rejects(call(), error => codeIs('COOKIE_API')(error) && !error.message.includes(TOKEN));
  }
});

test('GM tab-state adapter preserves unrelated metadata and distinguishes active/expired task state', async () => {
  let tab = { unrelated: { keep: true } };
  let tabs = {};
  const adapter = flow.gmStateAdapter(cb => cb(copy(tab)), (value, cb) => { tab = copy(value); cb(); }, cb => cb(copy(tabs)));
  const h = makeEnv(); const pending = await pendingFor(h.env);
  await adapter.set(pending);
  assert.deepEqual(tab.unrelated, { keep: true });
  assert.deepEqual(await adapter.get(), pending);
  tabs = { 1: copy(tab) };
  assert.equal(await adapter.busy(NOW), true);
  assert.equal(await adapter.busy(NOW + flow.STATE_TTL + 1), false);
  await adapter.set(null);
  assert.deepEqual(tab, { unrelated: { keep: true } });
  assert.equal(await adapter.get(), null);
});

test('missing GM interfaces fail explicitly', () => {
  assert.throws(() => flow.gmCookieAdapter(null), codeIs('GM_UNAVAILABLE'));
  assert.throws(() => flow.gmStateAdapter(null, null, null), codeIs('GM_UNAVAILABLE'));
});

// ==UserScript==
// @name         Anagnoraxis 会话恢复（本机）
// @namespace    anagnoraxis.local.session-helper
// @version      0.1.1
// @description  粘贴本人完整 Session，在当前浏览器配置中恢复会话并核对目标身份。需要 Tampermonkey Beta 的 HttpOnly Cookie 权限。
// @match        https://chatgpt.com/*
// @run-at       document-end
// @noframes
// @sandbox      DOM
// @grant        GM_cookie
// @grant        GM_getTab
// @grant        GM_saveTab
// @grant        GM_getTabs
// @grant        GM_registerMenuCommand
// @license      MIT
// ==/UserScript==

/*
 * 本机手动启动；不含账户资料、第三方请求、自动更新、付款或后台批量任务。
 * 不记录 Session，不把凭据放入 GM 存储、localStorage、URL 或日志。
 * GM_saveTab 仅保存一次性身份摘要、阶段和短期有效期，以便刷新后校验。
 * 此文件可以被 Node.js require，用于完全离线的模拟测试；不会自动打开网页。
 */
(function () {
  'use strict';

  const VERSION = '0.1.1';
  const UI_ID = 'anagnoraxis-session-restore-ui';
  const ORIGIN = 'https://chatgpt.com';
  const HOST = 'chatgpt.com';
  const SESSION_URL = ORIGIN + '/api/auth/session';
  const BASE = '__Secure-next-auth.session-token';
  const BASES = [BASE, 'next-auth.session-token', '__Secure-authjs.session-token', 'authjs.session-token'];
  const CHUNK_SIZE = 3933;
  const STATE_KEY = 'anagnoraxisSessionRestoreV1';
  const STATE_TTL = 120000;
  const PHASES = ['识别完整 Session', '提取会话与目标身份', '整理 Cookie', '写入并回读', '刷新网页', '核对登录账号'];
  const MESSAGES = Object.freeze({
    INPUT_EMPTY: '请粘贴完整 Session JSON。',
    INPUT_SIZE: '输入过大，已停止；只接受不超过 1 MiB 的 Session。',
    INPUT_JSON: '输入不是完整的 JSON 对象。',
    TOKEN_MISSING: '缺少非空 sessionToken。只有 accessToken 无法完成这里的会话恢复。',
    TOKEN_INVALID: 'sessionToken 含不支持的字符或长度异常；请重新复制完整 Session，不要手工拼接。',
    IDENTITY_MISSING: 'Session 缺少可核对的 user.id 或 user.email，无法确认是否登录到目标账号。',
    IDENTITY_INVALID: 'Session 中的账号身份字段格式异常。',
    EXPIRY_INVALID: 'Session 的 expires 字段无法识别。',
    EXPIRED: '这份 Session 已超过输入记录的有效期，请重新获取。',
    GM_UNAVAILABLE: '当前脚本环境没有所需 GM_cookie 或标签页存储接口。请确认使用 Tampermonkey Beta，并授权此脚本。',
    RUNTIME_UNAVAILABLE: '当前浏览器未提供所需的安全随机数、摘要或请求控制接口。界面已经启动，但不会开始会话恢复。',
    HTTPONLY_UNAVAILABLE: '未通过 HttpOnly Cookie 能力检查。尚未修改登录 Cookie；请检查 Tampermonkey Beta 的 Cookie 访问权限。',
    PROBE_CLEANUP: '测试 Cookie 未能确认清除，已停止。它不包含登录凭据，并会在一分钟内过期。',
    COOKIE_API: 'Cookie 接口报告失败，未把该操作视为成功。',
    COOKIE_TIMEOUT: 'Cookie 操作超时，结果未知。已停止自动操作，请在浏览器中检查当前登录状态。',
    COOKIE_SHAPE: 'Cookie 接口返回了无法识别的数据。',
    COOKIE_SCOPE: '检测到不同路径或分区的会话 Cookie，无法安全地自动替换；未开始导入。',
    COOKIE_COUNT: '现有会话 Cookie 数量异常，已停止自动替换。',
    COOKIE_CLEAR: '无法确认旧会话 Cookie 已清理，已停止后续写入。',
    COOKIE_READBACK: 'Cookie 写后回读不完全一致，尚未进入刷新或登录成功状态。',
    COOKIE_CHANGED: '操作期间会话 Cookie 发生外部变化。已停止，避免覆盖其他标签页的登录。',
    ROLLBACK_FAILED: '替换失败，且无法确认旧会话已完整恢复。请手动检查当前浏览器登录状态。',
    STATE_API: '无法保存刷新后的校验状态；已停止。',
    STATE_INVALID: '刷新后的任务记录无效或已经过期，未自动重试登录。',
    STATE_BUSY: '此浏览器已有会话恢复任务进行中。请完成该任务，或等两分钟后再试。',
    LOCK_UNAVAILABLE: '当前浏览器缺少跨标签页锁能力，已停止自动替换。',
    NETWORK: '无法读取刷新后的会话，登录结果未知。没有自动重试。',
    HTTP: '网站没有正常返回当前会话，登录未得到确认。',
    RESPONSE: '网站返回的内容不是可识别的会话数据，登录未得到确认。',
    NOT_LOGGED_IN: '网站未返回已登录身份。这份会话可能已失效，或仍需网站验证。',
    SESSION_REJECTED: '网站报告会话错误或会话已过期，不能视为登录成功。',
    IDENTITY_MISMATCH: '当前账号与输入 Session 的目标身份不一致，不能视为登录成功。',
    IDENTITY_UNCONFIRMED: '网站没有返回足够的身份字段，无法确认目标账号。',
    UNCERTAIN: '前一次操作结果未知。没有继续写入 Cookie，请手动检查浏览器状态。',
    NAVIGATION: 'Cookie 已写入，但未能发起页面刷新。可手动刷新本页，继续一次性身份核对。',
    UNEXPECTED: '操作没有完成，已停止。未输出任何凭据，请根据当前阶段检查。'
  });

  class FlowError extends Error {
    constructor(code) { super(MESSAGES[code] || MESSAGES.UNEXPECTED); this.code = code; }
  }
  function fail(code) { throw new FlowError(code); }
  function object(value) { return value !== null && typeof value === 'object' && !Array.isArray(value); }
  function textField(value) {
    if (value == null) return '';
    if (typeof value !== 'string' || value.length > 512 || /[\x00-\x1f\x7f]/.test(value)) fail('IDENTITY_INVALID');
    return value.trim();
  }
  function identityOf(session) {
    const user = object(session) && object(session.user) ? session.user : {};
    const account = object(session) && object(session.account) ? session.account : {};
    const identity = {};
    const userId = textField(user.id);
    const accountId = textField(account.id);
    const email = textField(user.email).toLowerCase();
    if (userId) identity.userId = userId;
    if (accountId) identity.accountId = accountId;
    if (email) identity.email = email;
    return identity;
  }
  function parseSession(raw, now = Date.now()) {
    if (typeof raw !== 'string' || !raw.trim()) fail('INPUT_EMPTY');
    if (new TextEncoder().encode(raw).length > 1048576) fail('INPUT_SIZE');
    let source;
    try { source = JSON.parse(raw); } catch { fail('INPUT_JSON'); }
    if (!object(source)) fail('INPUT_JSON');
    const token = source.sessionToken;
    if (typeof token !== 'string' || !token) fail('TOKEN_MISSING');
    // Preserve the exact supplied value. Do not trim, decode, decrypt or re-sign it.
    if (token.length < 40 || token.length > 65536 || !/^[A-Za-z0-9._~-]+$/.test(token)) fail('TOKEN_INVALID');
    const identity = identityOf(source);
    if (!identity.userId && !identity.email) fail('IDENTITY_MISSING');
    if (source.expires != null) {
      if (typeof source.expires !== 'string') fail('EXPIRY_INVALID');
      const expiry = Date.parse(source.expires);
      if (!Number.isFinite(expiry)) fail('EXPIRY_INVALID');
      if (expiry <= now) fail('EXPIRED');
    }
    return { token, identity };
  }
  function buildCookies(model) {
    const count = Math.ceil(model.token.length / CHUNK_SIZE);
    return Array.from({ length: count }, (_, index) => ({
      url: ORIGIN + '/', name: count === 1 ? BASE : BASE + '.' + index,
      value: model.token.slice(index * CHUNK_SIZE, (index + 1) * CHUNK_SIZE),
      domain: '.' + HOST, path: '/', secure: true, httpOnly: true, sameSite: 'lax'
      // Omit expirationDate: these are browser-session cookies. This does not extend server validity.
    }));
  }
  function sessionName(name) {
    return typeof name === 'string' && BASES.some(base => name === base ||
      (name.startsWith(base + '.') && /^\d+$/.test(name.slice(base.length + 1))));
  }
  function domainOf(cookie) { return String(cookie.domain || '').replace(/^\./, '').toLowerCase(); }
  function sessionGroup(cookie) { return object(cookie) && domainOf(cookie) === HOST && sessionName(cookie.name); }
  function cookieKey(cookie) { return [cookie.domain, cookie.path, cookie.name].join('|'); }
  function cookieEqual(expected, actual) {
    if (!actual || actual.name !== expected.name || actual.value !== expected.value ||
      domainOf(actual) !== domainOf(expected) || actual.path !== expected.path ||
      actual.secure !== true || actual.httpOnly !== true || actual.sameSite !== 'lax') return false;
    const hostOnly = expected.hostOnly != null ? expected.hostOnly : !String(expected.domain || '').startsWith('.');
    if (actual.hostOnly !== hostOnly) return false;
    if (expected.expirationDate != null) return Math.abs(actual.expirationDate - expected.expirationDate) <= 1;
    return actual.session === true && actual.expirationDate == null;
  }
  function sameSnapshot(left, right) {
    if (left.length !== right.length) return false;
    const stable = cookie => [cookieKey(cookie), cookie.value, cookie.secure, cookie.httpOnly,
      cookie.hostOnly, cookie.sameSite, cookie.expirationDate != null ? cookie.expirationDate : null, cookie.session].join('\u0000');
    const a = left.map(stable).sort(); const b = right.map(stable).sort();
    return a.every((value, i) => value === b[i]);
  }
  async function readGroup(env) {
    const all = await env.cookies.list({ domain: HOST });
    if (!Array.isArray(all)) fail('COOKIE_SHAPE');
    const selected = all.filter(sessionGroup);
    if (selected.length > 64) fail('COOKIE_COUNT');
    if (selected.some(c => c.path !== '/' || c.partitionKey != null || c.firstPartyDomain)) fail('COOKIE_SCOPE');
    return selected;
  }
  function allowedCookie(cookie, allowed) {
    return allowed.some(candidate =>
      (typeof candidate.hostOnly === 'boolean' && typeof candidate.session === 'boolean')
        ? sameSnapshot([candidate], [cookie]) : cookieEqual(candidate, cookie));
  }
  async function clearGroup(env, allowed) {
    let remaining = await readGroup(env);
    for (let attempt = 0; remaining.length && attempt < 72; attempt++) {
      if (remaining.some(cookie => !allowedCookie(cookie, allowed))) fail('COOKIE_CHANGED');
      const before = remaining;
      // The API does not offer a domain/path selector. Enumerate again after each deletion.
      await env.cookies.delete({ url: ORIGIN + '/', name: remaining[0].name });
      remaining = await readGroup(env);
      if (remaining.some(cookie => !allowedCookie(cookie, allowed))) fail('COOKIE_CHANGED');
      if (sameSnapshot(before, remaining)) fail('COOKIE_CLEAR');
    }
    if (remaining.length) fail('COOKIE_CLEAR');
  }
  function restoreDetails(cookie) {
    const details = { url: ORIGIN + '/', name: cookie.name, value: cookie.value, path: '/',
      secure: cookie.secure, httpOnly: cookie.httpOnly, sameSite: cookie.sameSite };
    if (!cookie.hostOnly) details.domain = cookie.domain;
    if (cookie.expirationDate != null) details.expirationDate = cookie.expirationDate;
    return details;
  }
  async function identityDigests(identity, env) {
    const result = {};
    for (const key of ['userId', 'accountId', 'email']) {
      if (identity[key]) result[key] = await env.hash('anagnoraxis-session-v1\u0000' + key + '\u0000' + identity[key]);
    }
    return result;
  }
  function validPending(state, now) {
    if (!object(state) || state.v !== 1 || typeof state.nonce !== 'string' ||
      !/^[a-zA-Z0-9_-]{12,80}$/.test(state.nonce) || !Number.isFinite(state.created) ||
      !Number.isFinite(state.deadline) || state.created > now + 1000 ||
      state.deadline <= now || state.deadline - state.created !== STATE_TTL ||
      !['preparing', 'awaiting_refresh', 'uncertain'].includes(state.phase) || !object(state.expected)) return false;
    const keys = Object.keys(state.expected);
    return keys.length > 0 && keys.some(k => k === 'userId' || k === 'email') &&
      keys.every(k => ['userId', 'accountId', 'email'].includes(k) && /^[a-f0-9]{64}$/.test(state.expected[k]));
  }
  async function probeHttpOnly(env) {
    const name = '__Host-anagnoraxis-probe-' + env.nonce();
    const value = 'capability-' + env.nonce();
    let okay = false;
    let cleanupOkay = false;
    try {
      await env.cookies.set({ url: ORIGIN + '/', name, value, path: '/', secure: true,
        httpOnly: true, sameSite: 'strict', expirationDate: Math.floor(env.now() / 1000) + 60 });
      const read = await env.cookies.list({ url: ORIGIN + '/', name });
      okay = Array.isArray(read) && read.length === 1 && read[0].name === name &&
        read[0].value === value && read[0].httpOnly === true && read[0].secure === true &&
        read[0].hostOnly === true && read[0].path === '/' && domainOf(read[0]) === HOST;
    } catch { okay = false; }
    finally {
      try {
        await env.cookies.delete({ url: ORIGIN + '/', name });
        const rest = await env.cookies.list({ url: ORIGIN + '/', name });
        cleanupOkay = Array.isArray(rest) && rest.length === 0;
      } catch { cleanupOkay = false; }
    }
    if (!cleanupOkay) fail('PROBE_CLEANUP');
    if (!okay) fail('HTTPONLY_UNAVAILABLE');
  }
  async function writeState(env, state) {
    await env.state.set(state);
    const actual = await env.state.get();
    if (state === null ? actual != null : JSON.stringify(actual) !== JSON.stringify(state)) fail('STATE_API');
  }
  async function rollback(env, snapshot, planned) {
    const current = await readGroup(env);
    // If another actor has installed a different credential, do not overwrite it.
    // A just-written value can have incorrect attributes: that is a reason to roll back.
    // Accept it only as our own exact name/value/scope, then fence the observed attributes
    // throughout clearGroup. Never accept a newly observed credential value.
    if (current.some(cookie => !allowedCookie(cookie, snapshot) && !planned.some(candidate =>
      candidate.name === cookie.name && candidate.value === cookie.value &&
      domainOf(candidate) === domainOf(cookie) && candidate.path === cookie.path))) fail('COOKIE_CHANGED');
    await clearGroup(env, current);
    for (const cookie of snapshot) await env.cookies.set(restoreDetails(cookie));
    if (!sameSnapshot(snapshot, await readGroup(env))) fail('ROLLBACK_FAILED');
  }
  async function prepareFlow(raw, env) {
    env.phase(1, 'running');
    const model = parseSession(raw, env.now());
    raw = '';
    env.phase(1, 'done'); env.phase(2, 'running');
    const expected = await identityDigests(model.identity, env);
    env.phase(2, 'done'); env.phase(3, 'running');
    const planned = buildCookies(model);
    model.token = '';
    model.identity = {};
    env.phase(3, 'done');
    env.phase(4, 'running');
    await probeHttpOnly(env);
    const snapshot = await readGroup(env);
    const created = env.now();
    const state = { v: 1, nonce: env.nonce(), phase: 'preparing', created, deadline: created + STATE_TTL, expected };
    await writeState(env, state);
    let changed = false;
    let readyToRefresh = false;
    try {
      env.phase(4, 'running');
      if (!sameSnapshot(snapshot, await readGroup(env))) fail('COOKIE_CHANGED');
      changed = true;
      await clearGroup(env, snapshot);
      for (const cookie of planned) await env.cookies.set(cookie);
      const actual = await readGroup(env);
      if (actual.length !== planned.length || !planned.every(c => actual.some(a => cookieEqual(c, a)))) fail('COOKIE_READBACK');
      env.phase(4, 'done');
      await writeState(env, { ...state, phase: 'awaiting_refresh' });
      readyToRefresh = true;
      env.phase(5, 'running');
      try { env.navigate(); } catch { fail('NAVIGATION'); }
      return { status: 'awaiting_refresh' };
    } catch (error) {
      if (readyToRefresh) throw error;
      const unknown = error && (error.code === 'COOKIE_TIMEOUT' || error.code === 'COOKIE_CHANGED');
      if (unknown) {
        try { await writeState(env, { ...state, phase: 'uncertain' }); } catch { /* no further cookie writes */ }
        throw error;
      }
      try {
        if (changed) await rollback(env, snapshot, planned);
      } catch (rollbackError) {
        try { await writeState(env, { ...state, phase: 'uncertain' }); } catch { /* preserve unknown outcome */ }
        if (rollbackError && rollbackError.code === 'COOKIE_CHANGED') throw rollbackError;
        fail('ROLLBACK_FAILED');
      }
      try { await writeState(env, null); } catch { fail('STATE_API'); }
      throw error;
    } finally {
      for (const cookie of snapshot.concat(planned)) cookie.value = '';
    }
  }
  async function resumeFlow(env) {
    const state = await env.state.get();
    if (state == null) return { status: 'idle' };
    if (!validPending(state, env.now())) {
      await writeState(env, null);
      fail('STATE_INVALID');
    }
    // Consume once before making a request. A refresh or error never retries automatically.
    await writeState(env, null);
    if (state.phase !== 'awaiting_refresh') fail('UNCERTAIN');
    for (let phase = 1; phase <= 5; phase++) env.phase(phase, 'done');
    env.phase(6, 'running');
    const session = await env.getSession();
    if (!object(session)) fail('RESPONSE');
    if (session.error != null && session.error !== false && session.error !== '') fail('SESSION_REJECTED');
    if (session.expires != null) {
      if (typeof session.expires !== 'string' || !Number.isFinite(Date.parse(session.expires))) fail('RESPONSE');
      if (Date.parse(session.expires) <= env.now()) fail('SESSION_REJECTED');
    }
    let actual;
    try { actual = identityOf(session); } catch { fail('RESPONSE'); }
    if (!actual.userId && !actual.email) fail('NOT_LOGGED_IN');
    const digests = await identityDigests(actual, env);
    for (const key of Object.keys(state.expected)) {
      if (!digests[key]) fail('IDENTITY_UNCONFIRMED');
      if (digests[key] !== state.expected[key]) fail('IDENTITY_MISMATCH');
    }
    env.phase(6, 'done');
    return { status: 'verified', email: actual.email || '' };
  }

  // Wrappers never surface raw extension errors: they can contain Cookie values.
  function callbackCall(invoke, kind, code, timeout = 15000) {
    return new Promise((resolve, reject) => {
      let settled = false;
      const timer = setTimeout(() => finish(false, new FlowError(kind === 'cookie' ? 'COOKIE_TIMEOUT' : code)), timeout);
      function finish(ok, value) {
        if (settled) return;
        settled = true; clearTimeout(timer);
        if (ok) resolve(value); else reject(value);
      }
      try { invoke((value, error) => error ? finish(false, new FlowError(code)) : finish(true, value)); }
      catch { finish(false, new FlowError(code)); }
    });
  }
  function gmCookieAdapter(gm) {
    if (!gm || !['list', 'set', 'delete'].every(key => typeof gm[key] === 'function')) fail('GM_UNAVAILABLE');
    return {
      list: details => callbackCall(cb => gm.list(details, (cookies, error) => cb(cookies, error)), 'cookie', 'COOKIE_API'),
      set: details => callbackCall(cb => gm.set(details, error => cb(undefined, error)), 'cookie', 'COOKIE_API'),
      delete: details => callbackCall(cb => gm.delete(details, error => cb(undefined, error)), 'cookie', 'COOKIE_API')
    };
  }
  function gmStateAdapter(getTab, saveTab, getTabs) {
    if (![getTab, saveTab, getTabs].every(fn => typeof fn === 'function')) fail('GM_UNAVAILABLE');
    const get = () => callbackCall(cb => getTab(tab => cb(tab)), 'state', 'STATE_API');
    return {
      get: async () => {
        const tab = await get();
        return object(tab) && tab[STATE_KEY] != null ? tab[STATE_KEY] : null;
      },
      set: async value => {
        const tab = await get();
        if (!object(tab)) fail('STATE_API');
        if (value === null) delete tab[STATE_KEY]; else tab[STATE_KEY] = value;
        await callbackCall(cb => saveTab(tab, () => cb()), 'state', 'STATE_API');
      },
      busy: async now => {
        const tabs = await callbackCall(cb => getTabs(value => cb(value)), 'state', 'STATE_API');
        if (!object(tabs)) fail('STATE_API');
        return Object.values(tabs).some(tab => validPending(object(tab) ? tab[STATE_KEY] : null, now));
      }
    };
  }
  async function fetchCurrentSession(fetcher) {
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), 10000);
    try {
      let response;
      try {
        response = await fetcher(SESSION_URL, { method: 'GET', credentials: 'same-origin', cache: 'no-store',
          redirect: 'error', headers: { Accept: 'application/json' }, signal: controller.signal });
      } catch { fail('NETWORK'); }
      if (!response.ok) fail('HTTP');
      if (!String(response.headers.get('content-type') || '').toLowerCase().includes('application/json')) fail('RESPONSE');
      let result;
      try {
        const text = await response.text();
        if (text.length > 1048576) fail('RESPONSE');
        result = JSON.parse(text);
      } catch { fail('RESPONSE'); }
      if (!object(result)) fail('RESPONSE');
      return result;
    } finally { clearTimeout(timer); }
  }
  function safeError(error) { return { code: error instanceof FlowError ? error.code : 'UNEXPECTED', message: error instanceof FlowError ? error.message : MESSAGES.UNEXPECTED }; }

  const exported = { VERSION, ORIGIN, HOST, SESSION_URL, BASE, BASES, STATE_TTL, PHASES, FlowError, MESSAGES,
    parseSession, buildCookies, identityOf, sessionName, sessionGroup, cookieEqual, sameSnapshot,
    readGroup, probeHttpOnly, validPending, identityDigests, prepareFlow, resumeFlow,
    gmCookieAdapter, gmStateAdapter, fetchCurrentSession, safeError };
  // Export only in a DOM-free test process. A webpage can define module.exports;
  // it must neither suppress this UI nor receive the privileged flow helpers.
  if (typeof window === 'undefined' && typeof document === 'undefined') {
    if (typeof module === 'object' && module && module.exports) module.exports = exported;
    return;
  }
  if (typeof window === 'undefined' || window.top !== window.self || location.origin !== ORIGIN) return;

  function mount() {
    if (document.getElementById(UI_ID)) return;
    const host = document.createElement('div');
    host.id = UI_ID;
    host.setAttribute('data-version', VERSION);
    host.style.cssText = 'position:fixed;right:18px;bottom:18px;z-index:2147483000;';
    const root = host.attachShadow({ mode: 'closed' });
    const style = document.createElement('style');
    style.textContent = `:host{all:initial;font:14px/1.6 system-ui,-apple-system,sans-serif;color:#16251f}*{box-sizing:border-box}button,textarea{font:inherit}button{cursor:pointer;border:0;border-radius:9px;padding:10px 14px;font-weight:650}.toggle,.start{background:#176647;color:white}.panel{display:none;width:min(460px,calc(100vw - 36px));max-height:calc(100vh - 50px);overflow:auto;background:#fafcfb;border:1px solid #cbded3;border-radius:15px;padding:20px;box-shadow:0 12px 50px #0003}.panel.open{display:block}.top{display:flex;align-items:center;justify-content:space-between;gap:16px}h2{font-size:18px;margin:0}.close{background:#e9efec;padding:5px 10px}.note{font-size:12px;color:#556c60;margin:12px 0}textarea{width:100%;height:160px;padding:10px;border:1px solid #b8c9c0;border-radius:8px;resize:vertical;background:white;color:#17251f}textarea:focus{outline:2px solid #267856;outline-offset:1px}.actions{display:flex;gap:8px;margin-top:10px}.clear{background:#e9efec}.start{flex:1}button:disabled{opacity:.5;cursor:default}ol{margin:16px 0 8px;padding-left:26px;font-size:13px}li{padding:3px 0;color:#64766b}li[data-state=running]{color:#99690a;font-weight:700}li[data-state=done]{color:#176647}li[data-state=error]{color:#a52b29}.status{font-size:13px;white-space:pre-wrap;overflow-wrap:anywhere;margin-top:12px;padding:10px;background:#eef4f0;border-radius:8px}.status.error{background:#fff0ed;color:#8d2825}.status.success{background:#e2f3e8;color:#145032}.footer{font-size:11px;color:#65756b;margin:10px 0 0}`;
    root.append(style);
    const toggle = document.createElement('button'); toggle.className = 'toggle'; toggle.textContent = '会话恢复';
    const panel = document.createElement('section'); panel.className = 'panel'; panel.setAttribute('aria-label', '会话恢复');
    const top = document.createElement('div'); top.className = 'top';
    const heading = document.createElement('h2'); heading.textContent = '会话恢复 · v' + VERSION;
    const close = document.createElement('button'); close.className = 'close'; close.textContent = '收起';
    top.append(heading, close);
    const note = document.createElement('p'); note.className = 'note';
    note.textContent = '需要 Tampermonkey Beta 及 HttpOnly Cookie 权限。此操作会替换当前浏览器配置的 ChatGPT 登录，影响同一配置中的其他标签页；新标签页不代表账号隔离。';
    const label = document.createElement('label'); label.textContent = '完整 Session JSON';
    const input = document.createElement('textarea'); input.placeholder = '在这里粘贴完整 Session，包含 sessionToken 和账号身份字段。';
    input.setAttribute('aria-label', '完整 Session JSON'); input.autocomplete = 'off'; input.spellcheck = false; input.setAttribute('autocapitalize', 'off');
    label.append(input);
    const actions = document.createElement('div'); actions.className = 'actions';
    const start = document.createElement('button'); start.className = 'start'; start.textContent = '替换登录并验证';
    const clear = document.createElement('button'); clear.className = 'clear'; clear.textContent = '清空'; actions.append(start, clear);
    const list = document.createElement('ol'); const rows = PHASES.map(name => { const row = document.createElement('li'); row.textContent = name; list.append(row); return row; });
    const status = document.createElement('div'); status.className = 'status'; status.setAttribute('role', 'status'); status.textContent = '脚本界面已运行。等待你粘贴完整 Session。';
    const footer = document.createElement('p'); footer.className = 'footer'; footer.textContent = '仅手动启动；不保存输入、不连接第三方服务、不付款。刷新后核对实际账号，才报告登录成功。';
    panel.append(top, note, label, actions, list, status, footer); root.append(panel, toggle); document.documentElement.append(host);
    let busy = false; let currentPhase = 0;
    function open() { panel.classList.add('open'); toggle.hidden = true; }
    function setBusy(value) { busy = value; start.disabled = value; clear.disabled = value; input.disabled = value; }
    function phase(number, state) { currentPhase = number; rows[number - 1].dataset.state = state; }
    function report(error) {
      const safe = safeError(error); status.className = 'status error'; status.textContent = safe.message + '\n错误代码：' + safe.code;
      if (currentPhase) rows[currentPhase - 1].dataset.state = 'error';
      open();
    }
    toggle.addEventListener('click', open);
    close.addEventListener('click', () => { input.value = ''; panel.classList.remove('open'); toggle.hidden = false; });
    clear.addEventListener('click', () => { input.value = ''; input.focus(); });
    window.addEventListener('pagehide', () => { input.value = ''; });
    // UI is visible even when Cookie access is unavailable. Menu registration is
    // optional and must not prevent the page button or diagnostics from working.
    open();
    if (typeof GM_registerMenuCommand === 'function') {
      try { GM_registerMenuCommand('打开会话恢复面板 · v' + VERSION, open); }
      catch { /* The visible page panel remains available. */ }
    }
    let env;
    try {
      if (typeof crypto === 'undefined' || !crypto.subtle || typeof crypto.getRandomValues !== 'function' ||
        typeof TextEncoder === 'undefined' || typeof AbortController === 'undefined') fail('RUNTIME_UNAVAILABLE');
      env = {
        cookies: gmCookieAdapter(typeof GM_cookie === 'undefined' ? null : GM_cookie),
        state: gmStateAdapter(typeof GM_getTab === 'undefined' ? null : GM_getTab,
          typeof GM_saveTab === 'undefined' ? null : GM_saveTab, typeof GM_getTabs === 'undefined' ? null : GM_getTabs),
        now: () => Date.now(), nonce: () => Array.from(crypto.getRandomValues(new Uint8Array(16)),
          byte => byte.toString(16).padStart(2, '0')).join(''), phase,
        hash: async value => Array.from(new Uint8Array(await crypto.subtle.digest('SHA-256', new TextEncoder().encode(value))), byte => byte.toString(16).padStart(2, '0')).join(''),
        navigate: () => { status.textContent = '写入回读通过，正在刷新；刷新后会继续第六步。'; location.replace(ORIGIN + '/'); },
        getSession: () => fetchCurrentSession(fetch.bind(window))
      };
    } catch (error) { report(error); start.disabled = true; return; }
    start.addEventListener('click', async () => {
      if (busy) return;
      let raw = input.value; input.value = ''; setBusy(true);
      for (const row of rows) delete row.dataset.state;
      status.className = 'status'; status.textContent = '正在检查输入及 Cookie 权限。';
      try {
        if (!navigator.locks || typeof navigator.locks.request !== 'function') fail('LOCK_UNAVAILABLE');
        await navigator.locks.request('anagnoraxis-session-restore-v1', { ifAvailable: true }, async lock => {
          if (!lock || await env.state.busy(env.now())) fail('STATE_BUSY');
          await prepareFlow(raw, env);
        });
      } catch (error) { report(error); setBusy(false); }
      finally { raw = ''; }
    });
    // This continuation never writes credentials. It checks only a short-lived pending task.
    (async () => {
      setBusy(true);
      try {
        const pending = await env.state.get();
        if (pending) { open(); status.textContent = '页面已刷新，正在核对实际登录账号。'; }
        let result;
        if (pending) {
          if (!navigator.locks || typeof navigator.locks.request !== 'function') fail('LOCK_UNAVAILABLE');
          result = await navigator.locks.request('anagnoraxis-session-restore-v1', { ifAvailable: true }, async lock => {
            if (!lock) fail('STATE_BUSY');
            return resumeFlow(env);
          });
        } else result = { status: 'idle' };
        if (result.status === 'verified') {
          status.className = 'status success';
          status.textContent = '登录成功：网站返回的账号身份与输入 Session 一致。' + (result.email ? '\n当前邮箱：' + result.email : '');
          open();
        }
      } catch (error) { report(error); }
      finally { setBusy(false); }
    })();
  }
  function boot() {
    try { mount(); }
    catch {
      // Report only a fixed diagnostic. Raw errors may include sensitive values.
      // Parse errors occur before execution and cannot be caught by this handler.
      const existing = document.getElementById(UI_ID);
      if (existing) existing.remove();
      const notice = document.createElement('div');
      notice.id = UI_ID;
      notice.setAttribute('role', 'alert');
      notice.style.cssText = 'position:fixed;right:18px;bottom:18px;max-width:380px;padding:16px;z-index:2147483000;background:#fff0ed;color:#8d2825;border:2px solid #a52b29;font:14px/1.6 system-ui;';
      notice.textContent = '会话恢复 v' + VERSION + '：界面初始化未完成（UI_BOOT_FAILED）。尚未启动会话恢复。请完整替换脚本后重试；不要粘贴凭据到报错信息。';
      document.documentElement.append(notice);
    }
  }
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', boot, { once: true });
  else boot();
})();

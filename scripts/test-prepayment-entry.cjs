'use strict';

// Execute the actual embedded source in a VM with only synthetic fetch results.
// The context has no require/process/native fetch, browser profile or credential.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const test = require('node:test');
const vm = require('node:vm');
const source = fs.readFileSync(path.join(__dirname, '../internal/checkout/prepayment_entry.js'), 'utf8');
const accountID = 'synthetic-private-account';
const userID = 'synthetic-private-user';
const email = 'fixture-owner@example.invalid';
const secret = 'synthetic-private-access-token';
const identity = {Email: email, UserID: userID};
const lookupURL = '/backend-api/accounts/check/v4-2023-04-27?timezone_offset_min=-540';
const diagnosticKeys = [
  'phase', 'reason', 'source', 'sessionHttpStatus', 'accountHttpStatus', 'subscriptionHttpStatus',
  'checkoutHttpStatus', 'accountIdPresent', 'accountBinding', 'plan', 'activeSubscription', 'checkoutRequested'
].sort();

function record(account = {}, entitlement = {has_active_subscription: false}) {
  return {account: {account_id: accountID, plan_type: 'free', structure: 'personal', user_id: userID, ...account}, entitlement};
}

function accountData(value = record()) {
  return {default_account: 'personal', accounts: {personal: value}};
}

function token(claims) {
  return 'synthetic.' + Buffer.from(JSON.stringify(claims)).toString('base64url') + '.not-a-valid-signature';
}

async function run(fixture = {}) {
  const calls = {session: 0, account: 0, subscription: 0, checkout: 0, unexpected: 0};
  const requests = [];
  const session = fixture.session ?? {user: {id: userID, email}, account: {id: accountID}, accessToken: secret};
  const value = (key, fallback) => Object.prototype.hasOwnProperty.call(fixture, key) ? fixture[key] : fallback;
  const response = (body, status, invalid) => ({
    status, ok: status >= 200 && status < 300,
    json: async () => { if (invalid) throw new Error('private response body ' + secret); return body; }
  });
  const fetchDouble = async (url, options = {}) => {
    requests.push({url, method: options.method, headers: options.headers, body: options.body});
    let name;
    if (url === '/api/auth/session' && options.method === 'GET') name = 'session';
    else if (url === lookupURL && options.method === 'GET') name = 'account';
    else if (url.startsWith('/backend-api/subscriptions?account_id=') && options.method === 'GET') name = 'subscription';
    else if (url === '/backend-api/payments/checkout' && options.method === 'POST') name = 'checkout';
    else { calls.unexpected++; throw new Error('unexpected fixture request'); }
    calls[name]++;
    if (fixture[name + 'Failure'] === 'network') throw new Error('private network detail ' + secret);
    if (fixture[name + 'Failure'] === 'timeout') {
      return new Promise((resolve, reject) => {
        if (options.signal.aborted) reject(new Error('private abort detail'));
        else options.signal.addEventListener('abort', () => reject(new Error('private abort detail')), {once: true});
      });
    }
    const bodies = {
      session,
      account: value('accounts', accountData()),
      subscription: value('subscription', {plan_type: 'free'}),
      checkout: value('checkout', {tag: 'hosted_checkout_session', url: 'https://pay.openai.com/pay/cs_synthetic_private'})
    };
    return response(bodies[name], fixture[name + 'Status'] ?? 200, fixture[name + 'InvalidJSON']);
  };
  const context = vm.createContext({
    location: {origin: fixture.origin ?? 'https://chatgpt.com'},
    fetch: fetchDouble,
    atob: encoded => Buffer.from(encoded, 'base64').toString('binary'),
    AbortController,
    setTimeout: (fn, ms) => setTimeout(fn, Object.values(fixture).includes('timeout') ? 5 : ms),
    clearTimeout
  });
  const expression = source.replace('__PREPAYMENT_IDENTITY__', JSON.stringify(fixture.identity ?? identity));
  const raw = await vm.runInContext(expression, context, {timeout: 1000});
  const result = JSON.parse(JSON.stringify(raw));
  assert.equal(calls.unexpected, 0);
  assert.deepEqual(Object.keys(result.diagnostic).sort(), diagnosticKeys);
  return {result, diagnostic: result.diagnostic, calls, requests};
}

function stopped(output, reason, kind = 'eligibility_unconfirmed') {
  assert.equal(output.result.errorKind, kind);
  assert.equal(output.diagnostic.reason, reason);
  assert.equal(output.diagnostic.checkoutRequested, false);
  assert.equal(output.calls.checkout, 0);
}

test('source has one identity slot and no exported or native network dependency', () => {
  assert.equal((source.match(/__PREPAYMENT_IDENTITY__/g) || []).length, 1);
  assert.equal(source.includes('module.exports'), false);
  assert.equal(source.includes('require('), false);
});

test('explicit personal free entitlement reaches one unchanged Plus checkout request', async () => {
  const out = await run();
  assert.equal(out.result.errorKind, '');
  assert.deepEqual(out.calls, {session: 1, account: 1, subscription: 0, checkout: 1, unexpected: 0});
  assert.equal(out.diagnostic.phase, 'complete');
  assert.equal(out.diagnostic.reason, 'checkout_created');
  assert.equal(out.diagnostic.source, 'account_entitlement');
  assert.equal(out.diagnostic.accountBinding, 'matched');
  assert.equal(out.diagnostic.plan, 'free');
  assert.equal(out.diagnostic.activeSubscription, 'false');
  assert.equal(out.diagnostic.accountIdPresent, true);
  assert.equal(out.diagnostic.checkoutRequested, true);
  assert.equal(out.diagnostic.checkoutHttpStatus, 200);
  assert.deepEqual(JSON.parse(out.requests.at(-1).body), {
    billing_details: {country: 'PH', currency: 'PHP'}, checkout_ui_mode: 'hosted',
    entry_point: 'all_plans_pricing_modal', plan_name: 'chatgptplusplan'
  });
  for (const request of out.requests) {
    assert.equal(Object.keys(request.headers).some(key => /user-agent|sec-ch|fingerprint/i.test(key)), false);
  }
});

test('plan and personal structure normalize casing and outer whitespace', async () => {
  const out = await run({accounts: accountData(record({plan_type: ' FREE ', structure: ' Personal '}))});
  assert.equal(out.calls.checkout, 1);
  assert.equal(out.diagnostic.plan, 'free');
});

test('explicit free plan can retain unknown active-subscription status', async () => {
  const out = await run({accounts: accountData(record({}, {}))});
  assert.equal(out.calls.checkout, 1);
  assert.equal(out.diagnostic.activeSubscription, 'unknown');
});

test('explicit inactive entitlement supports missing, null or empty account plan', async () => {
  for (const plan of [undefined, null, '', '  ']) {
    const out = await run({accounts: accountData(record({plan_type: plan}))});
    assert.equal(out.calls.checkout, 1);
    assert.equal(out.diagnostic.plan, 'free');
  }
});

test('missing, unknown or nonstring plan never becomes free without compatible evidence', async () => {
  for (const value of [record({plan_type: undefined}, {}), record({plan_type: 'future-plan'}), record({plan_type: 0}), record({plan_type: []})]) {
    stopped(await run({accounts: accountData(value)}), 'plan_unknown');
  }
});

test('every recognized paid plan and active entitlement stop before creation', async () => {
  for (const plan of ['go', 'plus', 'prolite', 'pro', 'team', 'business', 'enterprise', 'edu']) {
    stopped(await run({accounts: accountData(record({plan_type: plan}))}), 'already_subscribed', 'already_subscribed');
  }
  const out = await run({accounts: accountData(record({plan_type: 'free'}, {has_active_subscription: true}))});
  stopped(out, 'already_subscribed', 'already_subscribed');
  assert.equal(out.diagnostic.activeSubscription, 'true');
  assert.equal(out.diagnostic.plan, 'paid');
});

test('invalid active-subscription types and invalid entitlement objects are rejected', async () => {
  for (const active of ['false', 0, null, [], {}]) {
    stopped(await run({accounts: accountData(record({}, {has_active_subscription: active}))}), 'entitlement_invalid');
  }
  for (const entitlement of [[], false]) {
    stopped(await run({accounts: accountData(record({}, entitlement))}), 'entitlement_invalid');
  }
});

test('nullable optional entitlement and structure do not invent free eligibility', async () => {
  const out = await run({accounts: accountData(record({structure: null}, null))});
  assert.equal(out.calls.checkout, 1);
  assert.equal(out.diagnostic.activeSubscription, 'unknown');
  stopped(await run({accounts: accountData(record({plan_type: null, structure: null}, null))}), 'plan_unknown');
});

test('explicit default record supplies a real account ID when session lacks one', async () => {
  const out = await run({session: {user: {id: userID, email}, accessToken: secret}});
  assert.equal(out.calls.checkout, 1);
  assert.equal(out.diagnostic.accountBinding, 'default');
  assert.equal(out.diagnostic.accountIdPresent, true);
});

test('missing default never chooses the first account even when only one exists', async () => {
  const accounts = accountData(); delete accounts.default_account;
  stopped(await run({session: {user: {id: userID, email}, accessToken: secret}, accounts}), 'account_binding_missing');
});

test('already bound personal account is selected uniquely among multiple workspaces', async () => {
  const accounts = {accounts: {
    other: record({account_id: 'synthetic-other-workspace', plan_type: 'team', structure: 'team'}),
    personal: record()
  }};
  const out = await run({accounts});
  assert.equal(out.calls.checkout, 1);
  assert.equal(out.diagnostic.accountBinding, 'matched');
});

test('default workspace differing from session binding is rejected', async () => {
  const accounts = {default_account: 'other', accounts: {
    personal: record(), other: record({account_id: 'synthetic-other-account', plan_type: 'team'})
  }};
  stopped(await run({accounts}), 'account_identity_mismatch', 'identity_changed');
});

test('duplicate real account identifiers are ambiguous', async () => {
  stopped(await run({accounts: {default_account: 'personal', accounts: {personal: record(), duplicate: record()}}}), 'account_ambiguous');
});

test('session and JWT account conflicts stop before account lookup', async () => {
  const out = await run({session: {
    user: {id: userID, email}, account: {id: accountID},
    accessToken: token({'https://api.openai.com/auth': {chatgpt_account_id: 'synthetic-other-account'}})
  }});
  stopped(out, 'account_binding_conflict');
  assert.equal(out.calls.account, 0);
});

test('JWT account ID can bind lookup while JWT free plan never grants eligibility', async () => {
  const session = {user: {id: userID, email}, accessToken: token({
    'https://api.openai.com/auth': {chatgpt_account_id: accountID, chatgpt_plan_type: 'free'}
  })};
  const accepted = await run({session});
  assert.equal(accepted.diagnostic.accountBinding, 'matched');
  assert.equal(accepted.calls.checkout, 1);
  stopped(await run({session, accounts: accountData(record({plan_type: undefined}, {}))}), 'plan_unknown');
});

test('session account plan does not overrule unknown live entitlement', async () => {
  stopped(await run({session: {user: {id: userID, email}, account: {id: accountID, planType: 'free'}, accessToken: secret},
    accounts: accountData(record({plan_type: undefined}, {}))}), 'plan_unknown');
});

test('conflicting account identifiers in a record stop before checkout', async () => {
  stopped(await run({accounts: accountData(record({id: 'synthetic-other-account'}))}), 'account_identity_mismatch', 'identity_changed');
});

test('missing bound account and invalid default key are distinguished', async () => {
  stopped(await run({accounts: {accounts: {other: record({account_id: 'synthetic-other-account'})}}}), 'account_not_found');
  stopped(await run({accounts: {...accountData(), default_account: 'absent'}}), 'account_default_invalid');
  stopped(await run({accounts: {...accountData(), default_account: {id: accountID}}}), 'account_default_invalid');
});

test('account record user mismatch stops even with matching email login', async () => {
  stopped(await run({accounts: accountData(record({user_id: 'synthetic-other-user'}))}), 'account_identity_mismatch', 'identity_changed');
});

test('nonpersonal or invalid present structure is not silently switched', async () => {
  for (const structure of ['team', 'business', '', false]) {
    stopped(await run({accounts: accountData(record({structure}))}), 'workspace_unsupported');
  }
});

test('malformed account envelopes, entries, IDs and JSON fail closed', async () => {
  for (const accounts of [null, [], {}, {accounts: []}, {accounts: {}},
    {accounts: {personal: null}}, {accounts: {personal: {account: null}}},
    accountData(record({account_id: undefined})), accountData(record({account_id: 123}))]) {
    stopped(await run({accounts}), 'account_response_invalid');
  }
  stopped(await run({accountInvalidJSON: true}), 'account_response_invalid');
});

test('account HTTP denial or server error never invokes subscription fallback', async () => {
  for (const accountStatus of [400, 401, 403, 429, 500, 503]) {
    const out = await run({accountStatus});
    stopped(out, 'account_http_rejected');
    assert.equal(out.diagnostic.accountHttpStatus, accountStatus);
    assert.equal(out.calls.subscription, 0);
  }
});

test('only account 404 or 405 with existing binding permits legacy explicit-free lookup', async () => {
  for (const accountStatus of [404, 405]) {
    const out = await run({accountStatus});
    assert.equal(out.calls.checkout, 1);
    assert.equal(out.calls.subscription, 1);
    assert.equal(out.diagnostic.source, 'subscription');
    assert.equal(out.diagnostic.accountBinding, 'session');
    assert.equal(out.diagnostic.accountHttpStatus, accountStatus);
    assert.equal(out.diagnostic.subscriptionHttpStatus, 200);
    assert.equal(out.requests.find(r => r.url.startsWith('/backend-api/subscriptions?')).url,
      '/backend-api/subscriptions?account_id=' + encodeURIComponent(accountID));
  }
});

test('unsupported account endpoint without a bound ID cannot use legacy fallback', async () => {
  const out = await run({accountStatus: 404, session: {user: {id: userID, email}, accessToken: secret}});
  stopped(out, 'account_binding_missing');
  assert.equal(out.calls.subscription, 0);
});

test('legacy response must explicitly say free and HTTP denial never implies free', async () => {
  stopped(await run({accountStatus: 404, subscription: {}}), 'subscription_plan_missing');
  stopped(await run({accountStatus: 404, subscription: {plan_type: 'future-plan'}}), 'plan_unknown');
  stopped(await run({accountStatus: 404, subscription: {plan_type: 'plus'}}), 'already_subscribed', 'already_subscribed');
  stopped(await run({accountStatus: 404, subscriptionStatus: 403}), 'subscription_http_rejected');
  stopped(await run({accountStatus: 404, subscriptionInvalidJSON: true}), 'subscription_response_invalid');
});

test('legacy active-subscription fact cannot be overruled by a free plan label', async () => {
  const out = await run({accountStatus: 404, subscription: {plan_type: 'free', has_active_subscription: true}});
  stopped(out, 'already_subscribed', 'already_subscribed');
  assert.equal(out.diagnostic.activeSubscription, 'true');
  assert.equal(out.diagnostic.plan, 'paid');
  for (const active of [null, 'false', 0, []]) {
    stopped(await run({accountStatus: 404, subscription: {plan_type: 'free', has_active_subscription: active}}),
      'subscription_response_invalid');
  }
  const inactive = await run({accountStatus: 405, subscription: {plan_type: 'free', has_active_subscription: false}});
  assert.equal(inactive.calls.checkout, 1);
  assert.equal(inactive.diagnostic.activeSubscription, 'false');
});

test('legacy response explicit account and user identities must match the bound account', async () => {
  for (const subscription of [
    {plan_type: 'free', account_id: 'synthetic-other-account'},
    {plan_type: 'free', user_id: 'synthetic-other-user'},
    {plan_type: 'free', account_id: accountID, account: {id: 'synthetic-other-account'}}
  ]) {
    stopped(await run({accountStatus: 404, subscription}), 'account_identity_mismatch', 'identity_changed');
  }
});

test('account network error and timeout cause neither fallback nor checkout', async () => {
  for (const accountFailure of ['network', 'timeout']) {
    const out = await run({accountFailure});
    stopped(out, accountFailure === 'timeout' ? 'account_timeout' : 'account_network_error');
    assert.equal(out.calls.subscription, 0);
  }
});

test('legacy network error and timeout stop without checkout', async () => {
  for (const subscriptionFailure of ['network', 'timeout']) {
    stopped(await run({accountStatus: 404, subscriptionFailure}),
      subscriptionFailure === 'timeout' ? 'subscription_timeout' : 'subscription_network_error');
  }
});

test('session identity, access-token and HTTP failures stop before account lookup', async () => {
  const variants = [
    [{session: {user: {id: userID, email: 'wrong@example.invalid'}, accessToken: secret}}, 'identity_changed'],
    [{session: {user: {id: userID, email}}}, 'access_token_missing'],
    [{sessionStatus: 401}, 'session_http_rejected'],
    [{sessionInvalidJSON: true}, 'session_response_invalid'],
    [{sessionFailure: 'network'}, 'session_network_error'],
    [{sessionFailure: 'timeout'}, 'session_timeout']
  ];
  for (const [fixture, reason] of variants) {
    const out = await run(fixture);
    assert.equal(out.diagnostic.reason, reason);
    assert.equal(out.calls.account, 0);
    assert.equal(out.calls.checkout, 0);
  }
});

test('a lost checkout response is never automatically submitted again', async () => {
  for (const checkoutFailure of ['network', 'timeout']) {
    const out = await run({checkoutFailure});
    assert.equal(out.result.errorKind, 'checkout_unavailable');
    assert.equal(out.diagnostic.reason, checkoutFailure === 'timeout' ? 'checkout_timeout' : 'checkout_network_error');
    assert.equal(out.diagnostic.checkoutRequested, true);
    assert.equal(out.calls.checkout, 1);
  }
});

test('checkout HTTP and JSON failures preserve safe request facts', async () => {
  const denied = await run({checkoutStatus: 403});
  assert.equal(denied.result.errorKind, 'checkout_unauthorized');
  assert.equal(denied.diagnostic.checkoutHttpStatus, 403);
  assert.equal(denied.calls.checkout, 1);
  const paid = await run({checkoutStatus: 400, checkout: {detail: 'already paid private ' + secret}});
  assert.equal(paid.result.errorKind, 'already_subscribed');
  assert.equal(paid.diagnostic.plan, 'paid');
  assert.equal(paid.diagnostic.activeSubscription, 'unknown');
  const invalid = await run({checkoutInvalidJSON: true});
  assert.equal(invalid.diagnostic.reason, 'checkout_response_invalid');
  assert.equal(invalid.diagnostic.checkoutRequested, true);
  assert.equal(invalid.calls.checkout, 1);
});

test('only fixed diagnostic facts leave the script on success or failure', async () => {
  for (const fixture of [{}, {accountFailure: 'network'}, {checkoutStatus: 400, checkout: {detail: secret}},
    {accounts: accountData(record({user_id: 'synthetic-other-user'}))}]) {
    const out = await run(fixture);
    const encoded = JSON.stringify(out.diagnostic);
    for (const forbidden of [accountID, userID, email, secret, 'https://', 'Bearer', 'synthetic-other-user']) {
      assert.equal(encoded.includes(forbidden), false);
    }
    for (const key of ['sessionHttpStatus', 'accountHttpStatus', 'subscriptionHttpStatus', 'checkoutHttpStatus']) {
      assert.equal(Number.isInteger(out.diagnostic[key]), true);
      assert.equal(out.diagnostic[key] === 0 || out.diagnostic[key] >= 100 && out.diagnostic[key] <= 599, true);
    }
  }
});

test('foreign document origin performs no request', async () => {
  const out = await run({origin: 'https://foreign.invalid'});
  assert.equal(out.result.errorKind, 'context_invalid');
  assert.deepEqual(out.calls, {session: 0, account: 0, subscription: 0, checkout: 0, unexpected: 0});
});

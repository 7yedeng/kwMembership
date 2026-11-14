(async () => {
  const target = __PREPAYMENT_IDENTITY__;
  const diagnostic = {
    phase: 'session', reason: 'none', source: 'none',
    sessionHttpStatus: 0, accountHttpStatus: 0, subscriptionHttpStatus: 0, checkoutHttpStatus: 0,
    accountIdPresent: false, accountBinding: 'none', plan: 'unknown',
    activeSubscription: 'unknown', checkoutRequested: false
  };
  const result = (errorKind, reason, httpStatus = 0, values = {}) => {
    diagnostic.reason = reason;
    return {
      responseTag: '', checkoutURL: '', processorEntity: '', checkoutSessionID: '',
      errorKind, httpStatus, ...values, diagnostic: {...diagnostic}
    };
  };
  const object = value => value !== null && typeof value === 'object' && !Array.isArray(value);
  const has = (value, key) => Object.prototype.hasOwnProperty.call(value, key);
  const normalize = value => typeof value === 'string' ? value.trim().toLowerCase() : '';
  const status = response => Number.isInteger(response?.status) && response.status >= 100 && response.status <= 599 ? response.status : 0;
  const paidPlans = new Set(['go', 'plus', 'prolite', 'pro_lite', 'pro', 'team', 'business', 'enterprise', 'edu']);
  const ids = values => {
    const found = new Set();
    for (const value of values) {
      if (value === undefined || value === null) continue;
      if (typeof value !== 'string') return {invalid: true, values: []};
      const text = value.trim();
      if (!text) continue;
      if (text.length > 512 || /[\s\u0000-\u001f\u007f]/.test(text)) return {invalid: true, values: []};
      found.add(text);
    }
    return {invalid: false, values: [...found]};
  };
  // Every response and timeout stays inside this document. Never return a body,
  // credential, account identifier, server message or exception in diagnostics.
  const read = async (url, options, inspectRejectedBody = false) => {
    const controller = new AbortController();
    let timedOut = false;
    let httpStatus = 0;
    const timer = setTimeout(() => { timedOut = true; controller.abort(); }, 15000);
    try {
      const response = await fetch(url, {...options, signal: controller.signal});
      httpStatus = status(response);
      const ok = response.ok === true && httpStatus >= 200 && httpStatus < 300;
      if (!ok && !inspectRejectedBody) return {ok, httpStatus, valid: false};
      try {
        const value = await response.json();
        return {ok, httpStatus, valid: true, value};
      } catch {
        return timedOut ? {failure: 'timeout', httpStatus} : {ok, httpStatus, valid: false};
      }
    } catch {
      return {failure: timedOut ? 'timeout' : 'network_error', httpStatus};
    } finally {
      clearTimeout(timer);
    }
  };
  const get = headers => ({method: 'GET', credentials: 'include', cache: 'no-store', redirect: 'error', headers});
  if (location.origin !== 'https://chatgpt.com') return result('context_invalid', 'context_invalid');
  try {
    const sessionRead = await read('/api/auth/session', get({}));
    diagnostic.sessionHttpStatus = sessionRead.httpStatus;
    if (sessionRead.failure) return result('session_unavailable', 'session_' + sessionRead.failure, sessionRead.httpStatus);
    if (!sessionRead.ok) return result('session_unavailable', 'session_http_rejected', sessionRead.httpStatus);
    if (!sessionRead.valid || !object(sessionRead.value)) return result('session_unavailable', 'session_response_invalid', sessionRead.httpStatus);
    const session = sessionRead.value;
    if (!object(session.user) || normalize(session.user.email) !== normalize(target.Email) ||
        !normalize(target.Email) || target.UserID && session.user.id !== target.UserID || session.error) {
      return result('identity_changed', 'identity_changed');
    }
    const accessToken = typeof session.accessToken === 'string' ? session.accessToken : '';
    if (!accessToken) return result('access_token_unavailable', 'access_token_missing');
    let claims = {};
    try {
      const encoded = accessToken.split('.')[1] || '';
      const decoded = JSON.parse(atob(encoded.replace(/-/g, '+').replace(/_/g, '/')));
      if (object(decoded)) claims = decoded;
    } catch {}
    const authClaims = object(claims['https://api.openai.com/auth']) ? claims['https://api.openai.com/auth'] : {};
    if (session.account != null && !object(session.account)) return result('eligibility_unconfirmed', 'account_binding_invalid');
    const binding = ids([
      session.account?.id, session.account?.account_id, session.accountId, session.account_id,
      session.user.accountId, session.user.account_id, claims.chatgpt_account_id, claims.account_id,
      authClaims.chatgpt_account_id, authClaims.account_id
    ]);
    if (binding.invalid) return result('eligibility_unconfirmed', 'account_binding_invalid');
    if (binding.values.length > 1) return result('eligibility_unconfirmed', 'account_binding_conflict');
    let accountID = binding.values[0] || '';
    diagnostic.accountIdPresent = Boolean(accountID);
    diagnostic.accountBinding = accountID ? 'session' : 'none';
    const headers = {authorization: 'Bearer ' + accessToken};

    diagnostic.phase = 'account_lookup';
    const accountRead = await read('/backend-api/accounts/check/v4-2023-04-27?timezone_offset_min=-540', get(headers));
    diagnostic.accountHttpStatus = accountRead.httpStatus;
    if (accountRead.failure) return result('eligibility_unconfirmed', 'account_' + accountRead.failure, accountRead.httpStatus);
    if (accountRead.httpStatus === 404 || accountRead.httpStatus === 405) {
      if (!accountID) return result('eligibility_unconfirmed', 'account_binding_missing', accountRead.httpStatus);
      diagnostic.phase = 'subscription_lookup';
      const subscriptionRead = await read('/backend-api/subscriptions?account_id=' + encodeURIComponent(accountID), get(headers));
      diagnostic.subscriptionHttpStatus = subscriptionRead.httpStatus;
      if (subscriptionRead.failure) return result('eligibility_unconfirmed', 'subscription_' + subscriptionRead.failure, subscriptionRead.httpStatus);
      if (!subscriptionRead.ok) return result('eligibility_unconfirmed', 'subscription_http_rejected', subscriptionRead.httpStatus);
      if (!subscriptionRead.valid || !object(subscriptionRead.value)) return result('eligibility_unconfirmed', 'subscription_response_invalid', subscriptionRead.httpStatus);
      diagnostic.source = 'subscription';
      const subscription = subscriptionRead.value;
      const subscriptionIDs = ids([subscription.account_id, subscription.account?.account_id, subscription.account?.id]);
      const subscriptionUsers = ids([subscription.user_id, subscription.userId, subscription.user?.id]);
      if (subscriptionIDs.invalid || subscriptionIDs.values.length > 1 ||
          subscriptionIDs.values.length === 1 && subscriptionIDs.values[0] !== accountID ||
          subscriptionUsers.invalid || subscriptionUsers.values.length > 1 ||
          subscriptionUsers.values.length === 1 && subscriptionUsers.values[0] !== session.user.id) {
        return result('identity_changed', 'account_identity_mismatch');
      }
      if (has(subscription, 'has_active_subscription') && typeof subscription.has_active_subscription !== 'boolean') {
        return result('eligibility_unconfirmed', 'subscription_response_invalid');
      }
      const active = subscription.has_active_subscription;
      diagnostic.activeSubscription = active === true ? 'true' : active === false ? 'false' : 'unknown';
      const plan = normalize(subscription.plan_type);
      if (paidPlans.has(plan) || active === true) {
        diagnostic.plan = 'paid';
        return result('already_subscribed', 'already_subscribed');
      }
      if (plan !== 'free') return result('eligibility_unconfirmed', plan ? 'plan_unknown' : 'subscription_plan_missing');
      diagnostic.plan = 'free';
    } else {
      if (!accountRead.ok) return result('eligibility_unconfirmed', 'account_http_rejected', accountRead.httpStatus);
      if (!accountRead.valid || !object(accountRead.value) || !object(accountRead.value.accounts)) {
        return result('eligibility_unconfirmed', 'account_response_invalid', accountRead.httpStatus);
      }
      const data = accountRead.value;
      const entries = Object.entries(data.accounts);
      if (entries.length === 0 || entries.length > 128) return result('eligibility_unconfirmed', 'account_response_invalid');
      const records = [];
      const seen = new Set();
      for (const [key, value] of entries) {
        if (!object(value) || !object(value.account)) return result('eligibility_unconfirmed', 'account_response_invalid');
        const recordIDs = ids([value.account.account_id, value.account.id]);
        if (recordIDs.invalid || recordIDs.values.length === 0) return result('eligibility_unconfirmed', 'account_response_invalid');
        if (recordIDs.values.length > 1) return result('identity_changed', 'account_identity_mismatch');
        const id = recordIDs.values[0];
        if (seen.has(id)) return result('eligibility_unconfirmed', 'account_ambiguous');
        seen.add(id);
        if (accountID && key === accountID && id !== accountID) return result('identity_changed', 'account_identity_mismatch');
        records.push({key, id, value});
      }
      let defaultKey = '';
      if (has(data, 'default_account') && data.default_account != null) {
        if (typeof data.default_account !== 'string') return result('eligibility_unconfirmed', 'account_default_invalid');
        defaultKey = data.default_account.trim();
      }
      const defaultRecord = defaultKey ? records.find(record => record.key === defaultKey) : null;
      if (defaultKey && !defaultRecord) return result('eligibility_unconfirmed', 'account_default_invalid');
      if (accountID && defaultRecord && defaultRecord.id !== accountID) return result('identity_changed', 'account_identity_mismatch');
      let selected;
      if (accountID) {
        selected = records.find(record => record.id === accountID);
        if (!selected) return result('eligibility_unconfirmed', 'account_not_found');
        diagnostic.accountBinding = 'matched';
      } else {
        if (!defaultRecord) return result('eligibility_unconfirmed', 'account_binding_missing');
        selected = defaultRecord;
        accountID = selected.id;
        diagnostic.accountIdPresent = true;
        diagnostic.accountBinding = 'default';
      }
      const account = selected.value.account;
      diagnostic.source = 'account_entitlement';
      if (account.structure != null && normalize(account.structure) !== 'personal') {
        return result('eligibility_unconfirmed', 'workspace_unsupported');
      }
      const userBinding = ids([
        account.user_id, account.userId, account.user?.id,
        selected.value.user_id, selected.value.userId, selected.value.user?.id
      ]);
      const expectedUser = typeof session.user.id === 'string' ? session.user.id : '';
      if (userBinding.invalid || userBinding.values.length > 1 ||
          userBinding.values.length === 1 && (!expectedUser || userBinding.values[0] !== expectedUser)) {
        return result('identity_changed', 'account_identity_mismatch');
      }
      if (selected.value.entitlement != null && !object(selected.value.entitlement)) {
        return result('eligibility_unconfirmed', 'entitlement_invalid');
      }
      const entitlement = selected.value.entitlement || {};
      if (has(entitlement, 'has_active_subscription') && typeof entitlement.has_active_subscription !== 'boolean') {
        return result('eligibility_unconfirmed', 'entitlement_invalid');
      }
      const active = entitlement.has_active_subscription;
      diagnostic.activeSubscription = active === true ? 'true' : active === false ? 'false' : 'unknown';
      const rawPlan = account.plan_type;
      if (rawPlan != null && typeof rawPlan !== 'string') return result('eligibility_unconfirmed', 'plan_unknown');
      const plan = normalize(rawPlan);
      if (paidPlans.has(plan) || active === true) {
        diagnostic.plan = 'paid';
        return result('already_subscribed', 'already_subscribed');
      }
      if (plan !== 'free' && (plan !== '' || active !== false)) return result('eligibility_unconfirmed', 'plan_unknown');
      diagnostic.plan = 'free';
    }

    diagnostic.phase = 'checkout_create';
    diagnostic.checkoutRequested = true;
    const checkoutRead = await read('/backend-api/payments/checkout', {
      method: 'POST', credentials: 'include', cache: 'no-store', redirect: 'error',
      headers: {...headers, 'content-type': 'application/json'},
      body: JSON.stringify({
        billing_details: {country: 'PH', currency: 'PHP'},
        checkout_ui_mode: 'hosted',
        entry_point: 'all_plans_pricing_modal',
        plan_name: 'chatgptplusplan'
      })
    }, true);
    diagnostic.checkoutHttpStatus = checkoutRead.httpStatus;
    if (checkoutRead.failure) return result('checkout_unavailable', 'checkout_' + checkoutRead.failure, checkoutRead.httpStatus);
    if (!checkoutRead.ok) {
      const detail = checkoutRead.valid && object(checkoutRead.value) && typeof checkoutRead.value.detail === 'string'
        ? checkoutRead.value.detail.toLowerCase() : '';
      if (detail.includes('already paid')) {
        diagnostic.plan = 'paid';
        diagnostic.activeSubscription = 'unknown';
        return result('already_subscribed', 'already_subscribed', checkoutRead.httpStatus);
      }
      if (checkoutRead.httpStatus === 401 || checkoutRead.httpStatus === 403) return result('checkout_unauthorized', 'checkout_unauthorized', checkoutRead.httpStatus);
      return result('checkout_rejected', 'checkout_http_rejected', checkoutRead.httpStatus);
    }
    if (!checkoutRead.valid || !object(checkoutRead.value)) return result('checkout_unavailable', 'checkout_response_invalid', checkoutRead.httpStatus);
    const payload = checkoutRead.value;
    diagnostic.phase = 'complete';
    return result('', 'checkout_created', checkoutRead.httpStatus, {
      responseTag: typeof payload.tag === 'string' ? payload.tag : '',
      checkoutURL: typeof payload.url === 'string' ? payload.url : '',
      processorEntity: typeof payload.processor_entity === 'string' ? payload.processor_entity : '',
      checkoutSessionID: typeof payload.checkout_session_id === 'string' ? payload.checkout_session_id : ''
    });
  } catch {
    if (diagnostic.phase === 'session') return result('session_unavailable', 'session_response_invalid');
    if (diagnostic.phase === 'account_lookup') return result('eligibility_unconfirmed', 'account_response_invalid');
    if (diagnostic.phase === 'subscription_lookup') return result('eligibility_unconfirmed', 'subscription_response_invalid');
    return result('checkout_unavailable', 'checkout_response_invalid');
  }
})()

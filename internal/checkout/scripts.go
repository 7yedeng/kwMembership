package checkout

const inspectFrameJS = `(() => {
  const allowedOrigins = new Set(['https://chatgpt.com', 'https://pay.openai.com', 'https://js.stripe.com']);
  const origin = allowedOrigins.has(location.origin) ? location.origin : null;
  const path = String(location.pathname || '');
  let route = null;
  if (origin === 'https://chatgpt.com') {
    if (path === '/checkout' || path === '/checkout/') route = '/checkout';
    else if (/^\/checkout\/[A-Za-z0-9_-]+\/?$/.test(path)) route = '/checkout/{id}';
    else if (path === '/settings/subscription' || path === '/settings/subscription/') route = '/settings/subscription';
    else if (path === '/settings/billing' || path === '/settings/billing/') route = '/settings/billing';
    else if (path === '/account/billing/overview' || path === '/account/billing/overview/') route = '/account/billing/overview';
  } else if (origin === 'https://pay.openai.com') {
    if (/^\/checkout\/[A-Za-z0-9_-]+\/?$/.test(path)) route = '/checkout/{id}';
    else if (/^\/(?:c\/)?pay\/[A-Za-z0-9_-]+\/?$/.test(path)) route = '/pay/{id}';
  }
  const find = selectors => {
    for (const selector of selectors) {
      const element = document.querySelector(selector);
      if (element) return element;
    }
    return null;
  };
  const attribute = (selectors, names) => {
    const element = find(selectors);
    if (!element) return null;
    for (const name of names) {
      const value = element.getAttribute(name);
      if (typeof value === 'string' && value.trim()) return value.trim();
    }
    return null;
  };
  const has = selectors => Boolean(find(selectors));
  const control = entries => {
    for (const [id, selectors] of entries) if (has(selectors)) return id;
    return null;
  };
  const planRaw = attribute(['[data-kw-plan]', '[data-testid="plan-name"][data-plan]', 'meta[name="openai-plan"]'], ['data-kw-plan', 'data-plan', 'content']);
  const countryRaw = attribute(['[data-kw-country]', '[data-testid="checkout-price"][data-country]', 'meta[name="openai-country"]'], ['data-kw-country', 'data-country', 'content']);
  const currencyRaw = attribute(['[data-kw-currency]', '[data-testid="checkout-price"][data-currency]', 'meta[name="openai-currency"]'], ['data-kw-currency', 'data-currency', 'content']);
  const amountRaw = attribute(['[data-kw-amount]', '[data-testid="checkout-price"][data-amount]', 'meta[name="openai-amount"]'], ['data-kw-amount', 'data-amount', 'content']);
  const stateRaw = attribute(['[data-kw-checkout-state]', '[data-testid="checkout-state"][data-state]'], ['data-kw-checkout-state', 'data-state']);
  const amount = amountRaw !== null && /^\d+(?:\.\d{1,2})?$/.test(amountRaw) ? Number(amountRaw) : null;
  return {
    origin,
    routeTemplate: route,
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
      progression: control([
        ['payment-next', ['#payment-next', '[data-testid="checkout-next"]']],
        ['hosted-payment-next', ['[data-testid="hosted-payment-next-button"]', 'button[name="continue-payment"]']]
      ]),
      submit: control([
        ['payment-submit', ['#payment-submit', '[data-testid="checkout-submit"]']],
        ['hosted-payment-submit', ['[data-testid="hosted-payment-submit-button"]', 'button[name="submit-payment"]']]
      ]),
      upgradeX5: control([['upgrade-x5', ['[data-testid="upgrade-to-prolite"]', 'button[data-plan="prolite"][data-action="upgrade"]']]]),
      upgradeX20: control([['upgrade-x20', ['[data-testid="upgrade-to-pro"]', 'button[data-plan="pro"][data-action="upgrade"]']]]),
      challenge: control([
        ['challenge-3ds', ['[data-testid="three-d-secure-challenge"]', 'iframe[title="3D Secure authentication"]']],
        ['challenge-captcha', ['[data-testid="captcha-challenge"]']],
        ['challenge-sms', ['[data-testid="sms-verification"]']],
        ['challenge-bank', ['[data-testid="bank-verification"]']]
      ])
    }
  };
})()`

const fillFrameFunction = `(fragment => {
  if (!['https://chatgpt.com', 'https://pay.openai.com', 'https://js.stripe.com'].includes(location.origin)) return {accepted:false,filled:[]};
  const selectors = {
    cardNumber: ['#payment-numberInput', 'input[name="number"]', 'input[autocomplete="cc-number"]'],
    expiry: ['#payment-expiryInput', 'input[name="expiry"]', 'input[autocomplete="cc-exp"]'],
    expiryMonth: ['#payment-expiryMonthInput', 'select[name="exp-month"]'],
    expiryYear: ['#payment-expiryYearInput', 'select[name="exp-year"]'],
    cvc: ['#payment-cvcInput', 'input[name="cvc"]', 'input[autocomplete="cc-csc"]'],
    billingName: ['#billingAddress-nameInput', 'input[name="name"]', 'input[autocomplete="billing name"]'],
    billingLine1: ['#billingAddress-line1Input', 'input[name="addressLine1"]', 'input[autocomplete="billing address-line1"]'],
    billingCity: ['#billingAddress-cityInput', 'input[name="city"]', 'input[autocomplete="billing address-level2"]'],
    billingState: ['#billingAddress-stateInput', 'select[name="state"]', 'input[autocomplete="billing address-level1"]'],
    billingCountry: ['#billingAddress-countryInput', 'select[name="country"]', 'select[autocomplete="billing country"]'],
    billingPostal: ['#billingAddress-postalCodeInput', 'input[name="postalCode"]', 'input[autocomplete="billing postal-code"]']
  };
  const find = key => {
    for (const selector of selectors[key] || []) {
      const element = document.querySelector(selector);
      if (element) return element;
    }
    return null;
  };
  const filled = [];
  for (const [key, value] of Object.entries(fragment || {})) {
    if (typeof value !== 'string') continue;
    const element = find(key);
    if (!element) continue;
    const prototype = element instanceof HTMLSelectElement ? HTMLSelectElement.prototype : HTMLInputElement.prototype;
    const setter = Object.getOwnPropertyDescriptor(prototype, 'value')?.set;
    if (setter) setter.call(element, value); else element.value = value;
    element.dispatchEvent(new Event('input', {bubbles:true}));
    element.dispatchEvent(new Event('change', {bubbles:true}));
    element.dispatchEvent(new Event('blur', {bubbles:true}));
    filled.push(key);
  }
  return {accepted:true,filled};
})`

const activateControlFunction = `(controlId => {
  const selectors = {
    'payment-next': ['#payment-next', '[data-testid="checkout-next"]'],
    'hosted-payment-next': ['[data-testid="hosted-payment-next-button"]', 'button[name="continue-payment"]'],
    'payment-submit': ['#payment-submit', '[data-testid="checkout-submit"]'],
    'hosted-payment-submit': ['[data-testid="hosted-payment-submit-button"]', 'button[name="submit-payment"]'],
    'upgrade-x5': ['[data-testid="upgrade-to-prolite"]', 'button[data-plan="prolite"][data-action="upgrade"]'],
    'upgrade-x20': ['[data-testid="upgrade-to-pro"]', 'button[data-plan="pro"][data-action="upgrade"]']
  };
  let element = null;
  for (const selector of selectors[controlId] || []) {
    element = document.querySelector(selector);
    if (element) break;
  }
  if (!element || element.disabled === true || element.hidden === true || element.getAttribute('aria-disabled') === 'true') return {activated:false};
  element.click();
  return {activated:true};
})`

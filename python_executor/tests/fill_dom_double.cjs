// Offline DOM double for the production FILL_FRAME_JS. This intentionally does
// not claim Chromium, framework, or live checkout compatibility. All material
// below is synthetic test data; nothing is sent over a network or logged.
const fs = require('node:fs');
const vm = require('node:vm');
const {source, scenario} = JSON.parse(fs.readFileSync(0, 'utf8'));
let writes = 0, renderTicks = 0;
const elements = new Map();
let onRender = () => {};

class Input {
  constructor() {
    this.currentValue = '';
    this.disabled = false;
    this.readOnly = false;
    this.required = false;
    this.validity = {valid: true};
    this.attributes = {};
    this.onEvent = () => {};
  }
  get value() { return this.currentValue; }
  set value(value) { writes++; this.currentValue = String(value); }
  getAttribute(name) { return this.attributes[name] || null; }
  dispatchEvent(event) { this.onEvent(event); return true; }
}
class Select extends Input {
  constructor(values) { super(); this.options = values.map(value => ({value, disabled: false})); }
  get value() { return this.currentValue; }
  set value(value) {
    writes++;
    this.currentValue = this.options.some(option => option.value === value) ? value : '';
  }
}
class Event {
  constructor(type, options) { this.type = type; this.bubbles = options.bubbles; }
}
const input = selector => { const element = new Input(); elements.set(selector, element); return element; };
const select = (selector, values) => { const element = new Select(values); elements.set(selector, element); return element; };
const number = input('#payment-numberInput');
const expiry = input('#payment-expiryInput');
const cvc = input('#payment-cvcInput');
input('#billingAddress-nameInput');
select('#billingAddress-countryInput', ['PH', 'US']);
const postal = input('#billingAddress-postalCodeInput');
const fragment = {
  // Synthetic payment fixture, never a real account/card/billing profile.
  cardNumber: '4242424242424242', expiry: '1/30', expiryMonth: '1', expiryYear: '2030', cvc: '123',
  billingName: 'Synthetic Fixture', billingLine1: '1 Test Street', billingLine2: '', billingCity: 'Test City',
  billingState: 'Test State', billingCountry: 'PH', billingPostal: '00000',
};
let origin = 'https://pay.openai.com';
switch (scenario) {
  case 'formatted':
    number.onEvent = event => { if (event.type === 'input') number.currentValue = fragment.cardNumber.match(/.{1,4}/g).join(' '); };
    expiry.onEvent = event => { if (event.type === 'input') expiry.currentValue = '01 / 2030'; };
    break;
  case 'fallback-selector':
    elements.delete('#payment-numberInput');
    elements.set('input[autocomplete="cc-number"]', number);
    break;
  case 'normalized-selects':
    elements.delete('#payment-expiryInput');
    select('#payment-expiryMonthInput', ['01', '02']);
    select('#payment-expiryYearInput', ['29', '30']);
    break;
  case 'actual-billing-country':
    select('#billingAddress-countryInput', ['GB', 'US']);
    fragment.billingCountry = 'GB';
    break;
  case 'unsupported-select':
    select('#billingAddress-countryInput', ['US']);
    break;
  case 'sync-reset':
    cvc.onEvent = event => { if (event.type === 'input') cvc.currentValue = ''; };
    break;
  case 'render-replacement':
    onRender = () => { if (renderTicks === 1) input('#payment-cvcInput'); };
    break;
  case 'native-invalid':
    postal.validity.valid = false;
    break;
  case 'optional-invalid':
    input('#billingAddress-stateInput').attributes['aria-invalid'] = 'true';
    break;
  case 'optional-missing-material':
    input('#billingAddress-stateInput').required = true;
    fragment.billingState = '';
    break;
  case 'line2-accepted':
    input('#billingAddress-line2Input');
    fragment.billingLine2 = 'Suite 2';
    break;
  case 'line2-truncated': {
    const line2 = input('#billingAddress-line2Input');
    line2.onEvent = event => { if (event.type === 'input') line2.currentValue = line2.currentValue.slice(0, 3); };
    fragment.billingLine2 = 'Suite 2';
    break;
  }
  case 'optional-empty-material':
    input('#billingAddress-stateInput').currentValue = 'Unverified default';
    fragment.billingState = '';
    fragment.billingPostal = '';
    break;
  case 'required-empty-postal':
    postal.required = true;
    fragment.billingPostal = '';
    break;
  case 'aria-required-empty-state':
    input('#billingAddress-stateInput').attributes['aria-required'] = 'true';
    fragment.billingState = '';
    break;
  case 'new-required-after-render':
    elements.delete('#billingAddress-postalCodeInput');
    delete fragment.billingPostal;
    onRender = () => { if (renderTicks === 1) input('#billingAddress-postalCodeInput').required = true; };
    break;
  case 'foreign-origin':
    origin = 'https://foreign.example';
    break;
  case 'empty-non-rendering-frame':
    elements.clear();
    break;
  default:
    throw new Error('unknown synthetic DOM scenario');
}

const context = vm.createContext({
  document: {querySelector: selector => elements.get(selector) || null},
  location: {origin}, HTMLInputElement: Input, HTMLSelectElement: Select, Event,
  setTimeout, clearTimeout,
  requestAnimationFrame: callback => {
    if (scenario === 'empty-non-rendering-frame') return;
    return setImmediate(() => { renderTicks++; onRender(); callback(); });
  },
});
(async () => {
  const fill = vm.runInContext('(' + source + ')', context);
  const result = await fill(fragment);
  // Results contain field names and booleans only. No fixture values escape.
  process.stdout.write(JSON.stringify({result, hasForm: elements.size > 0, crossedRender: renderTicks >= 2, wrote: writes > 0}));
})().catch(() => { process.stderr.write('synthetic DOM evaluation failed\n'); process.exitCode = 1; });

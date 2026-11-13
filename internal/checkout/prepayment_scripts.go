package checkout

// This reads visible semantic rows only. It neither installs synthetic price
// attributes nor evaluates arbitrary page globals. Missing rows, duplicate
// conflicting prices, multiple plan names and ambiguous tax models stay unknown.
// These are supported DOM shapes, not a claim about every live merchant page.
const observePrepaymentDOMJS = `(() => {
  const allowed = ['https://chatgpt.com', 'https://pay.openai.com'];
  const empty = () => ({origin:'',plan:'',currency:'',subtotal:null,tax:null,discount:null,total:null,taxIncluded:null,
    finalControl:false,finalEnabled:false,progression:false,challenge:false,source:'visible_dom'});
  if (!allowed.includes(location.origin)) return empty();
  const visible = element => {
    if (!element || element.hidden || element.closest('[hidden],[aria-hidden="true"]')) return false;
    const style = getComputedStyle(element);
    return style.display !== 'none' && style.visibility !== 'hidden' && style.visibility !== 'collapse' && element.getClientRects().length > 0;
  };
  const normalized = value => String(value || '').replace(/\u00a0/g,' ').replace(/\s+/g,' ').trim();
  const text = element => normalized(element?.innerText || element?.textContent || '');
  const result = empty(); result.origin = location.origin;
  const values = {subtotal:[],tax:[],discount:[],total:[]};
  const taxModels = [];
  const currencyNames = [];
  const minor = value => {
    const parts=value.split('.');
    return BigInt(parts[0])*100n+BigInt(((parts[1] || '')+'00').slice(0,2));
  };
  const money = raw => {
    raw=normalized(raw);
    if (!raw || raw.length>100) return null;
    const found=/^(-)?\s*(PHP|EGP|USD|₱|E£)?\s*((?:0|[1-9]\d*|[1-9]\d{0,2}(?:,\d{3})+)(?:\.\d{1,2})?)\s*(PHP|EGP|USD|₱|E£)?$/i.exec(raw);
    if (!found) return null;
    const symbol=value => ({'₱':'PHP','E£':'EGP'}[value] || String(value||'').toUpperCase());
    const left=symbol(found[2]), right=symbol(found[4]);
    if (left && right && left!==right) return null;
    const value=found[3].replace(/,/g,'');
    if (value.length>20) return null;
    return {value,currency:left||right,negative:Boolean(found[1])};
  };
  const kind = raw => {
    const label=normalized(raw).toLowerCase().replace(/[:：]$/,'');
    if (/^(subtotal|sub total|小计|小計)$/.test(label)) return ['subtotal',null];
    if (/^(discount|discounts|折扣|优惠|優惠)$/.test(label)) return ['discount',null];
    if (/^(total|total due|amount due|due today|total due today|应付总额|應付總額|合计|合計|总计|總計|今日应付|今日應付)$/.test(label)) return ['total',null];
    if (/^(tax|taxes|税|税费|稅|稅費)(\s*\(\d+(?:\.\d+)?%\))?$/.test(label)) return ['tax',null];
    if (/^(tax \(included\)|included tax|tax included|含税|含稅)$/.test(label)) return ['tax',true];
    if (/^(tax \(exclusive\)|tax excluded|不含税|不含稅)$/.test(label)) return ['tax',false];
    return null;
  };
  const add = (label,raw) => {
    const category=kind(label), amount=money(raw);
    if (!category || !amount || amount.negative && category[0]!=='discount') return;
    values[category[0]].push(amount.value);
    if (amount.currency) currencyNames.push(amount.currency);
    if (category[0]==='tax' && category[1]!==null) taxModels.push(category[1]);
  };
  const leaves = [...document.querySelectorAll('span,div,p,dt,dd,th,td,label,h1,h2,h3,[role="heading"]')]
    .filter(element=>visible(element) && element.children.length===0);
  const planNames = new Set();
  for (const element of leaves) {
    const value=text(element);
    const match=/^(?:subscribe to\s+|订阅\s*|訂閱\s*)?chatgpt\s+(plus|go|pro|pro lite)$/i.exec(value);
    if (match) planNames.add(match[1].toLowerCase());
    if (/^(PHP|EGP|USD)$/.test(value)) currencyNames.push(value);
    if (/^(no discounts?|无折扣|無折扣)$/i.test(value)) values.discount.push('0.00');
    if (/^(no tax|tax exempt|免税|免稅)$/i.test(value)) {values.tax.push('0.00');taxModels.push(false);}
    if (!kind(value)) continue;
    if (element.tagName==='DT' && element.nextElementSibling?.tagName==='DD') {
      add(value,text(element.nextElementSibling));continue;
    }
    // Match one semantic label to one nearby value, never a whole-page number.
    for (let parent=element.parentElement,depth=0;parent && depth<3;parent=parent.parentElement,depth++) {
      const candidates=[...parent.children].filter(child=>child!==element && !child.contains(element) && visible(child));
      const amounts=candidates.map(child=>text(child)).filter(raw=>money(raw));
      if (amounts.length===1 && text(parent).length<200) {add(value,amounts[0]);break;}
      if (amounts.length>1 || text(parent).length>=200) break;
    }
  }
  if (planNames.size===1) result.plan=[...planNames][0];
  const currencies=[...new Set(currencyNames)];
  if (currencies.length===1) result.currency=currencies[0];
  for (const key of Object.keys(values)) {
    const unique=[...new Set(values[key].map(value=>minor(value).toString()))];
    if (unique.length===1) {
      const value=BigInt(unique[0]);
      result[key]=(value/100n).toString()+'.'+(value%100n).toString().padStart(2,'0');
    }
  }
  const models=[...new Set(taxModels)];
  if (models.length===1) result.taxIncluded=models[0];
  else if (models.length===0 && Object.keys(values).every(key=>result[key]!==null)) {
    const subtotal=minor(result.subtotal),tax=minor(result.tax),discount=minor(result.discount),total=minor(result.total);
    const inclusive=subtotal-discount===total && tax<=total;
    const exclusive=subtotal-discount+tax===total;
    // The explicit amounts can uniquely distinguish tax inclusion. Zero-tax
    // ambiguity is retained unless the page explicitly says tax-exempt/model.
    if (inclusive!==exclusive) result.taxIncluded=inclusive;
  }
  const controls=[...document.querySelectorAll('button,input[type="submit"],[role="button"]')].filter(visible);
  const final=[],next=[];
  for (const element of controls) {
    const label=normalized(element.getAttribute('aria-label') || element.innerText || element.value).toLowerCase();
    const enabled=!element.disabled && element.getAttribute('aria-disabled')!=='true';
    if (/^(subscribe|pay|pay now|subscribe and pay|confirm payment|订阅|訂閱|支付|立即支付|确认付款|確認付款)$/.test(label)) final.push(enabled);
    if (/^(next|continue|continue to payment|下一步|继续|繼續)$/.test(label)) next.push(enabled);
  }
  result.finalControl=final.length===1 && next.length===0;
  result.finalEnabled=result.finalControl && final[0];
  result.progression=next.length===1 && final.length===0;
  result.challenge=Boolean(document.querySelector('#challenge-form,#challenge-running,[data-testid="three-d-secure-challenge"],iframe[title="3D Secure authentication"],[data-testid="captcha-challenge"],[data-testid="sms-verification"],[data-testid="bank-verification"]'));
  return result;
})()`

# 本地结账金额审核

`membership-review` 是独立的离线审核入口。它检查输入的套餐、国家、币种、金额关系和预算，输出受控 JSON。它不启动浏览器、不读取账号、不连接卡台、不打开业务数据库，也不会提交付款。

## 当前验收边界

- `reviewReady: true` 仅表示本地输入完整、金额计算一致且落在预算区间。
- `remoteVerified` 和 `paymentAllowed` 始终为 `false`。
- `evidenceSource` 固定为 `local_input`。用户手工填写或复制的数值不能冒充执行器取得的真实页面证据。
- 这份摘要不证明账号身份、结账有效期、银行卡可用性、当前付款页面状态、续费状态或最终开通结果。
- 本模块识别 Go/Plus/x5/x20 标签及 PH/PHP、EG/EGP 市场组合，属于本地数据检查；不会扩大现有执行器的计划、国家或付款权限。

## 输入示例

以下数字全部为**合成测试数据，不是 Go 的当前价格**。

```json
{
  "expected": {
    "plan": "go",
    "country": "EG",
    "currency": "EGP"
  },
  "evidence": {
    "plan": "go",
    "country": "EG",
    "currency": "EGP",
    "subtotal": "200.00",
    "tax": "28.00",
    "discount": "0.00",
    "total": "228.00",
    "taxIncluded": false
  },
  "budget": {
    "min": "220.00",
    "max": "230.00"
  }
}
```

字段解释：

| 字段 | 含义 |
| --- | --- |
| expected | 订单预期的套餐、国家、币种，三项均须明确。 |
| evidence | 待审核的结账信息；必须与 expected 一致。 |
| subtotal | 折扣前小计；是否已经含税由 taxIncluded 明确表示。 |
| tax | 最终税额；零税必须显式提供字符串 `"0.00"`。 |
| discount | 折扣额；没有折扣也必须显式提供。 |
| total | 最终应付合计。 |
| taxIncluded | true 表示 subtotal 已含税；false 表示需要另加税。不得缺省或为 null。 |
| budget.min / max | 允许的最终合计区间，含上下边界。 |

所有金额都必须是十进制**字符串**，不能传 JSON 浮点数字。PHP 与 EGP 在此使用两位最小货币单位，不进行汇率换算。

接受 `"0"`、`"0.01"`、`"12.3"`、`"12.30"`。拒绝负数、正号、科学计数法、NaN、千位逗号、空白、过多小数位、前导零、非 ASCII 数字和整数溢出。不存在自动去逗号、猜币种、补零税或金额四舍五入。

## 含税与未含税算法

- `taxIncluded: false`：`total = subtotal - discount + tax`。
- `taxIncluded: true`：`total = subtotal - discount`；tax 只表示最终 total 中包含的税额，不能再次加税，并且不能大于 total。
- discount 不能大于 subtotal。
- 加法使用溢出检查；计算和预算比较都使用 int64 最小单位，不经过浮点。
- 零金额可以显式表示，但仍不会赋予付款权限。

未来真实页面适配器需要把页面语义转换成上述契约。例如原始页面的“subtotal”若始终表示未含税值，就不能仅因为商品有含税标记而直接把 taxIncluded 设置为 true。缺少税费或折扣事实时，应保留缺失状态并停止，不能猜测。

## CLI

```text
membership-review --example > quote-example.json
membership-review < quote-example.json
membership-review < my-quote.json
```

`--example` 向 stdout 输出可直接读取的合成请求，同时向 stderr 输出固定的“非实时价格”提示。默认模式从 stdin 读一份 JSON；输入上限 16 KiB，SIGINT/SIGTERM 可以取消未完成的读取。

成功示例的关键输出为：

```json
{
  "operation": "review",
  "code": "QUOTE_REVIEW_READY",
  "evidenceSource": "local_input",
  "reviewReady": true,
  "paymentAllowed": false,
  "remoteVerified": false,
  "quote": {
    "plan": "go",
    "country": "EG",
    "currency": "EGP",
    "minorUnitScale": 2,
    "subtotalMinor": 20000,
    "taxMinor": 2800,
    "discountMinor": 0,
    "totalMinor": 22800,
    "taxIncluded": false,
    "budgetMinMinor": 22000,
    "budgetMaxMinor": 23000
  }
}
```

金额输出为 int64 整数最小单位；消费端必须使用能保持整数精度的解析方式。不能把该摘要中的 ready 状态接到生产付款按钮作为唯一许可。

输出只包含固定状态、受支持的枚举和规范化金额；没有邮箱、Session、卡号、Cookie、自由文本或结账 URL 字段。未知输入字段、重复字段（包括大小写别名）、额外 JSON 文档均拒绝，错误不回显原始内容。

进程退出码：0 为本地审核通过或示例/帮助成功，2 为输入/审核/取消错误，1 为输出失败。操作错误时 quote 不返回，避免展示未经核验的部分金额。

## 接口与接入点

源码位于 [quote.go](../internal/checkout/quote.go)。

- `ParseQuoteMinor(value, currency)`：将受支持币种的十进制字符串转成 int64。
- `DecodeQuoteReview(reader)`：限长、严格结构解析；不接触网站。
- `ReviewQuote(QuoteReviewRequest)`：纯本地校验，返回 QuoteReviewReport。
- `QuoteFailure(code)` / `SafeQuoteCode(error)`：固定错误码映射，不返回上游原始错误。
- `QuoteEvidence` 用字符串指针和布尔指针保留“未取得”与“已明确为零/false”的区别。

可由 CLI、桌面程序或开发用展示界面共用此模块。现阶段没有修改既有 PageFacts、生产 PriceContract 浮点字段、数据库 schema、Go 付款许可或浏览器脚本。后续页面事实接入需单独完成，不能假定它已经发生。

## 已运行的检查

```text
GOPROXY=off GOTOOLCHAIN=local /workspace/scratch/1f9fd57481f1/toolchain/go/bin/go test ./internal/checkout ./cmd/membership-review
```

两包通过。针对性测试覆盖：

- 精确小数转换、最大 int64 边界、负数/科学计数/千分位/溢出拒绝；
- 未含税与含税两种算法，税额不一致、折扣超过小计；
- 缺失税额、折扣、总额或 taxIncluded 不自动当作零；
- 套餐、国家、币种不匹配以及预算边界；
- JSON 浮点、未知敏感字段、重复键、多个文档、超大输入；
- CLI 合成示例、错误信息不回显私密字段、阻塞 stdin 取消。

没有调用网站或真实账号，没有报价抓取、开卡、绑卡或付款验证。

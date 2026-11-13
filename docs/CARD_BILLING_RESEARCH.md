# ZovoCard 开卡姓名与账单地址：源码和接口核验

核验日期：2026-11-13。官方仓库基线为 `zovocard/zovo_card_cdk_auto@19f41ef347cdb3cce52fde0f2b43e12f70dc6ebe`；kwMembership 上游基线为 `16ceb97fc15f629c8b59df242e1baee2227e4c77`。调查阶段只读公开文档、代码和官方帮助网页；随后实现了卡资料解析与离线测试，见 §5.1。没有登录用户账号、读取凭据、开卡、充值或提交付款。

## 1. 结论

**为了使用 ZovoCard 的卡，不需要先找回 kwRedeem 的随机地址生成器。** 更直接的路线是：开卡时明确提供并保存持卡人姓名；付款前通过该张卡的详情接口取得姓名和 `card_address`；只有地址完整且来源已确认时才映射给付款执行器，缺失时使用与该卡绑定的已确认配置或停止等待补齐。

这个结论包含三个不同事实：

1. **ZovoCard 网页开卡弹窗会生成姓名。** 官方新手图文教程 §1.4 明确说明，弹窗预填随机持卡人姓名，并可更换。该网页行为不等于开放 API 会替调用者生成姓名。[官方图文教程](https://help.zovocard.com/zhcn/help/zc-beginner-illustrated/)
2. **开放 API 开卡要求调用方提交姓名。** `POST /cards/open` 的 `first_name`、`last_name` 都标为必填；开卡结果示例回显这两项。[官方 OpenAPI 文档 §6.2](https://github.com/zovocard/zovo_card_cdk_auto/blob/19f41ef347cdb3cce52fde0f2b43e12f70dc6ebe/docs/zovocard-openapi-zh.md)
3. **卡详情 API 已有账单地址字段，但不保证当前每张卡都返回完整地址。** `GET /cards/{id}` 的 `card_address` 明确标注为账单地址；然而文档示例中的六个地址字段全是空字符串，也未给出各 issuer 的非空保证。因此可以确认“有公开查询契约”，不能确认“每个渠道开卡时一定自动生成可用于实付的完整地址”。[官方 OpenAPI 文档 §6.5](https://github.com/zovocard/zovo_card_cdk_auto/blob/19f41ef347cdb3cce52fde0f2b43e12f70dc6ebe/docs/zovocard-openapi-zh.md)

此前对 kwMembership 的依赖分析强调了自有账单资料注入；这里补充更准确的优先数据源：**先保留并验证 ZovoCard 卡详情的账单资料，再处理其缺失情况。** 源码中的随机地址服务只是原作者采用的实现，不能由它的存在推断卡台没有相应字段。

## 2. 开卡 API 到底需要哪些资料

### 2.1 共同请求契约

官方文档 §6.2 定义统一 `POST /openapi/v1/cards/open`，通过 `product_code` 选择产品和对应渠道，没有公开一套按 issuer 分开的持卡人创建接口。

| 请求字段 | 文档要求 | 能证明的含义 |
| --- | --- | --- |
| `product_code` | 必填 | 从当前 `GET /products` 取得并原样提交，用它选择卡产品。 |
| `first_name` | 必填 | 调用者提交持卡人名。不能仅依赖网页会自动填写而省略。 |
| `last_name` | 必填 | 调用者提交持卡人姓。 |
| `init_amount` | 必填 | 初始注资；需满足产品最低值。它不是姓名或地址生成参数。 |
| `max_on_daily`、`max_on_monthly`、`max_on_percent` | 可选，仅 `four` | 分别为日、月、单笔限额。 |
| `transaction_limit`、`transaction_limit_type` | 可选，仅 `four` | 总交易额度及 `limited` / `unlimited` 类型。 |
| `card_address`、街道、城市、邮编、国家 | 该开卡参数表未列出 | 不能自造这些请求字段并宣称支持自定义账单地址。 |

批量开卡 `POST /cards/batch-open` 在上述参数上增加 `count`，未另设姓名/地址生成协议。开卡示例响应包含 `first_name` 和 `last_name`；这证明文档允许取回姓名，不证明该姓名已通过任何另外的身份验证。

### 2.2 issuer 差异：已有证据和不能外推的部分

| issuer | 文档 §6.11.1 的描述 | 姓名/地址可据文档确认到什么程度 |
| --- | --- | --- |
| `one` | 默认主力渠道 | 适用统一开卡请求契约；没有单独的完整地址生成保证。 |
| `two` | 已退役，存量卡只读/有限操作 | 不应假定仍可新开卡；存量卡的实际字段和可操作性需查询确认。 |
| `three` | 含卡池、按 BIN 等能力 | 未公布独立的姓名/地址字段要求或每张卡的地址完整性保证。 |
| `four` | 星链卡（PhotonPay），支持消费限额 | 明确多了限额字段；没有证据支持将“多了限额能力”外推成自动生成完整账单地址。 |

同一份文档 §6.1 仍有只提 `one` / `two` 的早期概述，而较后的 §6.11.1 列出四个渠道并注明 `two` 退役。实施时应按实际 `GET /products` 返回的产品和 issuer 判断能力；不能把概述当作当前渠道全集。

这些是 ZovoCard 对外适配层的契约。已公开的 `zovo_card_cdk_auto` 是 CDK 接入站，没有在本次检索中提供四个真实发卡渠道的完整开卡后端实现，因此不能仅靠此仓库解释每家底层 issuer 如何产生或登记地址。

## 3. 哪个接口可以取回卡绑定的姓名与地址

### 3.1 三种返回形态

| 接口 | 文档中的相关字段 | 对本项目的作用 |
| --- | --- | --- |
| `POST /cards/open` | `first_name`、`last_name`，以及卡号、有效期、CVV、状态和本地卡 ID | 保存开卡请求与结果关联；示例未包含 `card_address`。 |
| `GET /cards` | 列表项包含 `first_name`、`last_name`；列表不返回 CVV | 可用于定位卡和核对姓名；不应把列表当作完整付款材料。 |
| `GET /cards/{id}` | `user_name`、`first_name`、`last_name`、`card_address`，以及完整卡材料 | 已公开的按卡查询姓名与账单地址的入口。 |

卡详情的 `card_address` 有以下六项，示例全部为 `""`：

| 官方字段 | 可以映射到的内部字段 | 仍需验证 |
| --- | --- | --- |
| `address_line_one` | 街道第一行 `Line1` | 是否非空、是否为该卡实际登记资料。 |
| `address_line_two` | 第二行 `Line2` | 可空；原上游 checkout 数据结构没有这一项，本轮 provider 已新增保留。 |
| `city` | `City` | 实际格式和是否提供。 |
| `state` | `State` | 是地区代码还是名称，不能无依据统一为 `DE`。 |
| `country` | `Country` | 是否 ISO 两字母代码；文档没有给出非空样例。 |
| `post_code` | `PostalCode` | 按该国家的格式校验，不能固定猜为美国邮编。 |

`{id}` 是 **ZovoCard 本地数字卡 ID**，而卡详情中的 `card_id` 示例是底层渠道字符串 ID。这与 kwMembership 的命名有一处容易混淆：其 `Card.UpstreamCardID` 实际由 ZovoCard 响应的数字 `id` 赋值，`VMCardID` 才保存 `vm_card_id`。资料关联时应使用平台 key 加上被查询的数字卡 ID，保留产品/issuer 信息，不以姓名或后四位作为唯一键。[spacex.go](../internal/provider/spacex.go)

### 3.2 未发现的能力及结论边界

对固定版 OpenAPI 文档及官方 CDK 站 `backend`、`frontend/src`、`automation` 的定向检索中，未发现以下公开 API 契约：

- 独立的姓名生成 API；
- 独立的卡账单地址生成 API；
- 绑定或修改某张卡 `card_address` 的 API；
- 可保证各 issuer 返回完整地址的字段矩阵。

这不等于 ZovoCard 内部绝无这些能力；只表示当前公开材料不足以据此编写客户端。`PUT /cards/{id}/remark` 修改备注，不能代替地址更新。CDK 站的 `POST /api/v1/public/billing/check` 返回订阅摘要、发票或账单链接，也不是银行卡邮寄/账单地址查询；两种 “billing” 含义不能混用。对应来源分别为官方文档 §6.13、§6.20 以及 [gpt_billing.go](https://github.com/zovocard/zovo_card_cdk_auto/blob/19f41ef347cdb3cce52fde0f2b43e12f70dc6ebe/backend/internal/handler/gpt_billing.go)。

官方帮助文章也有措辞差异：[LinkedIn 教程](https://help.zovocard.com/zhcn/blog/b8ccocnq/) 概括称卡片带有账单地址；[付款被拒排查](https://help.zovocard.com/zhcn/blog/gyflphss/) 要求按发卡方登记资料核对地址。它们支持优先读取卡自身资料，但没有补齐 API 的 issuer 差异、空值语义或地址来源。技术实现仍需真实卡详情的脱敏样本或平台明确的字段说明。

## 4. kwMembership 为什么仍在用外部地址生成器

### 4.1 它没有保留卡台已有的姓名和地址字段

固定上游版本的 `SpaceXClient.GetCardMaterial` 已经调用正确的 `GET /cards/{id}`，但本地解码结构只定义 `card_number`、`cvv`、`expire`、`status`，返回的 `CardMaterial` 也只有卡号、CVV、月份、年份和状态。`user_name`、`first_name`、`last_name` 与 `card_address` 即使由卡台返回，也会在这个解码步骤被忽略。本分支已按 §5.1 补齐读取层。[spacex.go](../internal/provider/spacex.go)

`OpenCard` 同样只解码 ID、产品、余额、状态和费用，没有把返回姓名纳入 `OpenCardResult`；`ListProducts` 未保留 issuer。因此原代码目前无法依靠这些已丢弃字段建立按 issuer 或按卡选择账单资料的策略。这是客户端数据模型的缺口，不能当作卡台没有地址字段的证据。

### 4.2 原作者的地址服务提供了两个独立调用

[address.go](../internal/provider/address.go) 向配置的 `API_URL` 请求 `/api/public/us-address/generate?count=1&includePerson=true&state=DE`，默认指向本机 kwRedeem API，而不是 ZovoCard OpenAPI。

- [payment.go](../internal/processor/payment.go) 的 `paymentBuildFundingRequest` 在新开卡时调用 `Cardholder`，将返回姓名写入开卡资金请求。
- [checkout.go](../internal/processor/checkout.go) 在取得卡材料后又调用 `Billing`，取付款姓名和地址。

两次调用各自请求新资料，没有把第二次返回值绑定到第一次开卡请求或某张卡。`Billing` 的检查只接受 `US/DE` 并校验长度，无法证明地址与真实卡资料相符。这也是为何“把旧生成器抄回来”并未解决按卡绑定问题。

Efun 适配器可作为对照，但不能拿来推定 ZovoCard 行为：[efuncard.go](../internal/provider/efuncard.go) 的 `OpenCard` 实际调用 `/cards/purchase`，发送 `cardTypeId`、数量、金额和备注，不发送共用 `OpenCardInput` 中的姓名；其卡详情解码结构也没有姓名或账单地址。一个共用接口并不意味着两个供应商的身份/地址契约相同。

### 4.3 官方 automation 里的随机资料不是发卡方资料

官方 [automation/lib/address.js](https://github.com/zovocard/zovo_card_cdk_auto/blob/19f41ef347cdb3cce52fde0f2b43e12f70dc6ebe/automation/lib/address.js) 从固定姓名、街道列表和州/城市组合中随机生成资料，或从第三方地址页面抓取后回退本地；整个模块没有接收卡 ID，也不查询 `GET /cards/{id}`。因此它产生的是填表数据，不是从发卡渠道取得的该卡账单记录。

同目录 [README](https://github.com/zovocard/zovo_card_cdk_auto/blob/19f41ef347cdb3cce52fde0f2b43e12f70dc6ebe/automation/README.md) 说明该原型默认使用随机测试卡号，并将接入真实卡平台材料列为后续工作。它适合说明表单自动化结构，不能证明任意生成的姓名/地址可用于真实结算，也不构成必须依赖某个外部生成器的理由。

## 5. 建议的最小独立化实现

下面是基于现有证据的实现方向。本轮已完成 provider 解析、按卡选择、缺失处理和付款执行器接线；真实卡台返回值与实际 checkout 的验收仍待有权限的环境。

1. **开卡前使用明确配置的姓名，并固定在该资金请求内。** 保持同一开卡请求重试时姓名不变。调用者可以自有管理这两个字段，不需要调用 kwRedeem 的生成服务。网页预填的随机姓名是另一入口的行为，不能使 API 请求字段消失。
2. **扩展卡详情解码，保留姓名和可选 `card_address`。** 保留空值，区分“未提供”“部分提供”“完整提供”。不要在解码器里自动补出街道、城市或国家。
3. **按卡选择账单资料。** 优先使用可确认的卡详情资料；如需人工配置，配置应绑定到平台 key 和数字卡 ID，并记录来源。不能让所有卡隐式复用一个与卡无关的随机地址。
4. **地址缺失时给出明确状态。** “需要账单资料”应发生在可能触发付款的动作前；缺失不能自动调用生成器、改成美国 DE，或改动实际付款地区。
5. **保留现有付款动作许可与对账流程。** 补齐账单字段不等于验证了 checkout 页面、3DS 或订阅成功；这几个层次分别验收。

本分支的本地账单配置能力承担“明确资料”的实现，但通过格式校验不意味着已经由发卡方验证。离线测试已覆盖完整、空缺和部分地址、不同卡/平台的独立映射、姓名不符、详情请求失败及缺失资料停止；它不能说明某 issuer 的真实数据覆盖率。

### 5.1 本轮已实现的卡详情解析

[spacex.go](../internal/provider/spacex.go) 的 `CardMaterial` 新增 `Holder Cardholder`、`HolderName string`、`Billing *BillingAddress`；[address.go](../internal/provider/address.go) 的 `BillingAddress` 新增 `Line2`，旧地址服务的行为保持原样。

`GetCardMaterial` 从取得卡号/CVV的同一次 `GET /cards/{id}` 读取姓名和六项地址资料。`HolderName` 优先取非空 `user_name`，否则以空格拼接非空 `first_name`、`last_name`；有地址时 `Billing.Name` 使用该值。缺失、null、空对象或六项均为空字符串时 `Billing=nil`，但仍保留姓名；任一地址字段非空时保留部分资料，包括只有第二行的情况。

解析层保留姓名和地址的原始字符串，不补国家、地区或邮编，也不在解析时裁掉控制字符；后续校验器才能明确拒绝非法输入。它没有为字段增加“发卡方已验证”的承诺，也没有因为有地址就允许付款。地址或姓名类型错误沿用通用契约错误，错误消息不回显返回的私人资料。

[card_billing_test.go](../internal/provider/card_billing_test.go) 使用内存 HTTP transport 返回合成资料，覆盖完整地址、全名优先、姓名拼接、缺失/null/全空地址、部分地址、第二行、控制字符保留、不同卡独立请求与错误不泄露资料。新增测试没有网络连接；整个 provider 测试包通过 `GOTOOLCHAIN=local GOPROXY=off go test ./internal/provider`。这些测试证明字段处理和请求绑定，尚未证明生产 issuer 的实际地址覆盖情况。

### 5.2 本轮已完成的 resolver 与执行器接线

[`processor/card_billing.go`](../internal/processor/card_billing.go) 优先使用当前卡详情中完整的规范化地址。资料不全时，`NewWithServices` 要求 [`CardBoundBillingService`](../internal/provider/card_bound_billing.go) 或实现相同 `BillingForCard` 接口的服务；引用键为平台配置 key 与数字卡 ID。显式配置只填缺失项，已知字段有冲突则停止，完整 issuer 资料不会被过时配置覆盖。

[`processor/checkout.go`](../internal/processor/checkout.go) 将同一次卡详情请求取得的卡材料和地址一并传到执行器。`Line2` 已通过 Go checkout Material 和本地 bridge 保留，Go/Python 填表都验证其实际回读；非空第二行没有对应字段时不会静默丢弃。州和邮编的必填性取决于真实字段 required/aria-required/validity，不再由通用执行器强制填成美国 Delaware。

[`provider/card_bound_billing_test.go`](../internal/provider/card_bound_billing_test.go)、[`processor/card_billing_test.go`](../internal/processor/card_billing_test.go) 覆盖绑定不可变、跨卡/平台拒绝、部分补齐、冲突和控制字符；后者包含从一次合成卡详情 GET 到 executor 请求的接线测试。旧 `New` / `NewWithExecutor` / `NewWithAccountService` 保持兼容地址读取路径，必须选用 `NewWithServices` 才启用严格按卡兜底。

## 6. 官方 sandbox 能在无真实资金下验证什么

官方 OpenAPI 文档开头的“代理接入前测试”声明，`https://sandbox.zovocard.com/openapi/v1` 使用独立账号、密钥和数据；开卡、充值、退款和交易均为模拟结果，不请求真实发卡渠道，也不发生真实资金变动。[固定版本官方文档](https://github.com/zovocard/zovo_card_cdk_auto/blob/19f41ef347cdb3cce52fde0f2b43e12f70dc6ebe/docs/zovocard-openapi-zh.md)

| 测试层次 | 可在无真实资金下研究/验证的内容 | 不能由此证明的内容 |
| --- | --- | --- |
| 纯离线 fixture | 姓名/地址字段解码；空值处理；按卡绑定；资金请求一致性；未就绪时拒绝付款。 | 平台实际返回什么、某 issuer 是否有地址。 |
| sandbox 只读 | 在具有沙盒权限后，验证鉴权、产品列表、余额和已有模拟卡详情；检查地址字段的存在和形状。 | 生产渠道返回值与沙盒完全一致。 |
| sandbox 模拟写入 | 按官方描述，可以验证模拟开卡/充值/退款、卡查询、幂等和模拟交易对账；先核对沙盒可用能力与测试余额机制。 | 真实银行授权、生产手续费、真实地址登记、商户接受或 3DS。 |
| 真实生产卡的只读详情 | 经单独授权读取已有卡的字段，形成脱敏 issuer/产品样本矩阵，无需新开卡即可查证地址是否提供。 | 真实付款会成功或所有其他卡都具有同样地址行为。 |

本次没有登录 sandbox，也没有创建沙盒密钥或实际执行上述接口。公开文档未说明测试余额如何领取、当前哪些 issuer 已完整模拟，以及是否模拟所有 GPT 直充事件，因此不能把这些能力列为已实测。官方帮助中心也提示沙盒可能暂不可用；这不构成自动切换到生产写操作的理由。[官方代理教程 §6.8](https://help.zovocard.com/zhcn/help/zc-reseller-illustrated/)

即使模拟卡详情返回完整地址，它也只能证明客户端对该模拟结构的处理。不能把模拟卡交给真实商户 checkout，借此声称验证了真实支付。

## 7. 仍需向平台或真实脱敏样本确认的问题

| 待确认问题 | 为什么公开材料仍不足 |
| --- | --- |
| `one`、`three`、`four` 各产品的 `card_address` 是否通常非空；存量 `two` 如何返回 | 文档只有共享 schema，没有分渠道实例；示例六项全部为空。 |
| 地址来源是上游发卡方登记值、平台默认值，还是用户配置值 | 字段名和“账单地址”说明没有描述来源或验证语义。 |
| 用户能否设置/修改卡账单地址，是否有正式 API | 目前公开请求表和路由目录未发现对应契约，不能猜测 endpoint。 |
| `country`、`state`、邮编和完整姓名的准确格式；字段何时缺失 | 全空示例无法验证编码和归一化规则。 |
| 开卡提交姓名与详情 `user_name` 不一致时，哪一个是实际付款应使用的资料 | 公共示例使用相同姓名，没有冲突语义。 |
| sandbox 当前模拟的 issuer、测试余额机制及地址数据来源 | 文档确认模拟资金操作，但未给出该覆盖矩阵。 |

这几个问题得到答案后，才能决定哪些卡可完全依靠卡台返回值，哪些卡还需要显式配置。现阶段最合理的工程投入是补齐卡详情字段和按卡绑定，而不是先寻找或编写一个随机账单地址生成器。

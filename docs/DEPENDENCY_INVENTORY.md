# kwMembership 独立化依赖清单

核验日期：2026-11-13。本文以以下固定上游提交为分析基线；同仓库相对链接用于定位实现，本分支正在增加的接口及其验收结果以实施说明和最终代码为准。这里的“已核验”指公开源码、Git 历史及仓库元数据核验，未启动真实订单、发卡、充值或付款。

目标是保留卡台的卡服务，同时自行控制账号观测、账单资料和实际付款执行。`kwMembership` 已公开 Go 工作流与 Python 浏览器执行代码，但其上游基线仍依赖 `kwRedeem` 的数据模型，以及第三方订阅查询和取消续费接口。取得这三个仓库的 fork 后，仍需补齐这些依赖，才能成为可独立启动、可验证的付款系统。

## 1. 仓库、用户 fork 与固定版本

| 公开上游 | 用户 fork | 分支与分析提交 | 定位与关系 |
| --- | --- | --- | --- |
| [zovocard/zovo_card_cdk_auto](https://github.com/zovocard/zovo_card_cdk_auto) | [7yedeng/zovo_card_cdk_auto](https://github.com/7yedeng/zovo_card_cdk_auto) | `master` · `19f41ef347cdb3cce52fde0f2b43e12f70dc6ebe` | 官方帮助中心链接的 CDK 接入站；提交说明为 `chore(release): v1.4.35`。包含卡台 API 接入、订单管理和一个填表原型。 |
| [varrge/kwMembership](https://github.com/varrge/kwMembership) | [7yedeng/kwMembership](https://github.com/7yedeng/kwMembership) | `main` · `16ceb97fc15f629c8b59df242e1baee2227e4c77` | 第三方会员履约 worker；提交说明为“处理自动化协议”。本次未发现其属于 ZovoCard 官方的证据。本项目的独立化研究在此基础上进行。 |
| [PIPIWANGLUO/PPCDK](https://github.com/PIPIWANGLUO/PPCDK) | [7yedeng/PPCDK](https://github.com/7yedeng/PPCDK) | `master` · `462578e257c3dabb1ca93fac852bb37538efaab5` | 相关 CDK 站实现；提交说明为 `feat: simplify customer recharge flow`。GitHub 上游元数据为 `fork: false`，未给出官方 parent/source，不能仅凭相似代码认定为官方仓库或官方登记的 fork。 |

用户 fork 的存在已核验；上表 SHA 固定的是本轮分析所用的上游版本，不表示 fork 的后续开发 HEAD 永久保持不变。当前工作分支为 `research/independent-executor-20261113`。

### 旧 SpaceX 与 ZovoCard 是同一仓库历史

旧地址 [spacexcard/spacex_card_cdk_auto](https://github.com/spacexcard/spacex_card_cdk_auto) 的 GitHub API 请求重定向到仓库 ID `1315088031`，该 ID 对应现有 `zovocard/zovo_card_cdk_auto`。官方历史中的 [重命名提交 0c59a2e](https://github.com/zovocard/zovo_card_cdk_auto/commit/0c59a2e5d13f6e145b81d20daef063bfdc9bb4c1) 于 2026-08-16 将 `defaultGitHubRepo`、README、部署脚本和卡台域名由 SpaceX 改为 ZovoCard。历史 fork [AlasicPlat/spacex_card_cdk_auto](https://github.com/AlasicPlat/spacex_card_cdk_auto) 的 parent/source 也指向这一仓库 ID。因此旧名可以通过现有官方仓库的 Git 历史研究，无需把它计作另一套独立付款引擎；这里的 SpaceX 与航天公司无关。

许可事实：核验时三个上游的 GitHub `license` 元数据均为 `null`；ZovoCard 与 PPCDK 的 README 写有 `MIT License`，本次树检索未找到独立 LICENSE/COPYING/NOTICE 文件，kwMembership 则未找到独立许可文件或 README 许可声明。

## 2. 直充接入与自行付款的边界

官方 [CDK 文档（固定版本）](https://github.com/zovocard/zovo_card_cdk_auto/blob/19f41ef347cdb3cce52fde0f2b43e12f70dc6ebe/docs/zovocard-cdk-zh.md) 描述发码、预检、兑换、查询结果和订单对账。通过这些接口受理直充订单，实际履约仍在卡台服务内完成。公开的 CDK 接入站不等于该服务内部付款执行引擎的完整源码。

官方仓库的 [automation/README.md（固定版本）](https://github.com/zovocard/zovo_card_cdk_auto/blob/19f41ef347cdb3cce52fde0f2b43e12f70dc6ebe/automation/README.md) 另有 Playwright 填表原型，但说明默认使用随机测试卡号，接入真实卡材料仍列作后续工作。这不能作为真实扣款成功的证据。

对上表两个固定版本逐文件计算 SHA-256，PPCDK 的 [automation 目录](https://github.com/PIPIWANGLUO/PPCDK/tree/462578e257c3dabb1ca93fac852bb37538efaab5/automation) 与官方版本对应目录的 12 个文件全部逐字节相同，目录内无新增或缺失文件。因此 PPCDK 的这个目录未提供额外付款执行能力；此结论不扩展为两个仓库整体完全相同。

`kwMembership` 更接近“自行控制执行”：Go 管理订单状态、卡库存、资金意图、付款许可和对账，Python 领取本机命令后操作浏览器。其默认运行模式仍是禁止付款的 fixture；公开执行代码的存在，不能替代真实页面与结果契约的验证。源码入口见 [processor.go](../internal/processor/processor.go)、[bridge.go](../internal/executor/bridge.go)、[live.py](../python_executor/live.py) 和 [runner.py](../python_executor/runner.py)。

## 3. 上游运行主路径与保留代码

下表描述固定上游基线及本分支保留的兼容构造路径。独立化接口的具体名称、参数和接线方式由本分支实施说明映射，不在此假定。

| 依赖或代码 | 是否进入上游主路径 | 源码证据与实际用途 | 独立化所需替代 |
| --- | --- | --- | --- |
| `kwRedeem` 目录、共享 SQLite/WAL、配置与加密格式 | 是 | [config.go](../internal/config/config.go) 强制检查其 `package.json` 并加载配置；[README](../README.md) 将订单入口、Session 恢复、管理面、初始化和 Webhook 留给 Node 项目；Go 直接读写共享表。 | 完整迁移与初始化、兼容订单生产及 Session 恢复入口、配置来源；不能只放一个同名空目录。 |
| 第三方订阅查询 | 是，默认资格检查和对账使用 | [membership.go](../internal/provider/membership.go) 向 `https://gptserve.freespaces.app/api/subscription/info` POST `token` 字段，其值为完整 Session JSON 对象；调用位置见 [eligibility.go](../internal/processor/eligibility.go)、[reconciliation.go](../internal/processor/reconciliation.go)。 | 自有账号观测实现，明确身份、套餐、币种、到期时间、续费和欠费状态及观测时间。 |
| SpaceX 取消续费接口 | 是，相关资格检查和完成/部分完成对账会调用 | [renewal.go](../internal/provider/renewal.go) 向 `https://spacexcard.com/api/v1/gpt/cancel-renewal` 提交压缩为字符串的 Session；Bearer 凭据来自 [dependencies.go](../internal/processor/dependencies.go) 的 `extension_delivery_settings`。 | 有真实接口或页面证据支持的自有取消实现；不支持时明确失败，取消后重新观测。 |
| SpaceX/ZovoCard 卡服务 | 是 | [spacex.go](../internal/provider/spacex.go)、[card_platform.go](../internal/provider/card_platform.go) 覆盖产品、卡列表、卡材料、余额、开卡、充值、冻结和交易事实。上游基线使用 `https://spacexcard.com/openapi/v1`。 | 保留为外部卡服务；独立配置并验证目标基址、凭据、请求和响应契约，不把更换域名视为兼容性已证明。 |
| Efun 卡服务 | 配置选择后使用 | [efuncard.go](../internal/provider/efuncard.go) 与 [dependencies.go](../internal/processor/dependencies.go) 提供另一卡平台实现，基址由配置给出。 | 属于可选卡服务，不是所有部署均需依赖。 |
| kwRedeem 地址 API | 是，开新卡和付款分别读取 | [address.go](../internal/provider/address.go) 读取 `API_URL` 下的 `/api/public/us-address/generate`；[payment.go](../internal/processor/payment.go) 的开卡分支调用 `Cardholder`，[checkout.go](../internal/processor/checkout.go) 调用 `Billing`。默认 `API_URL` 为 `http://127.0.0.1:4300`。 | 自有、明确提供并与卡绑定的账单资料；姓名和地址不能继续独立随机生成。 |
| Go → Python 本机执行器 | 默认 `python` 模式使用 | [main.go](../cmd/membership-worker/main.go)、[bridge.go](../internal/executor/bridge.go)、[queue.go](../internal/executor/queue.go) 在回环地址传递命令、短期材料与危险动作许可。 | 保留状态机、许可、租约和未知结果对账语义；Python 不决定业务重试。 |
| `GPTClient` 的 checkout/session-to-cookie | 未发现主程序构造或调用；属于保留 helper | [gpt.go](../internal/provider/gpt.go) 定义 `https://spacexcard.com/api/v1/gpt/checkout` 和 `/session-to-cookie`；检索非测试 Go 源码，`NewGPTClient` 仅有定义。 | 不应把这两个旧 helper 地址计作当前默认执行路径的必经依赖。 |
| `legacy-go` 浏览器执行器 | 显式配置后可运行 | [config.go](../internal/config/config.go) 和 [main.go](../cmd/membership-worker/main.go) 可选择 Go Chrome 执行器。 | 它是可选兼容执行模式，不能与上行未调用的 `GPTClient` 一并称为死代码。 |

### 源码中出现的域名并不全是外部服务依赖

Python live 执行器实际访问 `chatgpt.com`、`pay.openai.com`，并允许识别 `js.stripe.com` iframe；这些是需要实测的页面边界。`https://api.openai.com/auth` 在 [live.py](../python_executor/live.py) 中是 JWT claims 的命名空间，不是该行发起的 HTTP 请求。[README](../README.md) 中的 `apikey.vsakura.top/.../card-transactions` 是示例 Webhook 地址，不能仅凭这段文字认定 worker 会向其发送数据。`127.0.0.1:4312` 是 Go/Python 内部服务。浏览器子资源、重定向及用户配置代理的完整网络集合，无法仅靠这些静态字符串穷尽。

## 4. 三个疑似缺失项目的核验边界

| 名称 | 已找到的直接引用 | 本次公开检索结果 | 对独立化的结论 |
| --- | --- | --- | --- |
| `kwRedeem` | [README](../README.md) 要求先执行其 `npm run db:init`；[config.go](../internal/config/config.go) 检查项目目录并加载配置；大量 SQL 读写其表。 | `varrge/kwRedeem` 的仓库和树请求返回 404；仓库名检索及 `kwRedeem in:name fork:true` 未得到可核验公开项目，限定作者 README 的搜索只找到 kwMembership。 | 这是实际缺失的配套项目。本次未找到公开仓库，不能据此判断它是私有、删除、改名还是从未公开。 |
| `checkout-link-generator` | ZovoCard [automation README](https://github.com/zovocard/zovo_card_cdk_auto/blob/19f41ef347cdb3cce52fde0f2b43e12f70dc6ebe/automation/README.md) 的 `--session ../checkout-link-generator/session`。 | 精确代码检索该路径只找到上述示例。对应 [fill-checkout.js](https://github.com/zovocard/zovo_card_cdk_auto/blob/19f41ef347cdb3cce52fde0f2b43e12f70dc6ebe/automation/fill-checkout.js) 使用本项目 `lib/card`、`lib/address`、`lib/session`，没有导入或调用这个名字的项目。 | 目前只能证明示例 Session 文件路径，不能认定为必需软件依赖，也不能任意 fork 同名项目补位。 |
| `accounthub` | ZovoCard [gpt_billing.go](https://github.com/zovocard/zovo_card_cdk_auto/blob/19f41ef347cdb3cce52fde0f2b43e12f70dc6ebe/backend/internal/handler/gpt_billing.go) 使用 `ACCOUNTHUB_BASE_URL` 和 `/gpt/invoices-by-email`。 | 精确接口路径代码搜索只找到该处理器；相关作者/仓库范围检索未建立任何公开同名仓库与它的关系。 | 这是 ZovoCard 接入站可选账单查询服务，查询失败可回退卡台订阅查询；未发现它是 kwMembership 当前主路径依赖。 |

这些检索结论限于本次可访问公开仓库与上述精准引用，不代表穷尽全部私有或已改名项目。

## 5. SQLite 依赖：35 张业务表，预检只检查其中 17 张

对 `internal` 下非测试 Go 源码中的 `FROM`、`JOIN`、`UPDATE`、`INSERT INTO` 和 `CREATE TABLE` 引用提取并逐项核对，得到以下 35 个业务表名；`sqlite_master` 是 SQLite 元数据表，未计入。表名是现有实现的实际依赖清单，不是可直接运行的完整迁移定义，也不表示每一张表都会在每一种运行模式下访问。

“预检”列表示 [store.go](../internal/store/store.go) 的 `VerifySharedSchema` 是否仅检查该表名存在。功能说明依据旁列中的调用点；列、约束、索引、默认数据和触发条件仍须另行核对。

| 实际表名 | 预检 | 用途与直接源码 |
| --- | --- | --- |
| `activation_jobs` | 否 | Session 失效且未越过资金边界时取消上游任务：[session_recovery.go](../internal/processor/session_recovery.go)。 |
| `admin_audit_logs` | 否 | Intake 自动建履约的审计记录：[intake.go](../internal/processor/intake.go)。 |
| `automatic_checkout_daily_usage` | 否 | 自动付款每日额度记账/恢复：[payment.go](../internal/processor/payment.go)、[session_recovery.go](../internal/processor/session_recovery.go)。 |
| `automatic_checkout_quota_reservations` | 是 | 自动付款配额预留与释放：[payment.go](../internal/processor/payment.go)、[checkout.go](../internal/processor/checkout.go)。 |
| `automatic_checkout_scopes` | 是 | 自动接单/付款授权范围：[intake.go](../internal/processor/intake.go)、[checkout.go](../internal/processor/checkout.go)。 |
| `card_capacity_reservations` | 否 | 卡容量预留、占用、对账和释放：[payment.go](../internal/processor/payment.go)、[reconciliation.go](../internal/processor/reconciliation.go)。 |
| `card_inventory_run_items` | 否 | 库存批次的逐卡任务：[inventory.go](../internal/processor/inventory.go)。 |
| `card_inventory_runs` | 否 | 库存扫描批次、租约和状态：[inventory.go](../internal/processor/inventory.go)。 |
| `card_price_signals` | 否 | 从卡交易归集的价格信号：[inventory.go](../internal/processor/inventory.go)、[eligibility.go](../internal/processor/eligibility.go)。 |
| `card_product_policies` | 否 | 产品与卡使用策略：[eligibility.go](../internal/processor/eligibility.go)、[payment.go](../internal/processor/payment.go)。 |
| `cdkeys` | 否 | 原始兑换码关联及 Session 恢复处理：[intake.go](../internal/processor/intake.go)、[session_recovery.go](../internal/processor/session_recovery.go)。 |
| `checkout_price_contracts` | 是 | 有效套餐、币种和金额范围契约：[checkout.go](../internal/processor/checkout.go)、[payment.go](../internal/processor/payment.go)。 |
| `checkout_validation_runs` | 是 | 页面预检结果和证据：[checkout.go](../internal/processor/checkout.go)。 |
| `extension_delivery_settings` | 是 | 兼容账号路径使用的取消续费凭据：[dependencies.go](../internal/processor/dependencies.go)。 |
| `fulfillment_dependency_circuits` | 否 | 外部依赖故障与熔断状态：[dependencies.go](../internal/processor/dependencies.go)。 |
| `fulfillment_interventions` | 否 | 挑战、异常和人工介入记录：[checkout.go](../internal/processor/checkout.go)、[reconciliation.go](../internal/processor/reconciliation.go)。 |
| `funding_intents` | 否 | 持久化资金准备请求及结果：[payment.go](../internal/processor/payment.go)、[session_recovery.go](../internal/processor/session_recovery.go)。 |
| `live_canary_authorizations` | 是 | 受限真实执行的授权记录：[checkout.go](../internal/processor/checkout.go)。 |
| `managed_card_transactions` | 否 | 卡授权、入账、退款和撤销事实：[inventory.go](../internal/processor/inventory.go)、[reconciliation.go](../internal/processor/reconciliation.go)。 |
| `managed_cards` | 是 | 已管理卡及库存/容量/对账状态：[inventory.go](../internal/processor/inventory.go)、[checkout.go](../internal/processor/checkout.go)。 |
| `membership_action_auth_snapshots` | 是 | 危险动作前的卡交易快照：[checkout.go](../internal/processor/checkout.go)、[reconciliation.go](../internal/processor/reconciliation.go)。 |
| `membership_action_permits` | 是 | 每次可能触发授权的动作许可：[checkout.go](../internal/processor/checkout.go)、[bridge.go](../internal/executor/bridge.go)。 |
| `membership_card_platforms` | 否 | 多卡平台配置与库存状态；有缺表兼容回退：[dependencies.go](../internal/processor/dependencies.go)、[inventory.go](../internal/processor/inventory.go)。 |
| `membership_checkout_commands` | 是 | Go/Python 命令、领取租约、材料领取标记和结果：[queue.go](../internal/executor/queue.go)、[bridge.go](../internal/executor/bridge.go)。 |
| `membership_fulfillment_attempts` | 是 | 每阶段执行尝试：[checkout.go](../internal/processor/checkout.go)、[reconciliation.go](../internal/processor/reconciliation.go)。 |
| `membership_fulfillment_settings` | 是 | 运行开关、rollout、库存控制与旧卡台配置：[dependencies.go](../internal/processor/dependencies.go)、[checkout.go](../internal/processor/checkout.go)。 |
| `membership_fulfillments` | 是 | 履约主状态、修订号、账号锁和资金边界：[state.go](../internal/processor/state.go)、[eligibility.go](../internal/processor/eligibility.go)。 |
| `membership_intake_settings` | 是 | 接单创建时间水位线：[intake.go](../internal/processor/intake.go)。 |
| `membership_no_payment_checks` | 否 | 无付款效果的多时间点核验证据：[reconciliation.go](../internal/processor/reconciliation.go)。 |
| `membership_observations` | 否 | 账号/会员状态观测：[eligibility.go](../internal/processor/eligibility.go)、[reconciliation.go](../internal/processor/reconciliation.go)。 |
| `membership_outbox` | 否 | 工作流状态事件与依赖故障通知：[state.go](../internal/processor/state.go)、[dependencies.go](../internal/processor/dependencies.go)。 |
| `membership_payment_stages` | 是 | 分阶段付款、页面、卡及结算状态：[checkout.go](../internal/processor/checkout.go)、[reconciliation.go](../internal/processor/reconciliation.go)。 |
| `membership_processor_lease` | 是 | Go 工作流所有权、epoch 和心跳围栏：[lease.go](../internal/store/lease.go)。 |
| `products` | 否 | Intake 解析订单的会员目标套餐：[intake.go](../internal/processor/intake.go)。 |
| `redeem_orders` | 是 | 原始兑换订单、身份与加密 Session：[intake.go](../internal/processor/intake.go)、[eligibility.go](../internal/processor/eligibility.go)。 |

### 为什么 17 表通过仍不足以启动完整业务

`VerifySharedSchema` 只执行 `sqlite_master` 表名存在性查询，未检查列、外键、唯一性、索引或默认行。正常 `run()` 也没有调用这个函数；它先打开数据库、建立处理器租约表，再初始化执行器和工作流。`--check` 分支才调用它，且打开数据库会配置 WAL，因此也不应将其描述成完全无写入的 schema 审计。证据见 [main.go](../cmd/membership-worker/main.go) 与 [store.go](../internal/store/store.go)。

即使刻意建立那 17 个空表，Intake 随后仍会访问未列入检查的 `cdkeys`、`products`、`admin_audit_logs`，库存流程也会查询 `card_inventory_runs`。`membership_card_platforms` 是有缺表兼容逻辑的可选扩展；这种兼容性不能推导为库存批次表也可省略。

非测试源码中的生产建表辅助仅找到 `membership_processor_lease` 的定义；[schema_compat.go](../internal/processor/schema_compat.go) 查询的是部分列是否存在，没有补全迁移。测试中的分片 DDL 仅服务于各自场景，不能拼接后声称取得了完整生产 schema。独立化必须取得原项目迁移或逐项重建并验证自己的完整模型、约束、初始数据和兼容策略。

## 6. 当前独立启动的具体阻塞

| 阻塞 | 代码中的事实 | 必须完成的工作 |
| --- | --- | --- |
| 配置入口仍绑定 kwRedeem | [config.go](../internal/config/config.go) 要求 `KAWANG_PROJECT_ROOT/package.json`；默认数据库和维护标记位于该项目目录；读取其 `JWT_SECRET`。 | 提供明确的独立配置入口，或具备真实完整的配套项目；不能通过伪造 `package.json` 宣称独立化完成。 |
| 数据模型与默认行不完整 | 上述 35 表覆盖接单、卡库存、资金、付款及恢复；多处要求 `id='default'` 的设置，Intake 依赖时间水位线，checkout 需要 active 价格契约。 | 一套可复现迁移及初始化；订单产生、恢复和配置写入都需遵守同一状态与事务约束。 |
| 加密/账号锁与原项目共享契约 | [secure.go](../internal/secure/secure.go) 使用 `SHA-256(JWT_SECRET)` 派生 AES-GCM 密钥，接受 `ivHex:tagHex:cipherHex`；[state.go](../internal/processor/state.go) 用同一密钥生成账号锁。 | 新入口需写入完全兼容的加密字段和身份/锁信息，或明确迁移旧数据；仅换一个密钥会使已有记录不可解密。 |
| 账号观测与取消续费仍有外部实现 | 上游默认构造建立 MembershipClient/RenewalClient；旧归一化在 [fulfillment.go](../internal/domain/fulfillment.go) 中依赖第三方 envelope 和特定空值语义。 | 自有实现需有真实响应样本和字段映射；未知套餐或空字段不得当作免费/无续费，取消不支持时停止相关流程，成功后仍需重新观测。 |
| 开卡姓名和付款账单没有持久绑定 | [address.go](../internal/provider/address.go) 每次重新请求生成地址；开卡和支付在不同调用点分别读取。它只做字段格式和 `US/DE` 检查。 | 使用明确提供的持卡人与账单资料，并按实际卡绑定；格式通过不能证明它与发卡方记录一致，多卡异址不能共用未区分的全局资料。 |
| 默认 Python 执行器只做 fixture | [runner.py](../python_executor/runner.py) 默认 `fixture`；payment 命令返回 `FIXTURE_PAYMENT_DISABLED`。live 需显式环境开关，还受 Go 的数据库 Gate、价格契约、额度和许可约束。 | 安装并验证运行依赖，完成下节的真实页面与对账证据；把环境变量设为 live 不是功能验收。 |
| 实际页面契约未获验证 | [live.py](../python_executor/live.py) 读取 `data-kw-*`、部分 `data-testid` 和 meta 标记；创建结账请求以及 [checkout.go](../internal/processor/checkout.go) 的价格选择固定 PH/PHP 流程。 | 用真实可授权访问的页面与接口证据验证；不能向真实 DOM 注入自定义标记制造“识别成功”，也不能由原有常量推定当前所有套餐/地区都可用。 |

## 7. 适合本轮离线完成的独立化范围

最有价值的范围是把账号和账单依赖变成明确注入的能力，并使卡台基址可配置；继续保留既有工作流状态、持久命令、卡交易快照和动作许可。这样可以验证“选定自有实现后，旧第三方查询、续费和随机地址路径不会被偷偷回退调用”，而不需要提前构造猜测的生产接口。

账号能力的挂接点是 [eligibility.go](../internal/processor/eligibility.go) 与 [reconciliation.go](../internal/processor/reconciliation.go) 中的观测和取消续费流程。离线验收应覆盖身份不符、未知套餐、缺失布尔字段、过期观测、付费币种/到期证据缺失，以及取消之后仍显示续费的情况。失败不能自动变为 `free` 或继续付款；自有实现错误也不应触发旧服务 fallback。

账单能力的挂接点是 [payment.go](../internal/processor/payment.go) 的开卡请求构建和 [checkout.go](../internal/processor/checkout.go) 的材料准备。一个小型本地配置实现可以在不访问地址生成服务的情况下返回明确的姓名与地址，并拒绝缺字段；这只完成替代和格式校验。卡片与账单实际匹配、不同卡的资料选择、最终入口接线与配置生命周期仍需单独解决。

付款执行器的离线验证应针对许可有效期、单次控制激活、超时后对账、重启丢失材料上下文后的拒绝重放等行为。单元测试使用人工 fixture 是有效的协议测试；它无法证明这些选择器存在于真实支付页面，也无法证明真实 3DS 或结算已经成功。

## 8. 从离线组件到真实运行仍需的证据

| 缺口 | 需要的具体证据 | 对应验收 |
| --- | --- | --- |
| 完整数据库初始化 | 原 kwRedeem 迁移/初始化源码，或自建的完整 schema、约束与种子数据定义；包括订单产生和 Session 恢复契约。 | 从全新数据库初始化，完整工作流可推进；不依赖散落测试 DDL 或手工插入残缺记录。 |
| 自有会员观测 | 经授权账号的脱敏只读样本、当前身份字段、各套餐/币种/到期/续费/欠费字段及时间含义。 | 缺失与明确 false 区分；身份绑定；过期或不完整观测被拒绝，成功后的目标套餐可重新确认。 |
| 自有取消续费 | 实际可用接口或页面动作证据、响应语义，以及取消后再次查询的样本。 | 未实现时明确报错；不虚构 endpoint；只有重新观测到明确关闭才推进相应状态。 |
| 真实卡账单资料 | 发卡方返回或明确确认的持卡人/账单资料，与卡平台 key 和上游卡 ID 的映射。 | 开卡和付款使用一致资料；多卡异址正确路由；不把生成的地址或配置自述当作发卡方事实。 |
| 真实 checkout 页面样本 | 经授权取得并脱敏的当前页面/iframe 结构、套餐/国家/币种/金额来源、按钮和错误状态。 | 受支持页面能识别；未知结构拒绝；不注入伪标记；价格契约与所见事实一致。 |
| 3DS、验证码、短信/银行验证 | 实际挑战状态、可用接管通道、挑战结束和超时后的页面/卡交易样本。 | 明确暂停与恢复条件；共享硬截止；超时或失败后继续对账，不重复激活付款控件，也不绕过挑战。 |
| 付款结果与不确定结果 | 卡授权、结算、退款/撤销样本及对应账户订阅变化；网络中断、进程退出等时序。 | 浏览器成功文案不单独决定完成；以卡交易和账号观测共同确认，未知结果保留对账状态，禁止盲目重付。 |

本清单将“源码可读”“离线测试通过”“独立启动成功”“真实付款与对账验证”作为不同证据层次。当前能确认的是公开源码结构和具体依赖；是否已补齐各层，应由本分支最终实现说明和实际验收结果逐项给出。

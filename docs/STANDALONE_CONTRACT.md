# 独立运行的数据与预检契约

审计基线：`0a28f6dbb0e315479980283de81ed5e9db762300`，2026-11-13。本文区分已存在的代码、此次新增的无付款 CLI，以及尚未实施的独立业务数据方案。下面的表结构矩阵是代码依赖清单，不是已经验证的 kwRedeem 生产迁移。

## 1. 可以独立运行到哪里

最小纵向范围有两种入口。已有 Session 的使用者可以从标准输入提供 Session 和显式价格契约，先本地校验，再在隔离 Chrome 中验证实际邮箱、创建 Plus checkout 会话、读取页面事实并输出脱敏结果。没有可用 Session cookie 的使用者可以显式选择 `--interactive-preflight`，仅提供预期邮箱和价格契约，在新开的独立浏览器中亲自登录并进入 checkout，然后读取同样的页面事实。

这两条路径直接使用 `checkout.ChromeExecutor.Execute` 的 `ModeSessionPreflight` 或 `ModeInteractivePreflight`。它们需要 **0 张业务表**，不需要 kwRedeem checkout、API 地址、卡台凭据、续费 token、账单服务、数据库中的订单、Python bridge 或共享 worker。Session 入口固定 `Stage=plus`、`TargetTier=plus`；交互入口允许 Plus 或 Go，并要求 stage、target 和价格契约 tier 一致。两者始终 `Material=nil`、`Guard=nil`。Session 入口的 `OnHandoff=nil`；交互入口只装入输出固定状态码的 handoff，并且 `Session`、`Cookies` 都为空。执行器不会进入付款材料填写、progression、submit 或升级执行分支。Go 的限制与证据范围见 [GO_PLAN_PREFLIGHT.md](GO_PLAN_PREFLIGHT.md)。

`--run` 不是纯只读网络操作：现有执行器会读取 `/api/auth/session`，可能查询 subscription，然后向 `/backend-api/payments/checkout` 发出 POST 创建 checkout 会话。它不创建卡、不充值卡、不取消续费、不提交付款，也不以一次预检代替最终到账证据。站点拒绝访问、要求验证或结构不识别时应停止，不能改变访问路径绕过环境限制。

`--interactive-preflight` 打开 `https://chatgpt.com` 后等待使用者在该浏览器内登录并导航到 checkout。该执行分支不调用 `preparePlusCheckoutJS`，不会由 CLI 自动 POST 创建 checkout；使用者的页面操作仍可能创建远程 checkout 会话，因此它同样不能称为只读网络模式。程序仅把 `/api/auth/session` 的邮箱结果用于内存中的身份比对，不导出账号 token。网站正常登录仍会在本次临时浏览器 profile 中产生认证 cookie；退出时由 allocator 清理 profile。预检通过不证明账号原先为免费、不证明自动续费状态或取消能力。操作步骤止于 checkout 页面；操作者应等待诊断结果，不填写卡或点击支付。程序缺少付款能力并不等于浏览器能拦截使用者的所有手动操作。

依据：[checkout/types.go](../internal/checkout/types.go)、[checkout/chrome.go](../internal/checkout/chrome.go) 的 `validateRequest`、`executeSessionPreflight`、`executeInteractivePreflight`、`openPlusCheckout`、`verifySessionIdentity`，以及 [checkout/scripts.go](../internal/checkout/scripts.go) 的 `preparePlusCheckoutJS`、`authenticatedEmailJS`。

### 为什么不直接启动整个 worker

`Processor.RunOnce` 会依次进入 intake、inventory、eligibility、payment、checkout、reconciliation；它不是预检专用调度器。现有 `checkoutPreflight` 成功后会把普通订单推进 `FUNDING_READY`。现有 eligibility 的 `preflightReadiness` 还要求库存初始化完成、卡行情可用和唯一有效价格契约。不能通过构造假库存或伪造 kwRedeem 文件来跳过这些条件。

Python `executor.Bridge` 是共享数据库 command/lease 执行桥。即使只跑 preflight，也需要真实的 fulfillment、command revision 和 material claim 校验；其构造函数还会将遗留 leased/action-required commands 标记为 expired。因此它不适合作为此次零业务表的独立入口。

依据：[processor/processor.go](../internal/processor/processor.go)、[processor/checkout.go](../internal/processor/checkout.go)、[processor/eligibility.go](../internal/processor/eligibility.go)、[executor/bridge.go](../internal/executor/bridge.go)。

## 2. 独立 CLI 的边界

新增入口为 `cmd/membership-preflight`，与默认 `cmd/membership-worker` 分开。它不调用 `config.Load`，不加载 `.env`，不探测 `KAWANG_PROJECT_ROOT`，不打开数据库，也不改变默认 worker 启动语义。

| 模式 | 输入 | 本地动作 | 网络动作 | 结果的含义 |
| --- | --- | --- | --- | --- |
| 默认 / `--check` | 封装请求 JSON | 有界解析、显式价格契约、Session cookie 可构造性、声明邮箱一致性 | 无 | `remoteVerified=false`；只说明本地输入契约成立 |
| `--inspect-session` | 原始 Session 对象 | 只检查预定义字段存在性、JSON 类型和可解析到期时间 | 无 | 不返回邮箱、用户 ID、令牌或未知字段名称；`remoteVerified=false` |
| `--run` | 与 check 相同 | 同样先做全部本地校验，然后构造固定 Session preflight 请求 | 可能创建 checkout 会话 | 只有返回受支持、价格一致的预检页面才可报告通过；不代表支付或订阅完成 |
| `--interactive-preflight` | 仅 `expectedEmail`、`targetTier`、`priceContract` | 严格校验元数据，强制 headed、新建临时 profile | 打开站点、读取身份与页面；使用者导航可能创建 checkout | 身份及 Plus/PH/PHP/价格范围全部一致才通过；不收集或导出 Session/token，不提交付款 |

普通 Session 封装请求只接受 `session`、`expectedEmail`、`targetTier`、`priceContract`。交互入口只有后三个元数据字段，明确拒绝 `session`、`sessionToken`、`accessToken`、`cookies`、卡材料及 profile/checkout 地址等其他字段。两者的价格契约只接受 `id`、`version`、`tier`、`currency`、`minAmount`、`maxAmount`，且 tier/target 必须为 `plus`，currency 必须为 `PHP`，金额为有限正数并满足 `maxAmount >= minAmount`。价格必须来自使用者已经核实的契约；CLI 不写死实际套餐价格，也不把页面价格自动登记为经过批准的价格契约。

封装请求及价格契约的未知字段、尾随 JSON、过大输入均拒绝。Session 本体沿用现有 provider 的异构对象契约：可包含 provider 不使用的服务端字段，但不会把未知键或值写入诊断。`--inspect-session` 是结构检查工具，不负责验证 Session 有效性。

所有 stdout 业务结果均为有界 JSON 白名单字段；错误只输出显式允许的安全 code，不输出原始错误、URL、Session、cookie、邮箱、用户 ID 或卡材料。交互 handoff 的 stderr 只写固定 `INTERACTIVE_LOGIN_REQUIRED`，不转发页面对象或身份。浏览器工厂显式关闭该 CLI 的 chromedp `Logf`、`Errorf`、`Debugf` 三条异步日志通道，避免原始 CDP 事件绕过返回错误的脱敏；不重定向全局 stderr，不改变生产 worker 的日志配置。浏览器运行选项通过显式 flags 指定。测试只使用合成 Session、fake executor、回环 mock CDP 和本地校验，不访问真实站点。

### 命令与输入

```sh
go run ./cmd/membership-preflight --check < /secure/preflight-request.json
go run ./cmd/membership-preflight --inspect-session < /secure/session.json
go run ./cmd/membership-preflight --run --chrome-path /usr/bin/chromium < /secure/preflight-request.json
go run ./cmd/membership-preflight --interactive-preflight --chrome-path /usr/bin/chromium < /secure/interactive-metadata.json
```

后两条会启动浏览器。Session `--run` 默认使用 headless；它的 `--headed` 选项只切换为显示模式，不添加人工登录 handoff。`--interactive-preflight` 强制 headed，即使显式传入 `--headed=false` 也保持 headed；它需要操作者可访问的私有桌面，例如仅向操作者开放的 Xvfb/VNC 显示环境。每次执行都新建 profile，不能指定已有 profile 或复用现有账号会话。`--proxy-server` 只接受显式的无凭据 HTTP/SOCKS5 host:port。CLI 不接受 Session 的命令行参数，避免把凭据写入进程参数和 shell history。

`--timeout` 默认 90s，允许 1s 至 5m。其实际语义取决于模式：

- Session `--run`：从进入执行器起的外层浏览器预算采用该值；单次页面操作还可能受到已有 30s 页面超时限制。
- `--interactive-preflight`：沿用现有 `challengeTimeout=5m`。首次导航和 handoff 完成后，登录与进入 checkout 的轮询窗口最多 5m；执行器外层预算为 `max(--timeout, 5m + 30s)`。由于本 CLI 接受的 `--timeout` 最大为 5m，这个模式的外层预算实际固定为 5m30s。把 `--timeout` 调低不会缩短交互窗口或外层预算，启动/初始导航耗时也会占用外层预算。
- 上述浏览器预算不包含等待 stdin 完整输入和 EOF 的时间，也不是清理结束的精确墙钟上限。stdin 等待、浏览器执行均可用 SIGINT/SIGTERM 取消；输入取消后输出 `PREFLIGHT_CANCELLED`，不会因管道仍保持打开而挂住。

交互元数据示例结构如下。这里的账号和金额仍是合成占位值，真实预检前应由使用者提供已核实的预期账号和价格契约：

```json
{
  "expectedEmail": "buyer@example.invalid",
  "targetTier": "plus",
  "priceContract": {
    "id": "synthetic-contract",
    "version": 1,
    "tier": "plus",
    "currency": "PHP",
    "minAmount": 100,
    "maxAmount": 200
  }
}
```

默认 `--check` 继续要求 Session；不会把这个不含 Session 的元数据对象自动解释成交互请求。交互命令开始后，在新开的浏览器中直接完成登录，并保持在预期账号下进入 checkout；程序观察到受支持的页面即输出结果并关闭本次浏览器。

以下是**仅用于本地 `--check` 的合成夹具**，账号、token 和价格范围均不是实际账号或商品价格：

```json
{
  "session": {
    "sessionToken": "synthetic-not-a-real-session",
    "user": {"email": "buyer@example.invalid"},
    "expires": "2099-01-01T00:00:00Z"
  },
  "expectedEmail": "buyer@example.invalid",
  "targetTier": "plus",
  "priceContract": {
    "id": "synthetic-contract",
    "version": 1,
    "tier": "plus",
    "currency": "PHP",
    "minAmount": 100,
    "maxAmount": 200
  }
}
```

本地成功结果的 `code` 为 `PREFLIGHT_INPUT_VALID`，`inputValid=true`，但 `remoteVerified=false`、`preflightPassed=false`、`paymentAllowed=false`。真实运行只有受支持的 Plus/PH/PHP checkout 页面与显式价格范围一致时才输出 `PREFLIGHT_PASSED`。`--inspect-session` 的 `inputValid` 保持 false，因为它不检查完整预检请求；`sessionShape.objectValid` 仅表示原始输入是单个有效 JSON 对象。

退出码：`0` 为当前模式成功（或固定帮助文本），`2` 为参数/输入/浏览器配置无效，`1` 为执行失败或输出失败。执行失败的 stdout 仍仅含安全 code；不打印底层异常文本。`CHECKOUT_UI_UNSUPPORTED`、`SECURITY_CHALLENGE_REQUIRED`、`SESSION_COOKIE_MISSING` 等错误是停止条件，不是自动换路重试的指令。

### Session 的精确输入要求

现有 [provider/session.go](../internal/provider/session.go) 的 `BrowserSessionFromJSON` 要求：

- Session 是 JSON 对象，总大小不超过 256 KiB。
- `user.email` 或顶层 `email` 是有效邮箱；CLI 另外要求它与 `expectedEmail` 一致。这一步仍是本地声明一致性，远程验证发生在浏览器内。
- `sessionToken` 必须存在，长度不超过 64 KiB，不含前后空格、换行或 NUL。其内容被按 3,936 字节切分为 `__Secure-next-auth.session-token` cookie。
- 可选 `expires` 必须是可解析且未过期的 RFC3339 时间。
- 只有 `accessToken` 的 JSON 不满足浏览器 Session 契约，返回 `SESSION_COOKIE_MISSING`。不调用第三方转换服务，也不把 access token 冒充 session cookie。

直接读取 `/api/auth/session` 得到的对象是否包含 `sessionToken`，必须根据授权会话的实际结构判断，不能仅凭 URL 推定完整输入可用。

## 3. 单一 SQLite 的后续方案

### 3.1 当前状态

本 CLI 不创建任何数据库。完整 worker 仍读取同一个配置的 SQLite，并使用 `BEGIN IMMEDIATE` 与持久化 lease epoch 做 fencing。当前生产 Go 代码只拥有 `membership_processor_lease` 的建表语句，其余业务表没有对应的生产 migration 文件。

`VerifySharedSchema` 只检查 17 个表名存在；下面的 SQL 依赖清单覆盖 35 个业务表，此外还有 SQLite 的 `sqlite_master` 元数据。表名检查不能证明列、唯一键、默认值、CHECK、外键、触发器、历史数据或外部生产者契约完整。

依据：[store/store.go](../internal/store/store.go)、[store/lease.go](../internal/store/lease.go)。

### 3.2 如需持久化独立预检请求

以下是**后续设计，尚未建表、未接入代码**。它只保存不付款开发请求及证据，不伪装成可供生产 worker 消费的 `redeem_orders` / `membership_fulfillments`。

只选择一个 SQLite 文件作为运行时事实来源。独立模式的请求、证据、迁移版本在该文件内，不建第二套 membership 状态库，也不建立 shadow dispatch queue 或 callback projection。既有 kwRedeem 共享部署保持原契约；从共享部署转为独立部署必须显式迁移所有权，不双写两库。

| 拟定表 | 精确最小字段 | 约束与默认值 | 用途 |
| --- | --- | --- | --- |
| `standalone_schema_versions` | `version INTEGER`、`scope TEXT`、`checksum TEXT`、`applied_at TEXT` | PK `(scope,version)`；全部 NOT NULL；无自动业务默认值；scope 固定 `preflight-only` | 证明只安装预检 schema，禁止把它认作完整 worker schema |
| `standalone_preflight_requests` | `id TEXT`、`client_request_id TEXT`、`request_fingerprint TEXT`、`target_tier TEXT`、`price_contract_json TEXT`、`state TEXT`、`state_revision INTEGER`、`created_at TEXT`、`started_at TEXT`、`finished_at TEXT`、`error_code TEXT` | `id` PK NOT NULL；`client_request_id` UNIQUE NOT NULL；fingerprint/target/contract/state/created 必填；revision NOT NULL DEFAULT 0；CHECK target=`plus`；CHECK state 属于 `accepted/running/passed/failed/uncertain`；contract 必须 `json_valid`；其余时间和 error 可空 | 同一请求的幂等入口与终态，不含可付款状态 |
| `standalone_preflight_evidence` | `request_id TEXT`、`revision INTEGER`、`observed_at TEXT`、`remote_verified INTEGER`、`sanitized_facts_json TEXT`、`safe_error_code TEXT` | PK `(request_id,revision)`；FK request→requests；前五列 NOT NULL；remote_verified CHECK 0/1；facts必须 `json_valid`；禁止原始 response/URL/cookie/邮箱；无业务默认值 | 保存已经过输出白名单处理的观察 |

开发入口接收一次性 Session 于内存；以上三表不持久化 Session。请求 fingerprint 使用随机独立服务器密钥的 HMAC，绑定会话和价格契约而不存原文，也不输出裸 token hash。重试需要重新提供输入；进程在创建 checkout 会话后丢失上下文时标记 `uncertain`，不自动重放网络操作。后续若需要 Session 持久化，必须另外设计密钥生命周期、过期删除和恢复接口；不得默认把新请求中的 Session 落明文。

该拟定 schema 只用于明确标识的预检入口，**不能传给 `membership-worker`**。完整 worker 独立化需要迁移下面 35 表及全部外部订单写入契约，不能借用这三表宣称业务完整。

### 3.3 将来的真正订单入口

生产订单入口必须在同一事务中完成：鉴权与租户绑定、幂等请求指纹、目标套餐/价格契约绑定、订单与 fulfillment 创建、加密 Session 写入或安全引用、账号锁、可审计状态初值。它不能直接把 state 设为 `FUNDING_READY` 或 `CHECKOUT_EXECUTION_WAIT`；不能接受客户端指定 `run_mode=automatic`、authorization、permit、余额、卡材料、完成状态或到账证据。

付款前仍必须具备库存、可花余额、持久化 funding intent 幂等键、卡交易基线快照、卡容量预留、价格契约、canary/automatic gate、quota、lease fencing。付款结果未知必须走 reconciliation，不能因请求超时重新付款。这个订单入口尚未实现；当前 CLI 的输入仅为一次预检请求。

## 4. 迁移前必须解决的具体缺口

1. **唯一生产 DDL 是 lease 表。** 测试 fixture 只包含各测试需要的列，合并它们仍不足以启动 worker；有些声明之间相互不同。
2. **多卡台不是只加一张配置表。** `membership_card_platforms` 存在后，inventory 查询会同时使用 `card_inventory_runs.provider_key` 和 `managed_cards.provider_key`；部分资金表通过 `tableHasColumn` 兼容，部分路径直接依赖新列。必须作为同一个 schema capability 升级，不能只建配置表。
3. **资金和 Permit 列在 fixture 中缺失。** `membership_action_permits` 的 authorization、control、expiry 等列，以及 payment stage 的 page facts/progression 时间列，是生产 SQL 的真实依赖。
4. **canary 表完全没有测试 DDL。** 只能从生产 SELECT/UPDATE 得到使用列，不能据此推定全部约束或签发入口。
5. **唯一键存在明确差异。** 测试 `fulfillment_interventions` 分别使用三列和四列唯一键；`automatic_checkout_daily_usage`、`card_price_signals` fixture 没有唯一键，但生产 UPSERT 明确需要复合唯一键。
6. **默认值与显式 INSERT 初值不同。** 代码写入 `'prepared'`、`'queued'`、`'reserved'` 不证明数据库具有该 DEFAULT；未显式提供的列必须依据正式迁移确定。
7. **加密和账号锁需要版本化。** 当前 `secure.Decrypter` 使用 SHA-256 派生 AES-256-GCM key，序列化为 `ivHex:tagHex:cipherHex`；账号锁使用相同 secret 的 HMAC-SHA256 和固定域前缀。独立配置不能悄悄更换密钥导致旧 Session 解密失败或账号锁分裂。
8. **现有 --check 不是严格只读数据库检查。** worker `--check` 调用 `store.Open`，其连接参数设置 WAL；独立 schema inspector 若实现应专门使用只读连接、query_only、明确的表/列/唯一键 capability 检查，而不是复用 `Open` 后忽略副作用。

依据：[processor/schema_compat.go](../internal/processor/schema_compat.go)、[processor/inventory.go](../internal/processor/inventory.go)、[processor/payment.go](../internal/processor/payment.go)、[processor/checkout.go](../internal/processor/checkout.go)、[secure/secure.go](../internal/secure/secure.go)、[processor/state.go](../internal/processor/state.go)、[cmd/membership-worker/main.go](../cmd/membership-worker/main.go)。

## 5. 表、列、约束与默认值矩阵

矩阵是本基线中所有 Go 生产 SQL 表引用、测试 CREATE TABLE 及生产 lease DDL 的并集，并对动态 SQL 的 `provider_key`、常量列清单和 INSERT/UPDATE 补充人工核对。

标记：`T` = 测试 DDL 实际声明；`P` = 生产 DDL；`Q` = 生产 SQL 使用、但测试及生产 DDL 均未提供该列声明。Q 的类型依据 Scan/写入值给出实现推断，**不是上游迁移证据**。`NN` 表示测试或生产声明 NOT NULL；`可空` 表示相应 DDL 未声明 NOT NULL；`有差异` 表示多个 fixture 不一致。SQLite 的 TEXT PRIMARY KEY 不在这些 fixture 中自动显示为 NOT NULL，不能把 PK 标签误作已经验证的 nullability。`无声明` 表示未出现 DEFAULT，不代表应用层没有初值。

每个表的唯一键分成 DDL 已观察值与生产 UPSERT 强制要求。外键、CHECK、触发器若没有证据即保持未知；此矩阵不会补造它们。对于只有测试读取/构造而当前生产 SQL 不使用的列，仍保留在清单中并标明其来源，避免把测试断言误写成生产强制规则。

<!-- BEGIN GENERATED CONTRACT MATRIX -->
### 5.1 `activation_jobs`

只在付款前 Session 失效时取消 pending/processing 作业、清除锁；本仓库没有它的生产创建/调度入口。

生产引用：[internal/processor/session_recovery.go](../internal/processor/session_recovery.go)。

DDL证据：[internal/processor/eligibility_dependency_test.go](../internal/processor/eligibility_dependency_test.go)、[internal/processor/eligibility_test.go](../internal/processor/eligibility_test.go)。

观察到的PK/UNIQUE：`(id)`。

| 列 | 类型/证据 | NOT NULL / PK | DEFAULT证据 |
| --- | --- | --- | --- |
| `id` | `TEXT` T | 可空；PK | 无声明 |
| `order_id` | `TEXT` T | NN | 无声明 |
| `status` | `TEXT` T | NN | 无声明 |
| `last_error` | `TEXT` T | 可空 | 无声明 |
| `locked_at` | `TEXT` T | 可空 | 无声明 |
| `locked_by` | `TEXT` T | 可空 | 无声明 |
| `updated_at` | `TEXT` T | 可空 | 无声明 |

### 5.2 `admin_audit_logs`

intake 写 membership.fulfillment.auto_create；detail 仅订单号和目标 tier。不是可推断所有 kwRedeem 审计字段的完整模型。

生产引用：[internal/processor/intake.go](../internal/processor/intake.go)。

DDL证据：[internal/processor/intake_test.go](../internal/processor/intake_test.go)。

观察到的PK/UNIQUE：`(id)`。

| 列 | 类型/证据 | NOT NULL / PK | DEFAULT证据 |
| --- | --- | --- | --- |
| `id` | `TEXT` T | 可空；PK | 无声明 |
| `action` | `TEXT` T | NN | 无声明 |
| `actor` | `TEXT` T | NN | 无声明 |
| `resource_type` | `TEXT` T | NN | 无声明 |
| `resource_id` | `TEXT` T | 可空 | 无声明 |
| `detail` | `TEXT` T | 可空 | 无声明 |
| `created_at` | `TEXT` T | NN | 无声明 |

### 5.3 `automatic_checkout_daily_usage`

生产 ON CONFLICT(scope_id,business_date) 明确要求该复合 PK/UNIQUE，fixture 未提供。business_date 的时区语义由 payment 代码决定，不能按 UTC 自行另建账。

生产引用：[internal/processor/payment.go](../internal/processor/payment.go)、[internal/processor/session_recovery.go](../internal/processor/session_recovery.go)。

DDL证据：[internal/processor/eligibility_dependency_test.go](../internal/processor/eligibility_dependency_test.go)、[internal/processor/eligibility_test.go](../internal/processor/eligibility_test.go)。

观察到的PK/UNIQUE：无DDL证据。

| 列 | 类型/证据 | NOT NULL / PK | DEFAULT证据 |
| --- | --- | --- | --- |
| `scope_id` | `TEXT` T | 可空 | 无声明 |
| `business_date` | `TEXT` T | 可空 | 无声明 |
| `order_units` | `INTEGER` T | 可空 | 无声明 |
| `risk_reserved_usd` | `REAL` T | 可空 | 无声明 |
| `updated_at` | `TEXT` T | 可空 | 无声明 |

### 5.4 `automatic_checkout_quota_reservations`

生产按 fulfillment_id 查唯一预留，并写 reserved_at；业务需防重复预留。释放与 daily_usage 扣减必须同事务，不允许把付款暴露后的预留当未用额度释放。

生产引用：[internal/processor/checkout.go](../internal/processor/checkout.go)、[internal/processor/payment.go](../internal/processor/payment.go)、[internal/processor/session_recovery.go](../internal/processor/session_recovery.go)。

DDL证据：[internal/processor/eligibility_dependency_test.go](../internal/processor/eligibility_dependency_test.go)、[internal/processor/eligibility_test.go](../internal/processor/eligibility_test.go)。

观察到的PK/UNIQUE：`(id)`。

| 列 | 类型/证据 | NOT NULL / PK | DEFAULT证据 |
| --- | --- | --- | --- |
| `id` | `TEXT` T | 可空；PK | 无声明 |
| `scope_id` | `TEXT` T | NN | 无声明 |
| `fulfillment_id` | `TEXT` T | NN | 无声明 |
| `business_date` | `TEXT` T | NN | 无声明 |
| `order_units` | `INTEGER` T | NN | 无声明 |
| `risk_reserved_usd` | `REAL` T | NN | 无声明 |
| `state` | `TEXT` T | NN | 无声明 |
| `released_at` | `TEXT` T | 可空 | 无声明 |
| `reserved_at` | `TEXT` Q（推断） | 未知 | 未知 |

### 5.5 `automatic_checkout_scopes`

active 范围绑定 site/product/tier、adapter、price contract 和 daily limits；查询按 revision 选择。状态/激活时刻不是客户端可自行开启的付款能力。正式签发与唯一有效范围约束未在本仓库迁移中体现。

生产引用：[internal/processor/checkout.go](../internal/processor/checkout.go)、[internal/processor/intake.go](../internal/processor/intake.go)、[internal/processor/payment.go](../internal/processor/payment.go)。

DDL证据：[internal/processor/intake_test.go](../internal/processor/intake_test.go)。

观察到的PK/UNIQUE：`(id)`。

| 列 | 类型/证据 | NOT NULL / PK | DEFAULT证据 |
| --- | --- | --- | --- |
| `id` | `TEXT` T | 可空；PK | 无声明 |
| `site_id` | `TEXT` T | NN | 无声明 |
| `product_id` | `TEXT` T | NN | 无声明 |
| `tier` | `TEXT` T | NN | 无声明 |
| `status` | `TEXT` T | NN | 无声明 |
| `activated_at` | `TEXT` T | 可空 | 无声明 |
| `adapter_version` | `TEXT` Q（推断） | 未知 | 未知 |
| `price_contract_id` | `TEXT` Q（推断） | 未知 | 未知 |
| `revision` | `INTEGER` Q（推断） | 未知 | 未知 |
| `scope_key` | `TEXT` Q（推断） | 未知 | 未知 |
| `daily_order_limit` | `INTEGER` Q（推断） | 未知 | 未知 |
| `daily_risk_limit_usd` | `REAL` Q（推断） | 未知 | 未知 |

### 5.6 `card_capacity_reservations`

fixture 观察到 UNIQUE(fulfillment_id)。多平台 provider_key 为条件列；slot_index 及活跃 reservation 的唯一容量语义依赖事务与域规则。未观察到正式 partial unique index。

生产引用：[internal/executor/bridge.go](../internal/executor/bridge.go)、[internal/processor/eligibility.go](../internal/processor/eligibility.go)、[internal/processor/inventory.go](../internal/processor/inventory.go)、[internal/processor/payment.go](../internal/processor/payment.go)、[internal/processor/reconciliation.go](../internal/processor/reconciliation.go)、[internal/processor/session_recovery.go](../internal/processor/session_recovery.go)。

DDL证据：[internal/executor/bridge_test.go](../internal/executor/bridge_test.go)、[internal/processor/eligibility_dependency_test.go](../internal/processor/eligibility_dependency_test.go)、[internal/processor/eligibility_test.go](../internal/processor/eligibility_test.go)、[internal/processor/payment_test.go](../internal/processor/payment_test.go)、[internal/processor/reconciliation_test.go](../internal/processor/reconciliation_test.go)。

观察到的PK/UNIQUE：`(fulfillment_id)`, `(id)`。

| 列 | 类型/证据 | NOT NULL / PK | DEFAULT证据 |
| --- | --- | --- | --- |
| `id` | `TEXT` T | 可空；PK/差异 | 无声明 |
| `state` | `TEXT` T | 有差异 | 无声明 |
| `card_id` | `TEXT` T | 可空 | 无声明 |
| `fulfillment_id` | `TEXT` T | 有差异 | 无声明 |
| `target_lane` | `TEXT` T | 有差异 | 无声明 |
| `released_at` | `TEXT` T | 可空 | 无声明 |
| `release_evidence_revision` | `INTEGER` T | 可空 | 无声明 |
| `planned_product_code` | `TEXT` T | 可空 | 无声明 |
| `slot_index` | `INTEGER` T | 可空 | 无声明 |
| `reserved_at` | `TEXT` T | NN | 无声明 |
| `consumed_at` | `TEXT` T | 可空 | 无声明 |
| `provider_key` | `TEXT` Q（推断） | 未知 | 未知 |

### 5.7 `card_inventory_run_items`

PK(run_id,upstream_card_id)；生产 pending 重试还需要 next_retry_at/error_code，fixture 缺失。attempt_count 在 INSERT 显式写 0，不证明有 DEFAULT 0。

生产引用：[internal/processor/inventory.go](../internal/processor/inventory.go)。

DDL证据：[internal/processor/inventory_dependency_test.go](../internal/processor/inventory_dependency_test.go)、[internal/processor/inventory_freeze_test.go](../internal/processor/inventory_freeze_test.go)。

观察到的PK/UNIQUE：`(run_id,upstream_card_id)`。

| 列 | 类型/证据 | NOT NULL / PK | DEFAULT证据 |
| --- | --- | --- | --- |
| `run_id` | `TEXT` T | NN；PK | 无声明 |
| `upstream_card_id` | `INTEGER` T | NN；PK | 无声明 |
| `status` | `TEXT` T | NN | 无声明 |
| `attempt_count` | `INTEGER` T | NN | 无声明 |
| `updated_at` | `TEXT` T | NN | 无声明 |
| `error_code` | `TEXT` Q（推断） | 未知 | 未知 |
| `next_retry_at` | `TEXT` Q（推断） | 未知 | 未知 |

### 5.8 `card_inventory_runs`

状态 discovering/reconciling/completed 与 locked_at/locked_by 控制扫描恢复。平台表存在后依赖 provider_key；不是所有查询都有旧列兼容分支。

生产引用：[internal/processor/inventory.go](../internal/processor/inventory.go)。

DDL证据：[internal/processor/inventory_dependency_test.go](../internal/processor/inventory_dependency_test.go)、[internal/processor/inventory_freeze_test.go](../internal/processor/inventory_freeze_test.go)。

观察到的PK/UNIQUE：`(id)`。

| 列 | 类型/证据 | NOT NULL / PK | DEFAULT证据 |
| --- | --- | --- | --- |
| `id` | `TEXT` T | 可空；PK | 无声明 |
| `status` | `TEXT` T | NN | 无声明 |
| `next_page` | `INTEGER` T | NN | 无声明 |
| `total_cards` | `INTEGER` T | 可空 | 无声明 |
| `discovered_cards` | `INTEGER` T | NN | `0`；无声明 |
| `last_error_code` | `TEXT` T | 可空 | 无声明 |
| `updated_at` | `TEXT` T | NN | 无声明 |
| `mode` | `TEXT` T | NN | 无声明 |
| `processed_cards` | `INTEGER` T | NN | `0`；无声明 |
| `held_cards` | `INTEGER` T | NN | `0`；无声明 |
| `started_at` | `TEXT` T | NN | 无声明 |
| `completed_at` | `TEXT` T | 可空 | 无声明 |
| `locked_at` | `TEXT` T | 可空 | 无声明 |
| `locked_by` | `TEXT` T | 可空 | 无声明 |
| `provider_key` | `TEXT` Q（推断） | 未知 | 未知 |

### 5.9 `card_price_signals`

生产 ON CONFLICT(card_id,tier) 要求复合唯一键；fixture 没有声明。fetched_at 是本地采集时刻，provider_time 是上游行情时刻，不能混用。

生产引用：[internal/processor/eligibility.go](../internal/processor/eligibility.go)、[internal/processor/inventory.go](../internal/processor/inventory.go)、[internal/processor/payment.go](../internal/processor/payment.go)。

DDL证据：[internal/processor/eligibility_dependency_test.go](../internal/processor/eligibility_dependency_test.go)、[internal/processor/eligibility_test.go](../internal/processor/eligibility_test.go)、[internal/processor/payment_test.go](../internal/processor/payment_test.go)。

观察到的PK/UNIQUE：无DDL证据。

| 列 | 类型/证据 | NOT NULL / PK | DEFAULT证据 |
| --- | --- | --- | --- |
| `card_id` | `TEXT` T | 可空 | 无声明 |
| `tier` | `TEXT` T | 可空 | 无声明 |
| `found` | `INTEGER` T | 可空 | 无声明 |
| `amount` | `REAL` T | 可空 | 无声明 |
| `provider_time` | `TEXT` T | 可空 | 无声明 |
| `min_usd` | `REAL` T | 可空 | 无声明 |
| `max_usd` | `REAL` T | 可空 | 无声明 |
| `fetched_at` | `TEXT` Q（推断） | 未知 | 未知 |

### 5.10 `card_product_policies`

现代路径按 provider_key/product_code 查询；旧兼容路径不带 provider_key。启用值是配置策略，不表示特定卡有可用容量或资金。正式唯一键未知。

生产引用：[internal/processor/eligibility.go](../internal/processor/eligibility.go)、[internal/processor/payment.go](../internal/processor/payment.go)。

DDL证据：[internal/processor/eligibility_dependency_test.go](../internal/processor/eligibility_dependency_test.go)、[internal/processor/eligibility_test.go](../internal/processor/eligibility_test.go)。

观察到的PK/UNIQUE：无DDL证据。

| 列 | 类型/证据 | NOT NULL / PK | DEFAULT证据 |
| --- | --- | --- | --- |
| `product_code` | `TEXT` T | 可空 | 无声明 |
| `enabled` | `INTEGER` T | 可空 | 无声明 |
| `provider_key` | `TEXT` Q（推断） | 未知 | 未知 |

### 5.11 `cdkeys`

Go 只读取 manual_type/metadata 并在确认付款前无暴露的 Session 失败时释放锁；码发行、兑换事务与其它字段属于外部生产者，未包含在此契约。

生产引用：[internal/processor/intake.go](../internal/processor/intake.go)、[internal/processor/session_recovery.go](../internal/processor/session_recovery.go)。

DDL证据：[internal/processor/eligibility_dependency_test.go](../internal/processor/eligibility_dependency_test.go)、[internal/processor/eligibility_test.go](../internal/processor/eligibility_test.go)、[internal/processor/intake_test.go](../internal/processor/intake_test.go)。

观察到的PK/UNIQUE：`(id)`。

| 列 | 类型/证据 | NOT NULL / PK | DEFAULT证据 |
| --- | --- | --- | --- |
| `id` | `TEXT` T | 可空；PK | 无声明 |
| `status` | `TEXT` T | NN | 无声明 |
| `locked_at` | `TEXT` T | 可空 | 无声明 |
| `locked_by_order_id` | `TEXT` T | 可空 | 无声明 |
| `updated_at` | `TEXT` T | 可空 | 无声明 |
| `manual_type` | `TEXT` T | 可空 | 无声明 |
| `metadata` | `TEXT` T | 可空 | 无声明 |

### 5.12 `checkout_price_contracts`

浏览器只接受 PHP 且金额区间有效；eligibility 要求 plus/PHP/active 恰好一条。正式的版本与 active 唯一约束未在 fixture 中声明。

生产引用：[internal/processor/checkout.go](../internal/processor/checkout.go)、[internal/processor/eligibility.go](../internal/processor/eligibility.go)、[internal/processor/payment.go](../internal/processor/payment.go)。

DDL证据：[internal/processor/checkout_flow_test.go](../internal/processor/checkout_flow_test.go)、[internal/processor/eligibility_dependency_test.go](../internal/processor/eligibility_dependency_test.go)。

观察到的PK/UNIQUE：`(id)`。

| 列 | 类型/证据 | NOT NULL / PK | DEFAULT证据 |
| --- | --- | --- | --- |
| `id` | `TEXT` T | 可空；PK | 无声明 |
| `version` | `INTEGER` T | 可空 | 无声明 |
| `tier` | `TEXT` T | 可空 | 无声明 |
| `currency` | `TEXT` T | 可空 | 无声明 |
| `min_amount` | `REAL` T | 可空 | 无声明 |
| `max_amount` | `REAL` T | 可空 | 无声明 |
| `status` | `TEXT` T | 可空 | 无声明 |

### 5.13 `checkout_validation_runs`

只写 passed 的脱敏 PageFacts；不是银行卡预授权或扣款证明。当前 worker 成功预检后会推进 FUNDING_READY，新 CLI 不调用该状态迁移。

生产引用：[internal/processor/checkout.go](../internal/processor/checkout.go)。

DDL证据：[internal/processor/checkout_flow_test.go](../internal/processor/checkout_flow_test.go)。

观察到的PK/UNIQUE：`(id)`。

| 列 | 类型/证据 | NOT NULL / PK | DEFAULT证据 |
| --- | --- | --- | --- |
| `id` | `TEXT` T | 可空；PK | 无声明 |
| `order_id` | `TEXT` T | 可空 | 无声明 |
| `site_id` | `TEXT` T | 可空 | 无声明 |
| `product_id` | `TEXT` T | 可空 | 无声明 |
| `tier` | `TEXT` T | 可空 | 无声明 |
| `adapter_version` | `TEXT` T | 可空 | 无声明 |
| `price_contract_id` | `TEXT` T | 可空 | 无声明 |
| `status` | `TEXT` T | 可空 | 无声明 |
| `sanitized_result` | `TEXT` T | 可空 | 无声明 |
| `started_at` | `TEXT` T | 可空 | 无声明 |
| `completed_at` | `TEXT` T | 可空 | 无声明 |
| `created_by` | `TEXT` T | 可空 | 无声明 |

### 5.14 `extension_delivery_settings`

仅 legacy renewalToken 读取 default 行。注入自有 AccountService 后取消路径不读该表，但旧 VerifySharedSchema 仍要求它存在，默认 worker 仍用 legacy 构造。

生产引用：[internal/processor/dependencies.go](../internal/processor/dependencies.go)。

DDL证据：[internal/processor/eligibility_dependency_test.go](../internal/processor/eligibility_dependency_test.go)。

观察到的PK/UNIQUE：`(id)`。

| 列 | 类型/证据 | NOT NULL / PK | DEFAULT证据 |
| --- | --- | --- | --- |
| `id` | `TEXT` T | 可空；PK | 无声明 |
| `spacexcard_api_token_encrypted` | `TEXT` T | 可空 | 无声明 |

### 5.15 `fulfillment_dependency_circuits`

UNIQUE(dependency,scope_key) 是生产 UPSERT 要求；failure_count/recovery_revision 的 DEFAULT 0 有 fixture 证据。half_open 抢占通过原 updated_at 比较并在事务内执行。

生产引用：[internal/processor/dependencies.go](../internal/processor/dependencies.go)。

DDL证据：[internal/processor/dependencies_test.go](../internal/processor/dependencies_test.go)、[internal/processor/eligibility_dependency_test.go](../internal/processor/eligibility_dependency_test.go)、[internal/processor/inventory_dependency_test.go](../internal/processor/inventory_dependency_test.go)、[internal/processor/payment_test.go](../internal/processor/payment_test.go)。

观察到的PK/UNIQUE：`(dependency,scope_key)`, `(id)`。

| 列 | 类型/证据 | NOT NULL / PK | DEFAULT证据 |
| --- | --- | --- | --- |
| `id` | `TEXT` T | 可空；PK | 无声明 |
| `dependency` | `TEXT` T | NN | 无声明 |
| `scope_key` | `TEXT` T | NN | 无声明 |
| `state` | `TEXT` T | NN | 无声明 |
| `failure_count` | `INTEGER` T | NN | `0` |
| `opened_at` | `TEXT` T | 可空 | 无声明 |
| `retry_at` | `TEXT` T | 可空 | 无声明 |
| `recovery_revision` | `INTEGER` T | NN | `0` |
| `reason_code` | `TEXT` T | 可空 | 无声明 |
| `updated_at` | `TEXT` T | NN | 无声明 |

### 5.16 `fulfillment_interventions`

fixture 唯一键有冲突：一版为(fulfillment_id,state,state_revision)，另一版额外包含reason_code。生产 INSERT OR IGNORE 与读逻辑使用前3字段定位；迁移前必须决定权威契约。feishu_* 列只见测试DDL，当前Go生产SQL不使用。

生产引用：[internal/processor/checkout.go](../internal/processor/checkout.go)、[internal/processor/reconciliation.go](../internal/processor/reconciliation.go)。

DDL证据：[internal/processor/checkout_flow_test.go](../internal/processor/checkout_flow_test.go)、[internal/processor/eligibility_dependency_test.go](../internal/processor/eligibility_dependency_test.go)、[internal/processor/reconciliation_test.go](../internal/processor/reconciliation_test.go)。

观察到的PK/UNIQUE：`(fulfillment_id,state,state_revision)`, `(fulfillment_id,state,state_revision,reason_code)`, `(id)`。

| 列 | 类型/证据 | NOT NULL / PK | DEFAULT证据 |
| --- | --- | --- | --- |
| `id` | `TEXT` T | 可空；PK | 无声明 |
| `fulfillment_id` | `TEXT` T | 有差异 | 无声明 |
| `state` | `TEXT` T | 有差异 | 无声明 |
| `state_revision` | `INTEGER` T | 有差异 | 无声明 |
| `reason_code` | `TEXT` T | 有差异 | 无声明 |
| `created_at` | `TEXT` T | 有差异 | 无声明 |
| `acknowledged_at` | `TEXT` T | 可空 | 无声明 |
| `acknowledged_by` | `TEXT` T | 可空 | 无声明 |
| `feishu_status` | `TEXT` T | 可空 | 无声明 |
| `feishu_sent_at` | `TEXT` T | 可空 | 无声明 |

### 5.17 `funding_intents`

fixture UNIQUE(fulfillment_id)；idempotency_key、request_fingerprint、加密request body必须绑定原操作。生产没有证据支持未知结果改新key重发。多平台 provider_key 通过兼容检查切换；正式 idempotency_key 唯一约束未见DDL。

生产引用：[internal/processor/payment.go](../internal/processor/payment.go)、[internal/processor/session_recovery.go](../internal/processor/session_recovery.go)。

DDL证据：[internal/processor/eligibility_dependency_test.go](../internal/processor/eligibility_dependency_test.go)、[internal/processor/eligibility_test.go](../internal/processor/eligibility_test.go)、[internal/processor/payment_test.go](../internal/processor/payment_test.go)。

观察到的PK/UNIQUE：`(fulfillment_id)`, `(id)`。

| 列 | 类型/证据 | NOT NULL / PK | DEFAULT证据 |
| --- | --- | --- | --- |
| `id` | `TEXT` T | 可空；PK | 无声明 |
| `fulfillment_id` | `TEXT` T | NN | 无声明 |
| `submitted_at` | `TEXT` T | 可空 | 无声明 |
| `operation` | `TEXT` T | NN | 无声明 |
| `target_card_id` | `TEXT` T | 可空 | 无声明 |
| `product_code` | `TEXT` T | 可空 | 无声明 |
| `amount` | `REAL` T | NN | 无声明 |
| `fee` | `REAL` T | NN | 无声明 |
| `idempotency_key` | `TEXT` T | NN | 无声明 |
| `request_fingerprint` | `TEXT` T | NN | 无声明 |
| `request_body_encrypted` | `TEXT` T | NN | 无声明 |
| `state` | `TEXT` T | NN | 无声明 |
| `provider_resource_id` | `TEXT` T | 可空 | 无声明 |
| `created_at` | `TEXT` T | NN | 无声明 |
| `resolved_at` | `TEXT` T | 可空 | 无声明 |
| `provider_key` | `TEXT` Q（推断） | 未知 | 未知 |

### 5.18 `live_canary_authorizations`

完全没有本仓库CREATE TABLE证据。上述列仅来自匹配签发快照、过期和consume操作；签发入口、其余列、默认值、唯一键、外键均未知，不能据此自动创建生产许可表。

生产引用：[internal/processor/checkout.go](../internal/processor/checkout.go)。

观察到的PK/UNIQUE：无DDL证据。

| 列 | 类型/证据 | NOT NULL / PK | DEFAULT证据 |
| --- | --- | --- | --- |
| `id` | `TEXT` Q（推断） | 未知 | 未知 |
| `fulfillment_id` | `TEXT` Q（推断） | 未知 | 未知 |
| `stage_key` | `TEXT` Q（推断） | 未知 | 未知 |
| `target_tier` | `TEXT` Q（推断） | 未知 | 未知 |
| `card_id` | `TEXT` Q（推断） | 未知 | 未知 |
| `price_contract_id` | `TEXT` Q（推断） | 未知 | 未知 |
| `adapter_version` | `TEXT` Q（推断） | 未知 | 未知 |
| `state` | `TEXT` Q（推断） | 未知 | 未知 |
| `snapshot_fingerprint` | `TEXT` Q（推断） | 未知 | 未知 |
| `approved_at` | `TEXT` Q（推断） | 未知 | 未知 |
| `invalidated_at` | `TEXT` Q（推断） | 未知 | 未知 |
| `consumed_at` | `TEXT` Q（推断） | 未知 | 未知 |

### 5.19 `managed_card_transactions`

PK(card_id,auth_id) 是生产 UPSERT 要求。authorization/settlement/refund/reversal证据位单调累积，迟到authorization不得抹去已确认settlement。decline_reason_code仅见测试DDL。

生产引用：[internal/processor/inventory.go](../internal/processor/inventory.go)、[internal/processor/reconciliation.go](../internal/processor/reconciliation.go)。

DDL证据：[internal/processor/reconciliation_test.go](../internal/processor/reconciliation_test.go)。

观察到的PK/UNIQUE：`(card_id,auth_id)`。

| 列 | 类型/证据 | NOT NULL / PK | DEFAULT证据 |
| --- | --- | --- | --- |
| `card_id` | `TEXT` T | NN；PK | 无声明 |
| `auth_id` | `TEXT` T | NN；PK | 无声明 |
| `auth_time` | `TEXT` T | 可空 | 无声明 |
| `auth_amount` | `REAL` T | NN | `0` |
| `auth_currency` | `TEXT` T | 可空 | 无声明 |
| `settle_amount` | `REAL` T | NN | `0` |
| `settle_currency` | `TEXT` T | 可空 | 无声明 |
| `type` | `TEXT` T | NN | 无声明 |
| `status` | `TEXT` T | NN | 无声明 |
| `merchant_normalized` | `TEXT` T | NN | 无声明 |
| `authorization_seen` | `INTEGER` T | NN | `0` |
| `settlement_seen` | `INTEGER` T | NN | `0` |
| `refund_seen` | `INTEGER` T | NN | `0` |
| `reversal_seen` | `INTEGER` T | NN | `0` |
| `decline_reason_code` | `TEXT` T | 可空 | 无声明 |
| `first_seen_at` | `TEXT` T | NN | 无声明 |
| `last_seen_at` | `TEXT` T | NN | 无声明 |

### 5.20 `managed_cards`

旧模式 ON CONFLICT(upstream_card_id)，多平台模式 ON CONFLICT(provider_key,upstream_card_id)。新版必须建立后者，并评估/迁移旧全局唯一键，不能同时错误阻止不同平台相同卡ID。PAN/CVV不存入该模型。

生产引用：[internal/processor/checkout.go](../internal/processor/checkout.go)、[internal/processor/eligibility.go](../internal/processor/eligibility.go)、[internal/processor/inventory.go](../internal/processor/inventory.go)、[internal/processor/payment.go](../internal/processor/payment.go)、[internal/processor/reconciliation.go](../internal/processor/reconciliation.go)。

DDL证据：[internal/processor/eligibility_dependency_test.go](../internal/processor/eligibility_dependency_test.go)、[internal/processor/eligibility_test.go](../internal/processor/eligibility_test.go)、[internal/processor/inventory_freeze_test.go](../internal/processor/inventory_freeze_test.go)、[internal/processor/payment_test.go](../internal/processor/payment_test.go)、[internal/processor/reconciliation_test.go](../internal/processor/reconciliation_test.go)。

观察到的PK/UNIQUE：`(id)`, `(upstream_card_id)`。

| 列 | 类型/证据 | NOT NULL / PK | DEFAULT证据 |
| --- | --- | --- | --- |
| `id` | `TEXT` T | 可空；PK | 无声明 |
| `product_code` | `TEXT` T | 有差异 | 无声明 |
| `lane` | `TEXT` T | 可空 | 无声明 |
| `consumed_slots` | `INTEGER` T | 有差异 | `0`；无声明 |
| `capacity_state` | `TEXT` T | 有差异 | `'AVAILABLE'`；无声明 |
| `upstream_status` | `TEXT` T | 有差异 | 无声明 |
| `reconciliation_state` | `TEXT` T | 有差异 | `'PENDING'`；无声明 |
| `upstream_card_id` | `INTEGER` T | 有差异；PK/差异 | 无声明 |
| `reconciliation_reason` | `TEXT` T | 可空 | 无声明 |
| `updated_at` | `TEXT` T | NN | 无声明 |
| `vm_card_id` | `TEXT` T | NN | 无声明 |
| `cached_available_amount` | `REAL` T | NN | `0`；无声明 |
| `bin` | `TEXT` T | 可空 | 无声明 |
| `last4` | `TEXT` T | 可空 | 无声明 |
| `last_balance_sync_at` | `TEXT` T | 可空 | 无声明 |
| `created_at` | `TEXT` T | NN | 无声明 |
| `last_transaction_sync_at` | `TEXT` T | 可空 | 无声明 |
| `provider_key` | `TEXT` Q（推断） | 未知 | 未知 |

### 5.21 `membership_action_auth_snapshots`

PK(permit_id,auth_id)。快照必须与原permit/card绑定，在可能产生资金动作前持久化，不能以事后查询补作事前基线。

生产引用：[internal/processor/checkout.go](../internal/processor/checkout.go)、[internal/processor/reconciliation.go](../internal/processor/reconciliation.go)。

DDL证据：[internal/processor/reconciliation_test.go](../internal/processor/reconciliation_test.go)。

观察到的PK/UNIQUE：`(permit_id,auth_id)`。

| 列 | 类型/证据 | NOT NULL / PK | DEFAULT证据 |
| --- | --- | --- | --- |
| `permit_id` | `TEXT` T | NN；PK | 无声明 |
| `card_id` | `TEXT` T | NN | 无声明 |
| `auth_id` | `TEXT` T | NN；PK | 无声明 |
| `snapshotted_at` | `TEXT` T | NN | 无声明 |

### 5.22 `membership_action_permits`

生产 INSERT 还需要大量fixture缺失的控制和授权列。issued→activated→reported/outcome_uncertain等状态不能由客户端直接设定；action_type区分progression/submit；正式sequence唯一键和外键未知。

生产引用：[internal/executor/bridge.go](../internal/executor/bridge.go)、[internal/processor/checkout.go](../internal/processor/checkout.go)、[internal/processor/reconciliation.go](../internal/processor/reconciliation.go)、[internal/processor/session_recovery.go](../internal/processor/session_recovery.go)。

DDL证据：[internal/executor/bridge_test.go](../internal/executor/bridge_test.go)、[internal/processor/eligibility_dependency_test.go](../internal/processor/eligibility_dependency_test.go)、[internal/processor/eligibility_test.go](../internal/processor/eligibility_test.go)、[internal/processor/reconciliation_test.go](../internal/processor/reconciliation_test.go)。

观察到的PK/UNIQUE：`(id)`。

| 列 | 类型/证据 | NOT NULL / PK | DEFAULT证据 |
| --- | --- | --- | --- |
| `fulfillment_id` | `TEXT` T | 有差异 | 无声明 |
| `stage_key` | `TEXT` T | 有差异 | 无声明 |
| `attempt_no` | `INTEGER` T | 有差异 | 无声明 |
| `action_type` | `TEXT` T | 有差异 | 无声明 |
| `state` | `TEXT` T | 有差异 | 无声明 |
| `id` | `TEXT` T | 可空；PK | 无声明 |
| `activated_at` | `TEXT` T | 可空 | 无声明 |
| `reported_at` | `TEXT` T | 可空 | 无声明 |
| `outcome_code` | `TEXT` T | 可空 | 无声明 |
| `sequence_no` | `INTEGER` T | NN | 无声明 |
| `installation_id` | `TEXT` Q（推断） | 未知 | 未知 |
| `browser_lease_epoch` | `INTEGER` Q（推断） | 未知 | 未知 |
| `adapter_version` | `TEXT` Q（推断） | 未知 | 未知 |
| `price_contract_id` | `TEXT` Q（推断） | 未知 | 未知 |
| `control_id` | `TEXT` Q（推断） | 未知 | 未知 |
| `page_fingerprint` | `TEXT` Q（推断） | 未知 | 未知 |
| `issued_at` | `TEXT` Q（推断） | 未知 | 未知 |
| `expires_at` | `TEXT` Q（推断） | 未知 | 未知 |
| `authorization_mode` | `TEXT` Q（推断） | 未知 | 未知 |
| `authorization_id` | `TEXT` Q（推断） | 未知 | 未知 |

### 5.23 `membership_card_platforms`

配置主键key；kind/base_url/encrypted credential不足以运行inventory，完整路径还需要enabled/priority/inventory/updated字段。现代平台存在性是若干schema分支的开关，必须成套升级。

生产引用：[internal/processor/dependencies.go](../internal/processor/dependencies.go)、[internal/processor/inventory.go](../internal/processor/inventory.go)。

DDL证据：[internal/processor/card_platform_config_test.go](../internal/processor/card_platform_config_test.go)。

观察到的PK/UNIQUE：`(key)`。

| 列 | 类型/证据 | NOT NULL / PK | DEFAULT证据 |
| --- | --- | --- | --- |
| `key` | `TEXT` T | 可空；PK | 无声明 |
| `kind` | `TEXT` T | 可空 | 无声明 |
| `base_url` | `TEXT` T | 可空 | 无声明 |
| `credential_encrypted` | `TEXT` T | 可空 | 无声明 |
| `enabled` | `INTEGER` Q（推断） | 未知 | 未知 |
| `priority` | `INTEGER` Q（推断） | 未知 | 未知 |
| `inventory_status` | `TEXT` Q（推断） | 未知 | 未知 |
| `last_inventory_error` | `TEXT` Q（推断） | 未知 | 未知 |
| `updated_at` | `TEXT` Q（推断） | 未知 | 未知 |
| `updated_by` | `TEXT` Q（推断） | 未知 | 未知 |
| `inventory_initialized_at` | `TEXT` Q（推断） | 未知 | 未知 |

### 5.24 `membership_checkout_commands`

UNIQUE(fulfillment_id,stage_key,attempt_no,command_kind)供INSERT OR IGNORE幂等入队。lease_epoch默认0；只能保存token SHA256，真实敏感材料留在bridge内存。expiry/context loss不能静默requeue。

生产引用：[internal/executor/bridge.go](../internal/executor/bridge.go)、[internal/executor/queue.go](../internal/executor/queue.go)、[internal/processor/session_recovery.go](../internal/processor/session_recovery.go)。

DDL证据：[internal/executor/bridge_test.go](../internal/executor/bridge_test.go)、[internal/executor/queue_test.go](../internal/executor/queue_test.go)、[internal/processor/eligibility_dependency_test.go](../internal/processor/eligibility_dependency_test.go)、[internal/processor/eligibility_test.go](../internal/processor/eligibility_test.go)。

观察到的PK/UNIQUE：`(fulfillment_id,stage_key,attempt_no,command_kind)`, `(id)`。

| 列 | 类型/证据 | NOT NULL / PK | DEFAULT证据 |
| --- | --- | --- | --- |
| `id` | `TEXT` T | 可空；PK | 无声明 |
| `fulfillment_id` | `TEXT` T | 有差异 | 无声明 |
| `stage_key` | `TEXT` T | 可空 | 无声明 |
| `attempt_no` | `INTEGER` T | 可空 | 无声明 |
| `command_kind` | `TEXT` T | 可空 | 无声明 |
| `priority_class` | `TEXT` T | 可空 | 无声明 |
| `target_tier` | `TEXT` T | 可空 | 无声明 |
| `adapter_version` | `TEXT` T | 可空 | 无声明 |
| `price_contract_id` | `TEXT` T | 可空 | 无声明 |
| `price_contract_version` | `INTEGER` T | 可空 | 无声明 |
| `fulfillment_revision` | `INTEGER` T | 可空 | 无声明 |
| `state` | `TEXT` T | 有差异 | 无声明 |
| `lease_epoch` | `INTEGER` T | 可空 | `0` |
| `lease_token_sha256` | `TEXT` T | 可空 | 无声明 |
| `leased_by` | `TEXT` T | 可空 | 无声明 |
| `leased_at` | `TEXT` T | 可空 | 无声明 |
| `heartbeat_at` | `TEXT` T | 可空 | 无声明 |
| `lease_expires_at` | `TEXT` T | 可空 | 无声明 |
| `hard_deadline_at` | `TEXT` T | 可空 | 无声明 |
| `material_claimed_at` | `TEXT` T | 可空 | 无声明 |
| `available_at` | `TEXT` T | 可空 | 无声明 |
| `payment_ready_at` | `TEXT` T | 可空 | 无声明 |
| `outcome_code` | `TEXT` T | 可空 | 无声明 |
| `sanitized_diagnostic` | `TEXT` T | 可空 | 无声明 |
| `created_at` | `TEXT` T | 可空 | 无声明 |
| `updated_at` | `TEXT` T | 有差异 | 无声明 |
| `ended_at` | `TEXT` T | 可空 | 无声明 |

### 5.25 `membership_fulfillment_attempts`

测试只提供终止attempt所需4列，实际新attempt INSERT还需要stage/attempt/resume/adapter/priceversion/started_at。业务上的(fulfillment_id,stage,attempt_no)唯一语义尚无DDL证据。

生产引用：[internal/processor/checkout.go](../internal/processor/checkout.go)、[internal/processor/reconciliation.go](../internal/processor/reconciliation.go)、[internal/processor/session_recovery.go](../internal/processor/session_recovery.go)。

DDL证据：[internal/processor/reconciliation_test.go](../internal/processor/reconciliation_test.go)。

观察到的PK/UNIQUE：`(id)`。

| 列 | 类型/证据 | NOT NULL / PK | DEFAULT证据 |
| --- | --- | --- | --- |
| `id` | `TEXT` T | 可空；PK | 无声明 |
| `fulfillment_id` | `TEXT` T | NN | 无声明 |
| `ended_at` | `TEXT` T | 可空 | 无声明 |
| `outcome_code` | `TEXT` T | 可空 | 无声明 |
| `stage` | `TEXT` Q（推断） | 未知 | 未知 |
| `attempt_no` | `INTEGER` Q（推断） | 未知 | 未知 |
| `resume_revision` | `INTEGER` Q（推断） | 未知 | 未知 |
| `adapter_version` | `TEXT` Q（推断） | 未知 | 未知 |
| `price_contract_version` | `INTEGER` Q（推断） | 未知 | 未知 |
| `started_at` | `TEXT` Q（推断） | 未知 | 未知 |

### 5.26 `membership_fulfillment_settings`

default 行的enabled/rollout_mode是资金门禁；不能为预检而把它们设为可付款。inventory_initialized_at仅生产UPDATE出现，fixture缺失。

生产引用：[internal/executor/bridge.go](../internal/executor/bridge.go)、[internal/processor/checkout.go](../internal/processor/checkout.go)、[internal/processor/dependencies.go](../internal/processor/dependencies.go)、[internal/processor/eligibility.go](../internal/processor/eligibility.go)、[internal/processor/inventory.go](../internal/processor/inventory.go)、[internal/processor/payment.go](../internal/processor/payment.go)。

DDL证据：[internal/executor/bridge_test.go](../internal/executor/bridge_test.go)、[internal/processor/eligibility_dependency_test.go](../internal/processor/eligibility_dependency_test.go)、[internal/processor/inventory_dependency_test.go](../internal/processor/inventory_dependency_test.go)、[internal/processor/inventory_freeze_test.go](../internal/processor/inventory_freeze_test.go)、[internal/processor/payment_test.go](../internal/processor/payment_test.go)、[internal/processor/reconciliation_test.go](../internal/processor/reconciliation_test.go)。

观察到的PK/UNIQUE：`(id)`。

| 列 | 类型/证据 | NOT NULL / PK | DEFAULT证据 |
| --- | --- | --- | --- |
| `id` | `TEXT` T | 可空；PK | 无声明 |
| `enabled` | `INTEGER` T | 可空 | 无声明 |
| `rollout_mode` | `TEXT` T | 可空 | 无声明 |
| `inventory_status` | `TEXT` T | NN | 无声明 |
| `spacexcard_app_id` | `TEXT` T | 可空 | 无声明 |
| `spacexcard_app_secret_encrypted` | `TEXT` T | 可空 | 无声明 |
| `last_inventory_error` | `TEXT` T | 可空 | 无声明 |
| `updated_at` | `TEXT` T | 可空 | 无声明 |
| `updated_by` | `TEXT` T | 可空 | 无声明 |
| `inventory_initialized_at` | `TEXT` Q（推断） | 未知 | 未知 |

### 5.27 `membership_fulfillments`

fixture存在order_id与order_no唯一约束但并非每个fixture都有。active账号锁是IMMEDIATE事务中的查重与迁移，正式partial unique约束未知。money_boundary_at一旦设置，不因Session失效而解除订单/CDK资金责任。

生产引用：[internal/executor/bridge.go](../internal/executor/bridge.go)、[internal/processor/checkout.go](../internal/processor/checkout.go)、[internal/processor/eligibility.go](../internal/processor/eligibility.go)、[internal/processor/intake.go](../internal/processor/intake.go)、[internal/processor/payment.go](../internal/processor/payment.go)、[internal/processor/reconciliation.go](../internal/processor/reconciliation.go)、[internal/processor/session_recovery.go](../internal/processor/session_recovery.go)、[internal/processor/state.go](../internal/processor/state.go)。

DDL证据：[internal/executor/bridge_test.go](../internal/executor/bridge_test.go)、[internal/processor/checkout_flow_test.go](../internal/processor/checkout_flow_test.go)、[internal/processor/eligibility_dependency_test.go](../internal/processor/eligibility_dependency_test.go)、[internal/processor/eligibility_test.go](../internal/processor/eligibility_test.go)、[internal/processor/fencing_test.go](../internal/processor/fencing_test.go)、[internal/processor/intake_test.go](../internal/processor/intake_test.go)、[internal/processor/payment_test.go](../internal/processor/payment_test.go)、[internal/processor/reconciliation_test.go](../internal/processor/reconciliation_test.go)、[internal/processor/state_test.go](../internal/processor/state_test.go)。

观察到的PK/UNIQUE：`(id)`, `(order_id)`, `(order_no)`。

| 列 | 类型/证据 | NOT NULL / PK | DEFAULT证据 |
| --- | --- | --- | --- |
| `id` | `TEXT` T | 可空；PK | 无声明 |
| `state` | `TEXT` T | 有差异 | 无声明 |
| `run_mode` | `TEXT` T | 可空 | 无声明 |
| `state_revision` | `INTEGER` T | 有差异 | `0`；无声明 |
| `card_reservation_id` | `TEXT` T | 可空 | 无声明 |
| `money_boundary_at` | `TEXT` T | 可空 | 无声明 |
| `updated_at` | `TEXT` T | 有差异 | 无声明 |
| `order_id` | `TEXT` T | NN | 无声明 |
| `order_no` | `TEXT` T | NN | 无声明 |
| `target_tier` | `TEXT` T | NN | 无声明 |
| `current_stage` | `TEXT` T | 可空 | 无声明 |
| `account_lock_key` | `TEXT` T | 可空 | 无声明 |
| `retry_at` | `TEXT` T | 可空 | 无声明 |
| `failure_code` | `TEXT` T | 可空 | 无声明 |
| `created_at` | `TEXT` T | NN | 无声明 |
| `completed_at` | `TEXT` T | 可空 | 无声明 |
| `resume_revision` | `INTEGER` T | NN | `0` |
| `browser_lease_epoch` | `INTEGER` T | 可空 | 无声明 |

### 5.28 `membership_intake_settings`

default 行 accept_orders_created_at 是 intake 下界；缺行会使JOIN不产出订单。生产没有初始化该设置的代码，不能假设空库会自行启动。

生产引用：[internal/processor/intake.go](../internal/processor/intake.go)。

DDL证据：[internal/processor/intake_test.go](../internal/processor/intake_test.go)。

观察到的PK/UNIQUE：`(id)`。

| 列 | 类型/证据 | NOT NULL / PK | DEFAULT证据 |
| --- | --- | --- | --- |
| `id` | `TEXT` T | 可空；PK | 无声明 |
| `accept_orders_created_at` | `TEXT` T | NN | 无声明 |
| `created_at` | `TEXT` T | NN | 无声明 |
| `created_by` | `TEXT` T | NN | 无声明 |

### 5.29 `membership_no_payment_checks`

UNIQUE(fulfillment_id,stage_key,checkpoint)，checkpoint是5m/1h/24h证据检查。没有扣款证据不等于即刻可释放，需结合会员、pending authorization与时间边界。

生产引用：[internal/processor/reconciliation.go](../internal/processor/reconciliation.go)。

DDL证据：[internal/processor/reconciliation_test.go](../internal/processor/reconciliation_test.go)。

观察到的PK/UNIQUE：`(fulfillment_id,stage_key,checkpoint)`, `(id)`。

| 列 | 类型/证据 | NOT NULL / PK | DEFAULT证据 |
| --- | --- | --- | --- |
| `id` | `TEXT` T | 可空；PK | 无声明 |
| `fulfillment_id` | `TEXT` T | NN | 无声明 |
| `stage_key` | `TEXT` T | NN | 无声明 |
| `checkpoint` | `TEXT` T | NN | 无声明 |
| `membership_unchanged` | `INTEGER` T | NN | 无声明 |
| `no_effective_transaction` | `INTEGER` T | NN | 无声明 |
| `no_pending_authorization` | `INTEGER` T | NN | 无声明 |
| `observed_at` | `TEXT` T | NN | 无声明 |

### 5.30 `membership_observations`

auto_renew允许NULL表示未知，不能转为false。自有typed观察的provider_code=0，legacy规范化业务码=200；生产schema需允许两者。付费tier的expiry/currency来自证据，不靠空值推断。

生产引用：[internal/processor/eligibility.go](../internal/processor/eligibility.go)、[internal/processor/reconciliation.go](../internal/processor/reconciliation.go)。

DDL证据：[internal/processor/eligibility_dependency_test.go](../internal/processor/eligibility_dependency_test.go)、[internal/processor/reconciliation_test.go](../internal/processor/reconciliation_test.go)。

观察到的PK/UNIQUE：`(id)`。

| 列 | 类型/证据 | NOT NULL / PK | DEFAULT证据 |
| --- | --- | --- | --- |
| `id` | `TEXT` T | 可空；PK | 无声明 |
| `fulfillment_id` | `TEXT` T | NN | 无声明 |
| `stage_key` | `TEXT` T | 可空 | 无声明 |
| `purpose` | `TEXT` T | NN | 无声明 |
| `provider_code` | `INTEGER` T | NN | 无声明 |
| `account_type` | `TEXT` T | NN | 无声明 |
| `currency` | `TEXT` T | 可空 | 无声明 |
| `auto_renew` | `INTEGER` T | 可空 | 无声明 |
| `is_overdue` | `INTEGER` T | NN | 无声明 |
| `is_delinquent` | `INTEGER` T | NN | 无声明 |
| `expire_time` | `TEXT` T | 可空 | 无声明 |
| `observed_at` | `TEXT` T | NN | 无声明 |

### 5.31 `membership_outbox`

dependency事件允许fulfillment_id/state_revision=NULL；payload为脱敏事件。不应另建回调projection或第二套membership状态作为事实来源。

生产引用：[internal/processor/dependencies.go](../internal/processor/dependencies.go)、[internal/processor/state.go](../internal/processor/state.go)。

DDL证据：[internal/processor/checkout_flow_test.go](../internal/processor/checkout_flow_test.go)、[internal/processor/eligibility_dependency_test.go](../internal/processor/eligibility_dependency_test.go)、[internal/processor/eligibility_test.go](../internal/processor/eligibility_test.go)、[internal/processor/fencing_test.go](../internal/processor/fencing_test.go)、[internal/processor/inventory_dependency_test.go](../internal/processor/inventory_dependency_test.go)、[internal/processor/payment_test.go](../internal/processor/payment_test.go)、[internal/processor/state_test.go](../internal/processor/state_test.go)。

观察到的PK/UNIQUE：`(id)`。

| 列 | 类型/证据 | NOT NULL / PK | DEFAULT证据 |
| --- | --- | --- | --- |
| `id` | `TEXT` T | 可空；PK | 无声明 |
| `event_type` | `TEXT` T | 有差异 | 无声明 |
| `fulfillment_id` | `TEXT` T | 可空 | 无声明 |
| `state_revision` | `INTEGER` T | 可空 | 无声明 |
| `payload` | `TEXT` T | 有差异 | 无声明 |
| `created_at` | `TEXT` T | 有差异 | 无声明 |

### 5.32 `membership_payment_stages`

生产需要page_facts_json、control/fingerprint、progression/submit时间等未在fixture完整提供的列。每个fulfillment/stage需要唯一业务记录；该复合唯一约束未见fixture。金额/目标tier/pricecontract在funding准备时固定，不能付款时重新猜价。

生产引用：[internal/executor/bridge.go](../internal/executor/bridge.go)、[internal/processor/checkout.go](../internal/processor/checkout.go)、[internal/processor/payment.go](../internal/processor/payment.go)、[internal/processor/reconciliation.go](../internal/processor/reconciliation.go)、[internal/processor/session_recovery.go](../internal/processor/session_recovery.go)。

DDL证据：[internal/executor/bridge_test.go](../internal/executor/bridge_test.go)、[internal/processor/payment_test.go](../internal/processor/payment_test.go)、[internal/processor/reconciliation_test.go](../internal/processor/reconciliation_test.go)。

观察到的PK/UNIQUE：`(id)`。

| 列 | 类型/证据 | NOT NULL / PK | DEFAULT证据 |
| --- | --- | --- | --- |
| `fulfillment_id` | `TEXT` T | 有差异 | 无声明 |
| `stage_key` | `TEXT` T | 有差异 | 无声明 |
| `attempt_no` | `INTEGER` T | 可空 | 无声明 |
| `adapter_version` | `TEXT` T | 可空 | 无声明 |
| `price_contract_id` | `TEXT` T | 可空 | 无声明 |
| `state` | `TEXT` T | 有差异 | 无声明 |
| `card_id` | `TEXT` T | 可空 | 无声明 |
| `id` | `TEXT` T | 可空；PK | 无声明 |
| `expected_tier` | `TEXT` T | NN | 无声明 |
| `price_signal_amount` | `REAL` T | 可空 | 无声明 |
| `price_signal_min` | `REAL` T | 可空 | 无声明 |
| `price_signal_max` | `REAL` T | 可空 | 无声明 |
| `price_signal_time` | `TEXT` T | 可空 | 无声明 |
| `updated_at` | `TEXT` T | NN | 无声明 |
| `submit_permitted_at` | `TEXT` T | 可空 | 无声明 |
| `matched_auth_id` | `TEXT` T | 可空 | 无声明 |
| `settlement_state` | `TEXT` T | 可空 | 无声明 |
| `membership_observation_id` | `TEXT` T | 可空 | 无声明 |
| `confirmed_at` | `TEXT` T | 可空 | 无声明 |
| `created_at` | `TEXT` T | NN | 无声明 |
| `adapter_path` | `TEXT` Q（推断） | 未知 | 未知 |
| `page_fingerprint` | `TEXT` Q（推断） | 未知 | 未知 |
| `page_permit_kind` | `TEXT` Q（推断） | 未知 | 未知 |
| `page_control_id` | `TEXT` Q（推断） | 未知 | 未知 |
| `page_ready_at` | `TEXT` Q（推断） | 未知 | 未知 |
| `page_facts_json` | `TEXT` Q（推断） | 未知 | 未知 |
| `progression_permitted_at` | `TEXT` Q（推断） | 未知 | 未知 |
| `progression_reported_at` | `TEXT` Q（推断） | 未知 | 未知 |
| `submit_reported_at` | `TEXT` Q（推断） | 未知 | 未知 |

### 5.33 `membership_processor_lease`

本仓库唯一生产CREATE TABLE。default行显式INSERT OR IGNORE，epoch++持久化；owner/token/epoch/status/expiry必须一起校验。所有业务写入复用同事务AssertLeaseTx。

生产引用：[internal/store/lease.go](../internal/store/lease.go)。

DDL证据：[internal/store/lease.go](../internal/store/lease.go)。

观察到的PK/UNIQUE：`(id)`。

| 列 | 类型/证据 | NOT NULL / PK | DEFAULT证据 |
| --- | --- | --- | --- |
| `id` | `TEXT` P | 可空；PK | 无声明 |
| `owner` | `TEXT` P | 可空 | 无声明 |
| `holder_token` | `TEXT` P | 可空 | 无声明 |
| `epoch` | `INTEGER` P | NN | `0` |
| `status` | `TEXT` P | NN | `'stopped'` |
| `version` | `TEXT` P | 可空 | 无声明 |
| `started_at` | `TEXT` P | 可空 | 无声明 |
| `heartbeat_at` | `TEXT` P | 可空 | 无声明 |
| `expires_at` | `TEXT` P | 可空 | 无声明 |
| `last_tick_at` | `TEXT` P | 可空 | 无声明 |
| `last_success_at` | `TEXT` P | 可空 | 无声明 |
| `last_error_code` | `TEXT` P | 可空 | 无声明 |
| `updated_at` | `TEXT` P | NN | 无声明 |

### 5.34 `products`

只读取membership_tier，优先级低于cdkeys.manual_type与metadata.manualType；不代表已包含商品、价格或订单生成的完整商业模型。

生产引用：[internal/processor/intake.go](../internal/processor/intake.go)。

DDL证据：[internal/processor/intake_test.go](../internal/processor/intake_test.go)。

观察到的PK/UNIQUE：`(id)`。

| 列 | 类型/证据 | NOT NULL / PK | DEFAULT证据 |
| --- | --- | --- | --- |
| `id` | `TEXT` T | 可空；PK | 无声明 |
| `membership_tier` | `TEXT` T | 可空 | 无声明 |

### 5.35 `redeem_orders`

Go消费/恢复的订单字段；schema并不说明订单认证、租户隔离、CDK扣用或完整订单状态机。extension_delivery_*字段仅见测试DDL，当前catchUp只依据加密Session存在性推进本地身份激活。

生产引用：[internal/processor/checkout.go](../internal/processor/checkout.go)、[internal/processor/eligibility.go](../internal/processor/eligibility.go)、[internal/processor/intake.go](../internal/processor/intake.go)、[internal/processor/payment.go](../internal/processor/payment.go)、[internal/processor/reconciliation.go](../internal/processor/reconciliation.go)、[internal/processor/session_recovery.go](../internal/processor/session_recovery.go)。

DDL证据：[internal/processor/checkout_flow_test.go](../internal/processor/checkout_flow_test.go)、[internal/processor/eligibility_dependency_test.go](../internal/processor/eligibility_dependency_test.go)、[internal/processor/eligibility_test.go](../internal/processor/eligibility_test.go)、[internal/processor/intake_test.go](../internal/processor/intake_test.go)、[internal/processor/reconciliation_test.go](../internal/processor/reconciliation_test.go)。

观察到的PK/UNIQUE：`(id)`。

| 列 | 类型/证据 | NOT NULL / PK | DEFAULT证据 |
| --- | --- | --- | --- |
| `id` | `TEXT` T | 可空；PK | 无声明 |
| `session_payload` | `TEXT` T | NN | 无声明 |
| `site_id` | `TEXT` T | 有差异 | 无声明 |
| `product_id` | `TEXT` T | 有差异 | 无声明 |
| `status` | `TEXT` T | 可空 | 无声明 |
| `error_message` | `TEXT` T | 可空 | 无声明 |
| `completed_at` | `TEXT` T | 可空 | 无声明 |
| `updated_at` | `TEXT` T | 可空 | 无声明 |
| `extension_delivery_status` | `TEXT` T | 有差异 | 无声明 |
| `order_no` | `TEXT` T | NN | 无声明 |
| `cdkey_id` | `TEXT` T | 可空 | 无声明 |
| `extension_delivered_at` | `TEXT` T | 可空 | 无声明 |
| `created_at` | `TEXT` T | NN | 无声明 |

<!-- END GENERATED CONTRACT MATRIX -->

## 6. 验收与后续实施顺序

1. 本地 CLI 校验：正常合成请求通过；未知封装字段、尾随JSON、超限、缺sessionToken、错误到期、声明邮箱不符、非plus目标和非法价格契约均停止。所有错误输出不含原始输入。
2. fake executor 验证两种请求都没有 Material/Guard/业务 Binding。Session 请求固定为 `ModeSessionPreflight`、OnHandoff 为空；交互请求固定为 `ModeInteractivePreflight`、Session/Cookies 为空、OnHandoff 仅通知固定码，且浏览器工厂收到 headed=true。若执行器报告任何 progression 或 submit，结果必须按边界违规拒绝。
3. 输出测试：未知 error code 被替换，原始 URL/token/email/error cause 不回显；inspect 只输出固定字段名称与类型；browser 配置失败不回显配置 URL 或文件内容；交互 handoff 不透传私密页面对象。回环 mock CDP 注入含合成敏感标记的畸形事件，验证 chromedp 的实际异步诊断进入显式回调，quiet 策略下隔离测试进程 stdout/stderr 不含该标记。
4. 取消测试：未结束的 stdin 管道可以由 context 取消或截止时间中断；CLI 端的合成二进制检查分别发送 SIGINT/SIGTERM，在输入端仍未关闭时收到安全的 `PREFLIGHT_CANCELLED` 并退出。浏览器模式没有在真实站点执行。
5. 真实站点验证另行基于用户授权与可用访问能力；本轮不通过其它工具或网络通道绕过被拒绝的访问。合成测试通过不代表当前 ChatGPT 页面和价格契约已实测。
6. 只有在决定正式单库所有权、补齐表/列/约束契约、迁移回滚和权限/订单入口后，才实施完整 standalone worker。不得靠本文件的 fixture 并集自动生成生产 DDL。
7. 完整资金路径上线前继续保留原来的卡台余额/产品/交易验证、funding 幂等、Permit、快照、canary/automatic、quota 和未知结果对账门禁。

静态清单统计：35张业务表，383个表内列条目，其中60个列条目仅有生产SQL使用证据、未在收集的CREATE TABLE中声明。此统计不包含SQLite内部表，也不声称覆盖未提供的kwRedeem源码或生产迁移。

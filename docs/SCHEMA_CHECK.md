# 只读 SQLite 契约检查与后续单库开发

## 已实现的范围

`cmd/membership-schema-check` 检查一个**已经存在、已停止写入的 SQLite 快照**。它不启动 worker、不读取业务行、不接触 Session 解密、卡台或浏览器，也不生成生产 schema。

实现与声明分别位于：

- [命令入口](../cmd/membership-schema-check/main.go)：显式文件路径、超时、固定错误码和 JSON 输出。
- [检查器](../internal/schemacheck/inspect.go)：只读打开和 schema metadata 检查。
- [机器可读契约](../internal/schemacheck/contract.json)：版本化的已知 SQL 要求与未知项。
- [完整原始矩阵](STANDALONE_CONTRACT.md)：35 张表、383 个列条目的来源、测试声明差异及迁移缺口。

契约矩阵基线为 `0a28f6dbb0e315479980283de81ed5e9db762300`，本次实现核对的源码提交为 `3a3b0f5937fe2da4b8a3b2db05ce9d106870b96e`。这些标识描述审阅范围，不表示已经获得 kwRedeem 的生产迁移、完整订单写入契约或真实数据库验证结果。

## 使用方法

```sh
go build -o /tmp/membership-schema-check ./cmd/membership-schema-check
/tmp/membership-schema-check --database /secure/offline-snapshot.sqlite --timeout 10s
```

数据库文件必须已经存在；不存在的文件不会被创建。`--database` 接受普通文件路径，不接受 SQLite URI、`:memory:` 或目录。文件名中的 `?`、`#` 等字符经 URI 编码后仍作为文件名处理，不能注入 `mode=rwc` 等连接选项。`--timeout` 默认 10 秒，允许 1 毫秒至 1 分钟；SIGINT/SIGTERM 也会取消检查。

命令不读取 `.env`，不调用 `config.Load` 或 `store.Open`，不需要 `KAWANG_PROJECT_ROOT`、`JWT_SECRET` 或卡台凭据。stdout 仅输出 JSON；参数、SQLite 异常和底层文件错误不会原样打印，stderr 只用于固定帮助文本。

### 为什么要求静止快照

普通 `mode=ro` 连接仍可能需要 SQLite 的 WAL 共享内存文件。为使本命令不创建或写入这些文件，实际连接使用：

```text
mode=ro
immutable=1
query_only=1
trusted_schema=0
```

`query_only` 是连接级保护，并会在开始检查时读回验证；`mode=ro` 提供底层文件访问保护。`immutable=1` 避免锁和共享内存写入，但也意味着 SQLite 不会把 WAL 作为当前文件的一部分读取。因此，在打开之前和返回之前，只要发现同名 `-wal`、`-shm` 或 `-journal`，命令就返回 `SCHEMA_SIDECAR_PRESENT`。

命令不会 checkpoint、删除 sidecar、切换 journal mode、复制数据库、恢复日志或修改权限。它还比较主文件身份、大小和修改时间，发现变化时返回 `SCHEMA_SOURCE_CHANGED`。这些检查无法证明所有并发写入都不存在，因此结果始终包含 `SOURCE_QUIESCENCE_NOT_PROVEN`。应由数据库所有者提供一致且停止写入的快照；不要仅复制活动 WAL 库的主文件，也不要为通过检查而删除 sidecar。

这不是面向活动生产数据库的一致性验证或备份工具。后续若需要在线检查，应先由数据库所有者提供受控快照流程，再检查该快照。

## 机器可读契约的含义

契约是检查器内嵌的 JSON 文件，不从输入数据库反推，也不在运行时从 Markdown 拼接。它声明表名、列名、要求生效条件、证据来源、已知唯一键和未知项。以后生产 SQL 改变时，需共同审阅和更新契约版本；不能把此文件直接转换成生产 DDL。

383 个列条目并不都被当成生产必需列：

| 声明 | 含义 | 当前处理 |
| --- | --- | --- |
| `always` | 当前生产 SQL 的已知依赖 | 要求存在 |
| `platform_catalog` | 平台目录存在后进入的库存路径 | `membership_card_platforms` 存在时要求存在 |
| `legacy_catalog` | 没有平台目录时的兼容库存路径 | 平台目录不存在时启用相应唯一键要求 |
| `if_present` | 当前可选表 | 表不存在时不冒充缺失必需表；存在时检查已知列 |
| `observed_only` | 测试中出现的列或具有旧列兼容路径的字段 | 保留来源，不将缺失直接判为不兼容 |

当前 legacy 选择会检查 34 张必需表、358 个必需列；平台目录存在时会检查 35 张表、371 个必需列。检查器始终报告声明覆盖总数 35/383。这个选择只依据 schema 中平台目录的存在，不读取 enabled、credential、provider key 或其他配置行，也不判断卡台是否可用。

以下测试字段没有被误作当前运行时强制要求：`activation_jobs.id`；`fulfillment_interventions` 的 `acknowledged_by`、`feishu_status`、`feishu_sent_at`；`managed_card_transactions.decline_reason_code`；`membership_intake_settings` 的 `created_at`、`created_by`；`redeem_orders` 的 `extension_delivery_status`、`extension_delivered_at`。例如 intake 查询中的 `o.created_at` 属于订单表，不证明 intake 设置表需要同名列。

`card_capacity_reservations`、`funding_intents`、`card_product_policies` 的 `provider_key` 具有列存在性兼容分支，保留为 `observed_only`。`managed_cards.provider_key` 和 `card_inventory_runs.provider_key` 则在平台目录存在时成为必需项，不能只创建平台目录而漏掉库存路径。

依据：[schema_compat.go](../internal/processor/schema_compat.go)、[inventory.go](../internal/processor/inventory.go)、[intake.go](../internal/processor/intake.go)。

### 已知唯一键

生产中的无 WHERE `ON CONFLICT` 明确使用以下六种 target；它们来自五张表，其中卡表按路径选择一种：

| 表 | 要求的完整列集合 | 条件 |
| --- | --- | --- |
| `automatic_checkout_daily_usage` | `scope_id, business_date` | 始终 |
| `card_price_signals` | `card_id, tier` | 始终 |
| `fulfillment_dependency_circuits` | `dependency, scope_key` | 始终 |
| `managed_card_transactions` | `card_id, auth_id` | 始终 |
| `managed_cards` | `upstream_card_id` | legacy 库存路径 |
| `managed_cards` | `provider_key, upstream_card_id` | 平台目录存在 |

检查器接受相同列集合的完整 PRIMARY KEY 或 UNIQUE 索引，不要求索引名称一致。非唯一索引、partial index、表达式索引和额外带一列的唯一索引均不能满足这些 target。索引列顺序不影响这里的冲突目标匹配。

平台目录存在时，即使复合唯一键已建立，遗留的全局 `UNIQUE(upstream_card_id)` 仍会阻止不同平台使用相同上游卡 ID，因而报告 `LEGACY_GLOBAL_CARD_ID_UNIQUENESS`。检查器不会自动删除该索引或改写历史卡号命名空间。

依据：[inventory.go](../internal/processor/inventory.go)、[dependencies.go](../internal/processor/dependencies.go)、[payment.go](../internal/processor/payment.go)、[reconciliation.go](../internal/processor/reconciliation.go)。

### 正式 DDL 与测试证据的区别

`membership_processor_lease` 是仓库中已有正式生产 CREATE TABLE 的表，因此检查器额外核对该表的主键、列 affinity、明确的非空要求、必须允许空值的列以及 `epoch=0`、`status='stopped'` 的默认值。只比较简单字面量；不会执行数据库中任意默认表达式。复杂表达式返回 `COLUMN_DEFAULT_SEMANTICS_UNVERIFIED`，不会猜测它与预期相等，也不会把该项当成已验证。

其他表的 fixture 类型只作为 `typeHint/typeEvidence` 保存在契约中，不强行当成生产类型、NOT NULL、DEFAULT、CHECK、外键或触发器规范。每张存在的上游业务表都会保留 `UPSTREAM_TABLE_CONSTRAINTS_UNVERIFIED`。`fulfillment_interventions` 的两种互相不同的 fixture 唯一键仍明确标为未解决；部分 `INSERT OR IGNORE` 幂等键只具有 fixture/语义证据，不能冒充已获取的上游迁移约束。

列的可写性使用独立的 `writeSources` 证据，与类型证据分开。当前 314 个列条目在生产 INSERT 列清单或 UPDATE SET 左值中有明确写入来源，其中包括 `Queue.updateOwned` 的固定动态 SET 片段。检查器拒绝把这些列声明为 generated column，因为实际 SQL 无法向它们写入；只读的 `products.membership_tier` 等字段不会因为类型来自 fixture 就被误标为可写。具有列存在性兼容分支的字段若实际存在且会被写入，也必须可写。

依据：[lease.go](../internal/store/lease.go) 及 [契约矩阵](STANDALONE_CONTRACT.md)。

## 输出与退出码

输出分为 `missing`、`incompatible`、`unknown` 三个数组。字段只包含内嵌契约中的表名、列名、列集合和固定 code；不输出文件路径、任意 schema 标识符、CREATE SQL、默认值原文、索引名称或业务行。

| 字段或状态 | 精确含义 |
| --- | --- |
| `missing` | 已知必需表、列或完整 UPSERT 唯一键不存在 |
| `incompatible` | 例如用 view/virtual table 代替业务表、正式 lease DDL 不匹配、multi-provider 库仍有旧全局卡 ID 唯一键 |
| `unknown` | 上游约束、复杂默认表达式、源文件静止性、业务行、外部订单生产者及运行时门禁尚未证明 |
| `knownRequirementsSatisfied=true` | 当前启用的已知要求全部匹配；不包含无法验证的已知默认表达式 |
| `productionReady=false` | 当前版本任何结果都保持 false，因为完整生产契约和业务条件尚未验证 |
| `paymentAllowed=false` | 检查器没有付款能力，任何结果都保持 false |
| `rowValuesRead=false` | 查询范围只有 SQLite schema/pragma metadata，没有 SELECT 业务表中的行 |

主状态优先报告 `incompatible`，然后是 `missing`；不存在已知缺口时仍为 `unknown`。即使数据库有全部 35 个表名，也必须逐列、逐项检查，不能因表名齐全而宣称 ready。

| 退出码 | 含义 |
| --- | --- |
| `0` | 固定帮助文本 |
| `1` | 已发现缺失或不兼容的已知 schema 条件 |
| `2` | 参数、文件、sidecar、取消、超时、读取或输出失败 |
| `3` | 未发现已知缺口，但仍有未解决的 schema/生产条件 |

退出码 3 是刻意保留的“尚不能放行”，不能在部署脚本中当成付款 worker 的启动许可。`go run` 还会通过 Go 包装程序报告子进程退出状态；需要准确接收这些退出码时，先 build 再执行二进制。

## 已完成的离线验证

测试只创建合成临时数据库，没有读取真实用户数据库或访问网络。

- 空文件保持零字节；不存在的文件不会创建；只有表名、没有列的 35 表 fixture 不能通过。
- 旧 fixture 会报告生产需要的缺列、完整唯一键和正式 lease 默认值/非空要求。
- 已明确写入的普通列被改成 generated column 时报告不兼容；仅被读取的派生列不会因此误报。lease 默认值检查遵循 SQLite 标识符大小写规则，不把带引号的 Go 十六进制浮点或下划线数字字符串误当 SQLite 数值零。
- partial、表达式、非唯一、超集索引不能充当 UPSERT 完整唯一键；完整反向列顺序可以匹配。
- multi-provider 下复合唯一键仍与旧全局卡 ID 唯一键冲突；目录存在时要求相应库存 provider 列。
- 满足已知声明的合成 fixture 仍输出 unknown，`productionReady/paymentAllowed` 仍为 false。
- 含合成私密行、默认值和文件名的 fixture 不会把这些内容写入报告。
- 文件内容、大小、修改时间、权限和目录项在检查前后相同；WAL/SHM/journal sidecar 被保留原样并拒绝处理。
- 显式 DDL 写入被 `query_only` 拒绝；测试在该连接临时关闭 `query_only` 后，底层 `mode=ro` 仍拒绝 INSERT。

这些测试验证本工具的边界，不证明某个实际生产 schema 完整，也不证明存在可用订单、有效 Session、正确余额或授权付款的业务配置。

## 下一阶段：真正单一数据库迁移

以下是设计顺序，**本轮没有实施迁移、订单 API、表初始化或 worker 启动改造**。

### 1. 先确定已有记录的唯一所有者

确认独立部署接管哪些原 kwRedeem 表、哪些商业字段仍由其他入口写入，以及如何迁移现有订单。整个系统只使用一个 SQLite 文件作为订单、fulfillment、lease、permit、资金请求和证据的事实来源，不增加第二套 membership 数据库、派发队列或投影回调。

必须获得正式迁移来源，补齐外键、CHECK、DEFAULT、唯一索引、触发器和历史版本。重点解决干预记录唯一键冲突、active 账号锁、价格契约有效范围、幂等 funding key、每个 fulfillment/stage 唯一性和平台卡 ID 命名空间。读测试 fixture 的并集无法完成这一步。

### 2. 建立可审阅的版本化迁移

正式确定后的首个版本应包含迁移 version、checksum、适用能力、已批准前置版本和应用时间。空库初始化与已有库升级分别定义；未知版本或未知约束停止升级。迁移需要一致快照、失败恢复方案，以及有代表性的旧版本合成数据库回归，不能只测试全新空库。

默认业务设置采用不可付款状态：禁用 fulfillment、不开启 automatic/canary，不自动签发许可或创建可花资金。保留现有 `BEGIN IMMEDIATE`、lease epoch、fencing、durable permit、资金快照和未知结果 reconciliation。加密格式和账号锁 HMAC 的版本与密钥生命周期另作显式设计，不伪造 kwRedeem 的目录或 `package.json` 来满足旧启动检测。

### 3. 最小可交付纵向范围

第一步可以是同库的“接收订单 → 保存已校验的会话材料 → 等待验证”闭环，终点固定在 `WAITING_SESSION_VALIDATION`。它需要订单/fulfillment 的权威 schema、幂等记录、加密能力、账号锁和审计来源，不需要创建卡、充值或付款。

如果尚未取得 `redeem_orders` 等商业表的正式契约，应先停留在契约和接口实现测试，不通过补一组猜测列来启动完整 worker。最终 API 与 worker 共用同一条订单和 fulfillment 事实；幂等账本只映射原请求到这条记录，不再形成一套等待转发的订单队列。

## 下一阶段：订单 API 的最小契约

建议将第一版入口限定为一个认证后的 `POST /v1/membership/orders`。请求带 `Idempotency-Key`，业务体仅接收目标套餐、用户明确授权的会话材料和服务器支持的商品标识；租户、付款模式、价格契约选择和权限从服务器配置得出。客户端不能指定 card、资金余额、订单完成状态、permit、authorization、`run_mode=automatic` 或到账证据。

单个 SQLite `BEGIN IMMEDIATE` 事务内执行：

1. 绑定认证租户与商品范围，校验请求格式、大小和本地声明身份；本地 Session 可解析不等于远程验证通过。
2. 按租户与 Idempotency-Key 查幂等账本，比较服务器 HMAC 请求指纹。同一 key、相同语义返回既有订单；同一 key、不同语义返回 409，不能创建第二次履约。
3. 使用正式密钥服务加密 Session，绑定目标套餐及服务器价格契约版本。身份不明时不创建“免费账号”观察，也不提前声明已取消续费。
4. 按已定义的账号锁规则防止并行重复履约，在同库创建订单、fulfillment、幂等映射及脱敏审计，初始状态固定为等待验证。
5. 提交后只返回订单标识、初始状态和 `paymentAllowed=false`；不返回 Session、token、完整 checkout URL 或卡材料。

幂等账本的最小字段可包括 `tenant_id`、`request_key`、`request_fingerprint`、`order_id`、`created_at`，使用复合唯一键 `(tenant_id,request_key)`。这是拟定元数据表，不是已安装的迁移；order 外键、保留周期和密钥轮换语义需要与正式订单 schema 一起确定。

并行 API 请求需要测试相同 key 的竞争、同一账号的不同 key 竞争、事务回滚、会话替换的版本比较和过期密钥恢复。批量入口宜复用单订单契约，每个子项保留自己的幂等标识和结果；不能因为批量提交就跳过单订单的许可或资金边界。服务器执行会话应继续按每次任务的独立 profile 和已有 lease/fencing 隔离，不引入共享浏览器 profile 作为账号状态库。

只有这个不付款闭环完成并通过并发/恢复测试后，才继续接回 eligibility、资金准备、checkout 和 reconciliation。只读 schema 报告是迁移证据的一部分，不能替代这些业务验证。

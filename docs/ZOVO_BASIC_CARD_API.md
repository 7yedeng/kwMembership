# Zovo 基础卡 API 接入

更新：2026-11-14（北京时间）。本次只核对公开源码/文档并运行合成 HTTP 与临时 SQLite 测试，没有读取真实密钥、开卡、加款或付款。

## 来源与确认范围

依据 Zovo 官方仓库的 [开放 API 文档](https://github.com/zovocard/zovo_card_cdk_auto/blob/19f41ef347cdb3cce52fde0f2b43e12f70dc6ebe/docs/zovocard-openapi-zh.md)。本轮通过 GitHub 文件接口重新读取默认 `master` 文档，返回 blob `7f0849ef2b82fd73bea1fdfba598431824df694c`，与本地上述提交的文档一致。仓库 [README 官方入口](https://github.com/zovocard/zovo_card_cdk_auto/blob/19f41ef347cdb3cce52fde0f2b43e12f70dc6ebe/README.md) 连接 Zovo 站点及原 SpaceX 频道。

新适配器使用独立 `kind=zovocard`。只复用已经逐项核对的基础响应解码，不把品牌更名作为所有接口完全一致的依据。旧 `spacexcard` 配置及默认地址保持原有行为。

## 配置与接口

生产默认地址为 `https://zovocard.com/openapi/v1`，沙盒为 `https://sandbox.zovocard.com/openapi/v1`。沙盒/生产密钥分别配置；构造客户端不访问上游。凭据 JSON 为 `appSecret` 加可选 `appId`，对应 `X-API-Key` 与 `X-App-Id` 请求头；无需猜测 HMAC 签名。

| 适配器方法 | 实际基础接口 | 说明 |
| --- | --- | --- |
| `ListProducts` | `GET /products` | 开卡产品及账户专属费率 |
| `ListCards` | `GET /cards?page=…&page_size=…&sync=1` | 可请求当前页余额同步，仍受上游同步频率限制 |
| `GetBalance` | `GET /balance` | 使用明确的 `spendable_balance`，不把保留金算作可开卡/加款余额 |
| `GetCardMaterial` / `GetCardSnapshot` | 有界 `GET /cards` 后 `GET /cards/{id}` | 先核对本地 ID 与上游 ID/卡号的归属，再由一次详情响应取得资料、余额及限制商户字段 |
| `ListTransactions` | `GET /cards/{id}/transactions` | 卡消费授权、清算、退款、撤销记录；不是账户充值流水 |
| `OpenCard` | `POST /cards/open` | 用户给定持卡人、产品和初始金额；调用方须先持久化资金意图 |
| `RechargeCard` | `POST /cards/recharge` | 用户给定卡 ID、金额与稳定幂等键 |
| `FreezeCard` | `POST /cards/freeze` | 基础卡状态操作，非付款前测试的一部分 |

没有调用 `/gpt-direct/*`、账号解析、会员建单或代充执行接口。`GetOpenAIPayments` 明确返回 `CARD_PLATFORM_CAPABILITY_UNSUPPORTED`，不会请求 `/cards/{id}/openai-payments`，也不会从三档区间猜测当前会员报价。

现有 worker 的库存/付款选号仍使用旧价格信号方法，因此 **将 `zovocard` 填入旧 worker 并不等于全套库存业务已接通**。本轮新的付款前入口直接使用页面报价和指定卡快照；要将 Zovo 全面接入旧自动选号，应先独立替换其价格证据来源。不会静默回退到代充价格 API。

## 只读单卡快照

`CardSnapshotReader` 是补充接口，配置包装器会透传。`CardSnapshot` 包含指定卡 ID、`CardBindingVerified`、`Material`、`AvailableAmount`、`Currency`、`RestrictedMerchants`、`MerchantRestrictionsKnown` 和可空的 `GoogleChatGPTBlocked`。

官方 `/cards/{id}` 的路径 `id` 为卡台本地数字 ID，详情内的 `card_id` 则是发卡渠道字符串 ID。二者不能直接比较。本实现先查询文档化的卡列表 `id / vm_card_id / card_number`，再要求详情 `card_id` 与该列表的 `vm_card_id` 一致、完整卡号一致；如详情额外返回本地 `id`，也必须一致。只有归属与资料检查全部通过才设置 `CardBindingVerified=true`。

绑定查找每页 100 张、最多 100 页，找到目标后停止；总数变化、重复 ID、未找到、缺字段、提前空页或达到页数上限均停止，不猜测下一页、不读取未绑定卡的详情。不使用未经文档化的按 ID 搜索参数。详情依然只 GET 一次；CVV、余额、账单来自这同一份已核对归属的响应。

- 卡号、CVV、姓名及地址只保留在内存中的 `Material`；快照 JSON 隐藏该字段，格式化整个快照也脱敏。
- 官方卡详情示例没有 `currency`。上游缺失时保持空，不拿结账页 PHP/EGP 数值直接与未标币种的卡余额比较。调用方若另有显式币种契约，应单独标明来源和冲突处理。
- `restricted_merchants=[]` 才是明确空规则；字段缺失或 `null` 是未知。兼容的 `google_chatgpt_blocked` 也保留三态，不把缺失当成 false。
- 官方地址示例是空对象。只有卡台返回或用户明确绑定到此配置 key / 卡 ID 的真实账单资料才可使用；本适配器不生成姓名或地址。
- 这是一份被读取时的快照，不承诺其在后续付款瞬间仍有效。实际付款前需要重新确认余额、限制和报价。

## 写操作的未知结果与幂等边界

文档附录明确幂等键格式为 `A-Za-z0-9._:-`、1–80 字符，且只有 **2xx** 响应会被缓存。卡充值结果未确认还可能返回 HTTP 400 与通用 `request_failed`。因此适配器：

1. 开卡/加款前校验稳定幂等键、参数与有限正数金额；无效输入不发请求。
2. 每次调用发送一次 POST，不做应用层重试；还清除请求 `GetBody`，避免 Go 标准 HTTP Transport 因 `Idempotency-Key` 而自动重放资金请求。
3. 拒绝重定向，凭据不会转发到其他站点。原始错误、URL、响应文案不进入日志错误，返回固定 `ZOVOCARD_*` 代码。
4. HTTP 202、409、5xx、传输异常、损坏响应，以及 HTTP 400 的通用失败/确认中状态，保留 `ZOVOCARD_WRITE_OUTCOME_UNCERTAIN`，`KnownNoWrite=false`、`Retryable=false`。
5. 只有文档明确的认证拒绝及窄范围参数/余额等前置拒绝才标记为已知未执行；不依赖中文 `msg` 匹配。
6. `RechargeIdempotent=false`；补充 `FundingReplayPolicy` 对未知开卡和加款都拒绝重放。processor 的显式恢复入口先检查该策略，保留原资金意图、幂等键和指纹，转交只读核验。旧适配器的恢复策略不改变。

支持一次基础写请求不表示新付款前 CLI 获得资金操作权限。该入口只读既有卡；缺卡、余额不足或资料不完整会停下，不会临时开卡或加款。

## 验证

provider 回归使用本地合成 Transport，覆盖配置 key/域名/凭据隔离、只读单卡快照、列表与详情错卡/卡号冲突、列表未找到/重复/缺字段、分页找到及上限终止、无效状态/到期卡、缺失事实、敏感字段不输出、基础接口范围、幂等键和金额校验、未知写状态、重定向、超大响应、超时脱敏。

processor 回归使用临时 SQLite 中已持久化的未知/孤立提交资金意图，分别走真实 `RecoverFundingOutcome` 入口，验证 Zovo 开卡与加款均返回 `FUNDING_RECOVERY_UNSUPPORTED`，无 HTTP 请求、原证据不变；既有 SpaceX 恢复测试继续通过。

上述验证不代表真实 Zovo 账户已具有权限、充值余额、合适卡产品或可用账单地址，也没有证明某张卡当前可以在目标商户成功支付。

# 付款、权益与续费的对账验收

## 范围与当前结论

本文描述现有 Go processor 的真实状态机，以及本次在临时 SQLite 和合成适配器响应上验证的契约。**这些验证不等于真实充值验收**：没有登录真实账号、调用真实卡台、开卡、加款、付款或取消真实订阅。

当前完整的付款证据链由 `internal/processor/checkout.go`、`reconciliation.go`、`account_service.go` 和 `internal/domain/fulfillment.go` 实现。卡台配置独立于账号服务，使用现有共享数据库；本次不新增 schema、队列或第二份账本。

当前 Go 业务支持的目标仍是 Plus / x5 / x20。严格权益确认仍要求 PHP 账户币种；卡交易的价格快照 `price_signal_*` 则是 USD。**Go / EGP 尚不满足该契约，不能把它标为已支持或据此进行真实验收。** PHP 授权金额和 USD 清算金额可以同时存在，但只能采用对应币种已明确的金额字段；代码不猜汇率，不把 EGP/PHP 数值当 USD。

## 三种“成功”必须分别判断

| 结果 | 必需证据 | 不能据此推断 |
| --- | --- | --- |
| 提交动作完成 | 浏览器动作已跨过持久化 permit 和提交前授权快照 | 不能仅凭页面、HTTP 成功或 executor 返回值确认扣款或会员开通 |
| 业务交付完成 | 本次付款匹配、目标会员权益确认、自动续费明确关闭；fulfillment 为 `COMPLETED` | 仍可能存在 `settlement_state=PENDING`，此时银行清算尚未完成 |
| 已确认清算 | 与该支付 stage 绑定的 `matched_auth_id`，规范化类型为 `Settlement`、状态为 `COMPLETE`，有正数清算金额及明确的对应 USD 币种，金额仍符合价格证据，且没有退款/撤销证据 | 不构成永久的无退款、无争议保证；当前不持续轮询已经清算完成的历史订单 |

`Authorization + COMPLETE` 表示授权状态完成，不能等同 `Settlement + COMPLETE`。修复后的代码将前者的 stage 清算状态保留为 `PENDING`，继续等待清算事件。

## 判定付款成功所需的持久化证据

1. **动作边界**：本 stage / attempt 的 submit permit 和其提交前 `auth_id` 快照必须存在。不能在对账时重建一份“现在的快照”替代原边界，更不能重新提交来补证据。
2. **同卡归属**：stage 绑定的 `managed_cards.id` 决定交易归属；该卡的 `provider_key` 决定卡台配置账号。适配器种类相同，不代表配置账号相同。不同卡台账号即使复用 upstream card ID 或 auth ID，也不能共享对账结果。
3. **本次交易增量**：使用提交时间之后首次观察到、已持久化的同卡交易，并排除原快照内的 auth ID。提交之前的旧账不能因为新一轮 API 空页被误认成本次付款。
4. **金额及币种**：必须显式提供 USD 价格基准。优先使用正数清算金额及其清算币种；普通授权证据可以使用授权金额及授权币种，但 `Settlement + COMPLETE` 必须有其自身的正数清算金额，缺失、零或负数不能回退授权金额。金额字段和币种字段必须成对，不能借用另一笔事件旧金额的币种。同一批次合并和跨轮次入库都遵循这一约束。缺失或不一致时保留未知。
5. **唯一匹配**：商户、金额范围和交易状态满足既有领域契约，只有一个有效的新交易候选。多个候选进入不确定状态，不能任取一个确认。
6. **账号权益**：通过账户服务重新取得该 Session 所属账号的观察，目标 tier、PHP 币种、未来有效期及无欠费状态同时满足严格确认。拥有服务还验证账号身份与观察时间；未知字段不能推导成已开通。
7. **续费回读**：取消接口返回成功仅是确认收到请求。必须重新读取账户观察，明确 `auto_renew=false` 才能完成最终业务交付；owned account service 的该观察不能早于取消完成时间或前一观察。

授权可能先出现、权益稍后才出现，也可能反过来。两者分别持久化、分别验证。单独一边成功不能把订单标为完成。

## 异常与恢复矩阵

| 情况 | 现有行为与验收要求 |
| --- | --- |
| 提交返回成功，但尚无付款/权益证据 | 停在 `PLUS_SUBMIT_PERMITTED` / `UPGRADE_SUBMIT_PERMITTED` 等待对账；回调不直接确认 stage |
| 有明确 submit permit 的提交超时 | 转入 `PAYMENT_OUTCOME_UNCERTAIN`；保留原 permit、attempt 和 baseline，仅继续查证，不重新执行 checkout |
| 资金边界之后执行器整体丢失 | `EXECUTOR_OUTCOME_UNCERTAIN` 保持人工恢复入口；普通 checkout 调度不重放，也不假装已经自动恢复 |
| 卡交易延迟、会员权益已出现 | 保留 `PAYMENT_OUTCOME_UNCERTAIN`，等待卡台证据；不再次付款 |
| 卡授权已出现、会员权益延迟 | `PLUS_RECONCILING` / `UPGRADE_RECONCILING` 等待权益；后续空页或新 processor 实例仍使用已保存的同卡交易 |
| 已知 pending 授权，后续 API 返回空页 | 不能把已有授权抹掉，也不能写出“无有效交易 / 无 pending 授权”的通过证据 |
| 退款、撤销后出现旧授权页 | 已保存的退款/撤销优先级不能被旧授权覆盖；不能恢复成付款成功 |
| 待清算交易变更金额或缺失/更换币种 | 转入 `PAYMENT_OUTCOME_UNCERTAIN`，记录 `POST_COMPLETION_TRANSACTION_CHANGED` 待复核；不标为清算完成 |
| 第二个 worker 同时启动 | 未过期 lease 阻止第二个实例获得执行权；租约接管后，旧 epoch 不能落付款观察或推进状态 |
| 取消续费超时，但服务器可能已执行 | 下一轮先 Observe；观察已为 false 则完成，不盲目重复取消；仍为 true 才继续既有取消逻辑 |
| 取消成功但观察缺字段、过旧、仍为 true | 不能确认已关闭续费；保持重试或未知状态 |
| `COMPLETED` 且清算待定 | 继续查询绑定卡台账号的待清算交易，直到取得清算证据或发生异常 |

`5m`、`1h`、`24h` 的无付款检查是供恢复/人工判定使用的观察记录，不是自动重扣授权。所有这些检查仍必须受到已有交易证据约束。

## 本次已复现并修复的问题

| 原问题 | 修复及回归证据 |
| --- | --- |
| 落盘交易未参与下一轮匹配：先见授权、后见权益且本轮空页时丢失付款证据 | 从同卡、提交边界之后的规范化交易记录重建对账输入；`TestReconciliationDelayedEntitlementUsesDurablePaymentAfterRestart` |
| $1 pending 授权曾出现，24h 后空页却写出三份“无付款 / 无 pending”记录 | 无付款检查和付款匹配共同使用持久化证据；`TestReconciliationKnownPendingCannotBecomeNoPaymentOnEmptyPage` |
| 金额符合区间但币种缺失仍可成功，且更新金额会借用旧记录币种 | `PaymentDeltaOptions.ExpectedCurrency` 必须显式为 USD；同批次 collapse、inventory 和 reconciliation 的金额/币种成对更新；`TestPaymentTransactionMatchingRequiresAmountCurrencyEvidence`、`TestReconciliationCurrencyCannotBeBorrowedFromEarlierAmount`、`TestReconciliationSamePageCannotBorrowCurrencyFromAnotherAmount` |
| 已授权的付款进入清算分支后只检查 `status=COMPLETE`，跳过金额/币种复核 | 清算刷新再次验证绑定交易的金额、币种、类型及状态；`TestReconciliationSettlementNeedsAmountAndCurrencyEvidence` |
| 标为 `Settlement + COMPLETE` 但缺少正数清算金额时，回退授权金额并误报清算完成 | 统一 matcher 拒绝缺失/零/负数清算金额，初次确认和后续刷新共同受约束；`TestPaymentCompleteSettlementRequiresItsOwnPositiveAmount`、`TestReconciliationCompleteSettlementCannotBorrowAuthorizationAmount` |
| 授权 `COMPLETE` 被直接标为清算 `COMPLETE` | 只有 `Settlement + COMPLETE` 可以完成清算；`TestReconciliationCompletedAuthorizationIsNotSettledPayment` |
| 旧 schema 不含 `managed_cards.provider_key` 时，付款后的待清算读取报错 | 使用与付款阶段读取一致的旧版 SpaceX 默认配置兼容路径；新 schema 仍按精确配置 key 路由；`TestReconciliationSettlementUsesBoundConfigurationAndLegacyFallback` |

额外契约覆盖了仅有会员权益不够、同卡旧账与其他卡交易不能混用、退款/撤销不能被旧页覆盖、提交成功/超时不重放、worker 接管拒绝旧实例写入，以及取消续费超时后的先回读行为。原有 post-cancellation freshness、lease loss、reservation/capacity 与资金幂等测试仍保留。

本次实际执行：

```sh
GOTOOLCHAIN=local GOPROXY=off go test ./internal/domain ./internal/processor -count=1
```

两个包通过。上表的问题均先写契约测试，在旧逻辑上观察到相应失败，再修复；未用生产接口、真实 Session 或卡片替代测试夹具。新增测试和既有合成测试只能证明这些本地契约，不能证明卡台/账号外部接口此刻可用。

## 后续真实单笔验收清单

当前不执行以下操作。待拥有外部配置、合法账号和该笔实际付款授权后，在**受支持的套餐及币种契约**内执行一次 canary，并保留可复核的非敏感证据。

1. **绑定及限额**：确定目标账户身份、套餐、账单币种/金额、卡台配置 key、managed card ID 和允许的付款金额范围。确认卡台资金事实来自实际读取，不能用配置值或卡号存在作为余额证据。
2. **付款前观察**：读取账号当前权益，建立同卡交易 baseline；保存 fulfillment ID、stage/attempt、permit ID、价格契约版本和 baseline 时间。不要记录 Session、卡号、CVV、完整 checkout URL 或解密凭据。
3. **单次提交**：确认 durable permit 与快照已提交到共享库，再激活一次动作。成功响应与超时都交给同一套对账流程；任何未知状态都不能人工“再点一次试试”。
4. **卡台对账**：观察本次新增 auth ID、授权/清算类型、状态、金额及对应币种。相同 auth ID 的重复页只能更新该条证据，不能累计为多次扣款。金额或币种有冲突时停在待复核状态。
5. **权益对账**：在预期账号上重新确认目标套餐及有效期。记录观察时间和其与付款边界的先后关系；原始提交页面或返回的“成功”字符串不替代这一观察。
6. **续费对账**：取消后再观察，确认自动续费关闭且当前权益仍有效。取消请求超时则先观察实际状态。
7. **清算对账**：区分业务 `COMPLETED` 和 financial settlement `COMPLETE`。如仍为 `PENDING`，报告“已开通，待清算”；只在绑定交易真实 `Settlement + COMPLETE` 且证据仍符合时报告“清算已确认”。
8. **重启恢复**：在安全的只读对账阶段重启一个 worker，确认仍使用原 attempt、permit、baseline、card/config key 和 auth ID；核对没有第二次扣款或新的付款动作。
9. **验收记录**：汇总上述 ID、状态、非敏感金额/币种、观察时间、是否发生挑战/人工操作及最终续费状态。若有任一缺口，保留具体状态并报告未通过，不能用本地测试绿色代替真实证据。

## 当前边界

- 没有新增或伪造官方账户、取消续费、发卡或支付接口。真实账户服务、卡台响应、挑战处理和实际清算仍需分别验收。
- 既有 Plus/x5/x20 的 PHP 权益契约与 USD 卡台价格证据保持不变；支持 Go/EGP 需要先提供其价格、套餐映射、账户观察及币种契约，不能仅修改显示名称。
- 本次没有数据库迁移或历史数据回填。旧版本曾被误标为 `COMPLETE` 的历史记录不会自动纠正，应基于真实交易证据审计。
- 已清算完成的记录不做永久退款/争议监控；后续若需要该能力，应独立定义查询周期和状态契约。
- 对账保留持久化已知事实，但无法从一个缺少关键字段或持续不可用的外部系统创造缺失证据。此类记录保持未知/待复核，不能靠重扣解决。

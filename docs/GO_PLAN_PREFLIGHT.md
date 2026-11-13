# Go 套餐预检、埃及币种与真实交易边界

核验日期：2026-11-13。本页描述本分支的开发能力和官方公开产品信息，不记录真实 Session、邮箱、卡片资料或付款链接。

## 1. 当前可执行范围

| 入口 | 套餐范围 | 执行动作 |
| --- | --- | --- |
| `membership-preflight --inspect-session` | 不判断套餐 | 仅本地检查已知 Session 字段的存在性、类型及本地到期时间；不证明远程登录有效。 |
| 默认 `--check` | Plus 自动 Session 请求契约 | 仅本地校验；Go 返回 `PREFLIGHT_TARGET_UNSUPPORTED`，不构造浏览器。 |
| `--run` | Plus | 复用既有 Plus checkout 请求；可能创建远程结账会话，不填卡或付款。Go 在启动浏览器前被拒绝。 |
| `--interactive-preflight` | Plus/PH/PHP；Go/PH/PHP 或 Go/EG/EGP | 新建独立 profile，用户自行登录并进入结账页，代码核对身份和明确的页面事实；不自动创建 Go checkout，不填卡、不推进、不提交。 |
| 默认 worker / 付款 executor | 原有 Plus、x5、x20 工作流 | 本次没有加入 Go 资金规则、Go SKU、Go 自动付款或 Go 自动取消适配器。 |

Go 交互请求要求 `Stage=go`、`TargetTier=go`、`PriceContract.Tier=go`。执行器同时拒绝 Cookie、原始 Session、卡材料、付款 Guard、履约 Binding 和预制付款链接。其他 mode 不能利用 Go 请求进入付款分支。

Go 浏览器执行器与 Python 队列入口共同使用 [ValidatePlanContract](../internal/checkout/plan_contract.go)，在启动浏览器或创建命令前校验当前阶段的套餐契约。高档位订单仍可以先经过 Plus 阶段，该阶段使用 Plus 价格契约；升级阶段才要求 x5/x20 契约。把 Go 价格契约塞进 Plus 或升级请求同样被拒绝。

页面复核要求观察到的 plan 与目标和价格契约均一致，国家和币种与请求契约匹配，应付金额在给定有限正数区间内。埃及 Go 必须显式提供 `priceContract.country=EG`、`currency=EGP`；只有旧 PHP 契约允许省略 country，并兼容为 PH。缺少国家的 EGP、PH/EGP、EG/PHP 均拒绝。EG/EGP 不开放给自动付款、Session 预检或 Plus/升级请求。Go 请求遇到 Plus 页面会失败；Plus 请求遇到 Go 页面也会失败。`paymentAllowed` 始终为 false。

`PriceContract.Country` 表示预期结账市场，与卡片的 `BillingAddress.Country` 分开。空 Country 使用 `omitempty` 保持旧 worker 的 IPC JSON 兼容；本次不增加数据库列，也不修改自动 Plus 脚本的 PH/PHP 和原 SKU。

金额区间必须来自实际展示的报价和用户限定的预算。测试中的 100–200 等数值全部是合成 fixture，**不是 Go 或 Plus 的当前售价**。

## 2. 页面识别尚未得到真实 Go 验证

当前 `inspectFrameJS` 读取明确的套餐、国家、币种和金额属性，例如 `data-kw-*`、指定 `data-testid` 元素上的 `data-*`、以及白名单 meta 标签。它识别显式 `go`、`EG`、`EGP` 值，没有取得证据证明真实 Go 结账页具有这些属性。国家或币种缺失、未知时保持未知，不从正文、URL、配置或代理出口推断。

因此，“离线 Go 事实能够通过分类”不能写成“真实 Go 页面已适配”。属性缺失或页面布局不支持时，应继续停止；不能向真实页面注入 fixture 属性，也不能因为页面文字出现 Go 就推断当前购买的套餐。

交互身份观察现已拒绝非空认证错误；在账号来源上重新观察失败或返回空身份时，会清除此前已验证标记。浏览器只输出脱敏的结果字段，不导出登录凭据。

既有自动 Plus 脚本把明确的有效 Go 订阅加入已付费保护列表，避免将该事实当作免费账号。它仍然只构造原有 Plus SKU；这没有证明 Go 订阅 API 的所有真实响应形式已被覆盖。

## 3. EGP、EG、PHP、PH 与 IP 的区别

| 名称 | 含义 | 能证明什么 |
| --- | --- | --- |
| Go | 订阅套餐 | 应与结账页、订单和后续权益观察一致。 |
| EG | 埃及国家代码 | 埃及 Go 交互观察要求的明确国家值。 |
| EGP / E£ | 埃及镑 | OpenAI 官方列出的埃及订阅计费币种。 |
| PH | 菲律宾国家代码 | 旧契约及现有 Plus 自动执行使用的国家值。 |
| PHP / ₱ | 菲律宾比索 | OpenAI 官方列出的菲律宾订阅计费币种。 |
| 代理出口 IP | 网络连接来源 | 不能单独证明账单币种、价格、账号资格或卡片可支付。 |

OpenAI 的 [Go 说明](https://help.openai.com/en/articles/11989085-what-is-chatgpt-go) 写明 Go 在 ChatGPT 支持的国家可用；[支持国家列表](https://help.openai.com/en/articles/7947663-chatgpt-supported-countries) 包含埃及和菲律宾。

[多币种计费说明](https://help.openai.com/en/articles/10421635-multicurrency-billing) 列出 EGP/Egypt 与 PHP/Philippines。它要求在完成购买前核对结账显示的价格和币种；本地价格可能不同于美元价格简单换算。公开文档没有提供本分支可据以写死的实时埃及 Go 金额。测试中的 193 仅为合成值；卡台参考报价也不能直接作为目标账号的最终账单或付款许可。

公开说明没有给出“地区 IP 必然获得该地区报价”的保证，也没有公开完整的币种选择算法。不能把代理切换当作已经完成币种切换。

后续服务器可以为合法业务配置按任务固定的网络路由，但需要持久化选择、隔离和错误处理。当前进程级代理不等于按订单路由，具体审计见 [SERVER_ISOLATION_AND_IPC.md](SERVER_ISOLATION_AND_IPC.md)。本次没有使用代理、其他浏览器或原始 HTTP 绕过受管浏览器的站点拒绝。

IPC 复核确认当前失败在 `socket(AF_UNIX, ...)` 创建阶段，尚未执行 `bind`。对抽象 socket、路径替换、socketpair 与 TCP 方案的逐项判断见 [IPC_DIAGNOSIS_FOLLOWUP.md](IPC_DIAGNOSIS_FOLLOWUP.md)。该环境限制尚未修复；地区契约测试通过不代表本机 Chromium 已能启动。

## 4. 卡台材料与取消续费

卡台开卡成功不能单独证明目标商户接受该卡。[OpenAI 的拒付说明](https://help.openai.com/en/articles/7232916-why-was-my-credit-card-declined) 要求正确的卡信息、账单地址和可用余额，并指出银行、地区和持卡人认证也可能影响支付。

本分支从同一卡片的详情或明确的按卡配置取得持卡人/账单地址。不能根据目标币种覆盖卡片的真实账单国家，也不能用随机地址生成器冒充发卡行记录。

[官方取消说明](https://help.openai.com/en/articles/7232927-canceling-your-chatgpt-subscription) 对网页订阅的路径为：账户菜单 → Settings → Billing → Cancel plan → Cancel；通过 Apple 或 Google 购买则在原渠道取消。应至少提前 24 小时取消以避免下一周期扣费，通常保留当前已付周期的权益。取消不自动退款。

程序的取消完成条件应是新观察确认自动续费已关闭，而不是只收到一次操作响应。本分支已有这一服务接口和结果复核，但真实 Go 账户观察与取消动作仍未实现、未验证。本轮没有为真实账号开通 Go，也没有执行取消续费。

## 5. 当前访问限制

受管浏览器对用户新登录的 ChatGPT 标签页再次返回 `host restrictions gate` 拒绝。该限制发生在工具取得站点读取权限时；用户已登录和 Session 本地结构有效都不能解除它。当前工具未提供关闭这一门禁的选项。

本轮真实附件只进行了本地结构和到期时间检查，没有复制到项目、发送到 CI 或通过其他客户端访问被拒绝的站点。后续真实验收依赖受允许的站点访问能力、实际 Go 报价、对应卡产品费用和明确交易范围；离线测试不能替代这些事实。

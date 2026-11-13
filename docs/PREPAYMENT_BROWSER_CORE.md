# 独立浏览器的付款前准备核心

## 范围与实际状态

`ChromeExecutor.PreparePrepayment` 是单任务验收入口。它复用共享 Session 核心，在自己创建的临时 Chrome profile 内完成登录核验、已有 Plus 结账入口、真实页面报价读取、账单与卡字段填入、精确回读，最后报告“待确认”。接口没有确认付款命令，不接生产 worker 的付款调度，不引入数据库或队列。

目前支持的请求目标严格为 `plus / PH / PHP`，对应仓库中已有、静态审阅过的 Plus 请求形状。该值是**请求的结账市场**，并不证明账号的地区资格，也不替代持卡人的真实账单地址。Go 套餐与其他市场没有经过此入口的适配；不能把 Plus SKU 改成猜测的字符串后当成支持。

**本轮没有登录真实 ChatGPT 账号、访问真实付款页或提交卡台资金操作。** 管理环境已明确限制相关目标，后续测试只使用真正的 localhost/127.0.0.1 页面，没有映射或代理真实服务域名。源码与合成页面验证不能称作实际商户全链路验收。真实页面缺少受支持事实时，程序明确停止并输出缺项。

## 生命周期与模块边界

入口类型在 `internal/checkout/prepayment.go`，浏览器实现为 `prepayment_chrome.go`。CLI/桌面包装见 `PREPAYMENT_TOOL.md`。

1. 先检查 Session 格式、预算上下限、精确套餐目标和卡材料输入方式。错误输入不会启动浏览器。
2. 创建全新临时 profile 和单独 Chrome 进程。调用方不能传入现有浏览器的远程调试地址。可选可见窗口用于验收观察；生产 executor 的 Xvfb/worker 合约不变。
3. 在首次站点导航前安装付款前防误触措施，再把**同一个** Chrome context 借给 `sessionrestore.UseChromeContext` 和 `Apply`。共享核心写入 Cookie、精确回读属性、检查远端用户 ID/邮箱，既不创建另一个浏览器，也不接管关闭权。
4. 再次绑定 Session 身份；只有明确返回 `free` 的资格响应才发出既有 Plus checkout 请求。现有订阅、身份变化、未知资格、未知 checkout 响应形状均停止。
5. 观察可信结账页面，先取得明确套餐与币种。`ResolveBilling` 只提供卡绑定的实际账单地址；填写账单之后等待税额与合计稳定，解决“缺账单就没有最终税额”的顺序依赖。
6. 所有必需报价事实齐备后，用整数最小货币单位校验算式与用户预算，再调用 `ResolveMaterial(ctx, ReviewedQuote)`。卡台查询/绑定由外层适配器负责，浏览器核心不依赖 Zovo，也不自动开卡或加款。
7. 材料中的账单必须与先前账单逐字段一致。填写可信 frame 中的卡与账单字段，然后重新读取报价；即使新价格仍在预算内，只要与账单完成后的基准报价不同也停止。
8. 两次最终检查均只读字段，不重新填写。要求字段精确回读通过、报价不变、最终控制唯一且启用。出现“下一步/继续”等进展控制时返回 `PREPAYMENT_PROGRESSION_REQUIRED`；本入口没有 durable permit，因此不会点击越过它。
9. `OnReady` 输出脱敏观察结果。可选 `HoldOpen` 最长 5 分钟，期间没有付款确认指令；检测到已知提交尝试立即撤销 ready。正常等待结束后再次读取报价和字段。
10. 成功、取消和失败都关闭拥有的浏览器并删除临时 profile。Windows 退出后仍可能短暂持有文件句柄，程序仅对本任务创建的临时目录进行有界删除重试，固定等待总量不超过 2.9 秒；持续删除失败不能保留成功结果，也不会杀掉用户其他 Chrome 或修改权限。

直接传入 `Material` 的调用方提前持有全部材料，但代码仍按“账单→报价→卡字段”的次序发送。外部 resolver 可在第一次只读卡查询中缓存同一卡记录，两阶段调用不得换卡。卡号、CVV、Session、凭据及 checkout 长链不进入报告、错误文本或常规日志。

## 报价证据契约

新路径只读 `prepayment_scripts.go` 中的可见语义 DOM；没有把旧 `data-kw-*` fixture 属性当成真实报价，也不会扫描任意页面全局对象或私有 Stripe 内存。

| 事实 | 支持的可见形状 | 不能确定时 |
| --- | --- | --- |
| 套餐 | 唯一 `ChatGPT Plus` / `Subscribe to ChatGPT Plus` 等明确标题 | `missingFacts: plan` 或目标不符 |
| 币种 | 对应金额旁 PHP/EGP/USD 标识或明确支持的货币符号；同页必须一致 | `currency` 未知或目标不符 |
| 小计、税额、折扣、合计 | 明确语义标签与唯一邻接金额；支持严格千位分组和两位小数 | 对应字段未知 |
| 零税/零折扣 | 页面明确给出零金额、`Tax exempt` / `No discounts` 等 | 不将缺省行假定为零 |
| 含税方式 | 明确含税/未税标签，或完整显式金额只能满足一种算式 | `taxIncluded` 未知 |
| 市场 | 原始明确 Plus 请求中的 PH，报告来源 `requested_checkout_market` | 不是从 PHP 或账单国家猜测 |

重复但不同的同类金额、同时出现不同套餐/币种、正在计算的税、隐藏行和无法辨认的标签都不会生成 ready。若真实页面不显示折扣行且没有明确“无折扣”说明，本版会阻断；这是一项真实适配缺口，不能靠在网页上加零值标签消除。

稳定观察需要连续相同快照；中途读取错误会清除连续性，不能在超时后沿用旧报价。账单填入后仍有缺项则继续等待到页面超时；只能返回最新读取成功的缺项报告，完整但没有稳定下来的快照不能通过。报价复核使用 `ReviewQuote`，不以浮点数比较钱，不将卡余额的 USD 与页面 PHP 数额直接比较。

Stripe 官方 Custom Checkout Session 文档给出了 `currency`、`total`、`subtotal`、`discount`、税额及税状态等结构；但 Hosted Checkout 页面不保证把该对象公开到全局，本版没有假装能取得它。官方 WebMCP 需要运行时发现当前工具及 schema，也不是所有付款页都有；本版未调用任何 WebMCP 付款或表单工具。[官方 Session 对象文档](https://docs.stripe.com/js/custom_checkout/session_object)；[官方 WebMCP 文档](https://docs.stripe.com/agentic-commerce/agents/webmcp)。

## iframe、字段与提交防误触

主页面允许来源保持为 `chatgpt.com` / `pay.openai.com`，支付字段 frame 另外允许 `js.stripe.com`。每个 frame 的真实来源由该 renderer 内读取。其他 iframe 即使有相同卡字段名字也不接收材料。

`prepayment_frames.go` 将 FrameID 与拥有它的长期 Target context 一起保存。主 renderer 引用远程 frame 但不能执行时，必须由属于当前页面的已附加 iframe Target 补齐；没有完整证据就重试或失败。操作派生的短 context 只取消这次读取，不调用 child 的 CloseTarget。不会遍历或接管用户其他浏览器/标签页，也不关闭 site isolation 来让测试通过。目标 detach/destroy 后不仅删除 registry，还取消该 target 及其后代的**证据 context**；已返回给调用方的旧 frame 事实也立即失效。此取消不关闭原 chromedp target，原请求监听仍遵循拥有者的生命周期。

字段填写复用已有受限选择器与原生 value setter，发送正常 input/change 事件以触发表单计算。回读比较归一化后的卡号、有效期、CVV 和账单值，并检查字段 validity；最后两次核验不发送 input/change。`readOnly` 分支不会把页面清掉的值重新填回后再声称验收成功。

`prepayment_fence.go` 另外提供两类防误触：

- 在页面和已拥有的 iframe 内阻止原生 form submit/requestSubmit 与捕获阶段 submit 事件。尝试通过 CDP Runtime binding 报告给 Go，权威状态保留在 Go 中。
- 在主 Target、已附加 iframe 和支持 Fetch 域的已附加 worker 内使用 CDP Fetch 拦截已知资金确认路由，包括 `payment_intents/.../confirm`、`setup_intents/.../confirm`、`payment_pages/.../confirm`、`checkout-api/confirm` 等。输入处理器自动发起这些请求也会被阻断。普通税额计算和结账初始化没有被误列为资金确认。

新附加 renderer 先安装各自防护再恢复。自动生命周期通道保持非 flat 形式，记录它的原始 SessionID 和父 Target，通过公开 CDP `Target.sendMessageToTarget` 发送唯一固定的 `Runtime.runIfWaitingForDebugger` 命令，等待该原始 session 的确认。发送与确认共用不超过 5 秒的期限，失败会中断正在等待的导航并清理浏览器，不再空等整个任务超时。字段操作继续走 chromedp 的独立 flat session；不能误把第二个 session 的恢复命令当成原始暂停已解除。每个后来附加且正在等待的自动 session 都单独完成同一确认流程。

worker 没有 Page/form 域，单独安装请求防护，并用标准 auto-attach 递归保护嵌套 worker。当前 Chrome 不支持相关命令时明确停止，不把部分覆盖称作完整防护。无法建立防护、未知关联 target 类型、未完成附加或仍未确认恢复的会话不能出具 ready。实现不禁用站点 worker、不改网站权限策略，也不绕过验证码或 3DS。worker 的运行兼容性需要独立真实浏览器用例，不能从 iframe 用例推断其已验收。

这不是对任意私有商户 API 的穷尽证明。准确保证是：**没有主动点击/提交路径，并阻断已知资金确认端点与原生表单提交**。未知端点、站点未来变更或不支持的浏览器行为仍需显式适配与验收，不能把 `submissionFenceVerified` 解读成“网络绝无任何副作用”。

## 报告判定

`PREPAYMENT_REVIEW_READY` 只表示检查时刻的付款前准备状态。`paymentAllowed` 和 `submitActivated` 始终为 false。外层还会核对卡台响应与请求卡 ID 的绑定、商户能力、同币种余额、目标指纹和最终清理。

典型阻断码包括：`PREPAYMENT_ALREADY_SUBSCRIBED`、`PREPAYMENT_ELIGIBILITY_UNCONFIRMED`、`PREPAYMENT_QUOTE_INCOMPLETE`、`PREPAYMENT_QUOTE_CHANGED`、`PREPAYMENT_FIELDS_NOT_VERIFIED`、`PREPAYMENT_PROGRESSION_REQUIRED`、`PREPAYMENT_SUBMISSION_BLOCKED`、`PREPAYMENT_FENCE_UNAVAILABLE`。这些码不包含原始 HTTP/CDP 错误或敏感材料。

## 验证方法及边界

单元测试覆盖身份、资格、报价缺项/币种/预算、两阶段账单、账单被换、卡字段被重置、后填报价变化、进展按钮、未知提交防护、取消/hold 与清理失败。OOPIF helper 的独立测试覆盖正确 renderer 补齐、只在成功后去重、失效 owner、取消传播且不关闭 owner、拒绝创建未知浏览器 context、禁止序列化 context。

显式启用的 `TestPrepaymentChromeRuntimeLocal` 使用真正 Chrome、临时 profile 和 loopback HTTP fixture。主页面 `127.0.0.1`、卡 iframe `localhost`，必须实际出现独立 iframe Target；不通过真实站点主机名或关闭隔离伪造这一点。

六次全新启动中，前三次分别验证普通卡字段、格式化卡号、分栏有效期的正常待确认结果；后三次分别验证最终 CVV 被清空、账单 input 引发 requestSubmit、独立 iframe input 引发资金确认 fetch 必须失败。每轮检查零付款控制激活/原生提交计数、外国 frame 未被填写、初始存储为空与最终 profile 删除；最后一种请求到达测试服务器即失败。账单触发的税额延迟更新也是实际异步浏览器行为。

```sh
KWMEMBERSHIP_REQUIRE_PREPAYMENT_RUNTIME_TESTS=1 \
KWMEMBERSHIP_CHROME_RUNTIME_PATH=/absolute/path/to/chrome \
go test -count=1 -timeout=240s -run '^TestPrepaymentChromeRuntimeLocal$' ./internal/checkout
```

Linux 需要非 root 与私有 DISPLAY；Windows 不要求 DISPLAY，使用普通安装的 Chrome 和独立临时 profile。未显式配置浏览器时该可选测试会跳过；设置 REQUIRE 后缺少浏览器会失败。测试不依赖真实 Session、真实卡或外部站点。是否在某一操作系统通过，以该次 CI/本机运行记录为准，不能用编译成功代替真实浏览器运行。

定位已知故障时可用 `-run '^TestPrepaymentChromeRuntimeLocal/fresh_profile_0$'` 只跑一个全新 profile；正式验收仍需全部六轮。窄诊断接收器只由这个 loopback 测试安装，输出固定阶段、数值错误码与固定分类；生产没有该接收器，也不会打开 Cookie、Session、请求体、原始 CDP 消息或 checkout URL 日志。

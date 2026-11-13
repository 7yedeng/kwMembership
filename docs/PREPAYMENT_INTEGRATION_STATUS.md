# Session → Plus 付款前整合状态

更新：2026-11-14（北京时间）。工作分支：`feature/modular-single-flow-20261114`。

## 本轮目标

用户已经自行验证 Session 登录，要求继续完成自动化到最终确认付款前，先验证 Plus，并让 Zovo 只承担可替换的基础卡台职责。本轮新增独立 `membership-prepare` CLI 和桌面窗口，把会话恢复、结账入口、报价审核和填表连接到同一个自有浏览器生命周期。最终付款继续关闭。

这是一条可以由用户本机运行的付款前验收入口，不宣称已在真实 ChatGPT 结账页完整跑通。受管 Browser 对 ChatGPT 的 host restrictions gate 尚未解除，因此本轮没有改用其他工具访问被拒绝的目标。

## 模块边界

| 模块 | 实现 | 输入、输出与边界 |
| --- | --- | --- |
| 会话恢复 | `internal/sessionrestore.Apply` / `UseChromeContext` | 只借用调用者已创建的 Chrome target；不另起进程、不关闭浏览器；输出 Cookie 和身份核验 |
| 单笔编排 | `internal/preparation` | 解析 stdin、绑定卡台配置与 cardId、组合账单和卡资料、生成脱敏报告 |
| 结账与页面 | `internal/checkout.PreparePrepayment` | 在同一 profile 进入 Plus，收集可见报价、填字段、再次核验；接口没有提交或通用点击方法 |
| 卡台 | `provider.CardPlatformRegistry` + `kind=zovocard` | 基础产品、余额、卡和交易 API；不使用 GPT 代充 API |
| 桌面窗口 | `tools/prepayment-desktop` | 原生独立输入窗口；只给 CLI 发送一次任务和取消命令；不保存 Session 或密钥 |
| 资金与对账 | 既有 processor / kwRedeem SQLite | 保留原持久化资金意图、许可和未知结果核对；没有新建业务数据库或队列 |

## 实际运行顺序

先离线校验输入，创建独立临时浏览器，然后恢复 Session 并严格回读 Cookie、核验目标身份。Plus 入口还会检查账号当前为免费状态；已有付费订阅或状态不明时停止。

进入结账后，先确认目标页面与套餐、币种，再通过基础卡接口取得同一卡的资料。账单资料先填写，因为税额可能依赖账单地址；等待页面稳定后审核小计、折扣、税和总额。审核通过才向卡字段传入完整卡材料。填写结束后再次核验报价和字段，要求最终付款控制已明确出现且可用。页面还需要中间推进、挑战验证、价格变化或字段回读失败时，返回明确失败代码。

达到付款前状态后发出脱敏 `ready` 事件，可观察最多五分钟；没有下一条确认支付指令。取消、超时或清理失败会撤销最终成功标志。卡台材料、Session、支付 URL 和原始网页错误不写日志。

## Plus 与 Go 的范围

共同核心可以复用：会话、临时 profile、卡台、账单、金额运算、字段回读与付款边界。当前明确支持的自动建单组合为 **Plus / PH / PHP**，依据已有源码的明确请求契约。它不等于“任意地区 Plus 已验收”。

Go 的真实 SKU、资格判断和结账契约尚未获得可靠现场证据，不把 Plus 的 `plan` 字符串改成 `go` 冒充完成。最早文档中的“已有 Plus”指已有入口与执行器基础，不代表真实页面适配和端到端验收都已完成。本轮补齐了衔接代码与可见 DOM 适配；真实页面覆盖仍待运行反馈。

## 卡台接口与已发现的协议问题

基础接口依据 [Zovo 官方文档固定提交](https://github.com/zovocard/zovo_card_cdk_auto/blob/19f41ef347cdb3cce52fde0f2b43e12f70dc6ebe/docs/zovocard-openapi-zh.md)，文档 blob 为 `7f0849ef2b82fd73bea1fdfba598431824df694c`。

卡台配置 key 与实现 kind 分离。同类卡台的多个账号不会混用资料。详情、余额与持卡信息来自一次卡详情响应；本地 cardId 与上游卡标识还需要通过列表证据核对。缺实际账单资料时仅允许显式补入同卡登记资料；不随机生成姓名或地址。

官方文档同时写明：幂等缓存只保存 2xx 响应，卡加款结果未确认也可能返回 HTTP 400。因此底层 Zovo 写请求只发一次，取消标准 HTTP transport 对可重建 POST body 的潜在自动重放，未知结果禁止进入旧 processor 的资金 replay。`FundingReplayWindow=0` 在旧代码中原本表示不设时间窗，不能用它表达禁止重试；新增补充策略显式禁止 Zovo 未知结果重放。

当前验收工具只查既有卡，资金动作数固定为零。基础开卡、加款方法已实现；旧生产库存仍会查询商户价格信号，所以还不能直接把旧 worker 的 SpaceX 类型替换成 Zovo 后就宣称整条生产链路可用。

## 页面证据与可选技术方向

当前实现读取可见的语义金额行，不给页面注入测试价格属性，也不从隐藏页面对象中搜集令牌。缺折扣、税额或税包含义时保持缺失；不会把未展示数据自动补成零。最终报价需要稳定读回；测试支持的 DOM 形状不代表所有线上页面。

[Stripe 官方 WebMCP 文档](https://docs.stripe.com/agentic-commerce/agents/webmcp) 已描述结账页可提供订单摘要和表单操作工具。文档要求在当前页面动态发现工具及 schema，状态变化后重新发现，缺工具时退回普通自动化。本轮没有将其编造成已接通的生产接口。

[Stripe Checkout Session 对象文档](https://docs.stripe.com/js/custom_checkout/session_object) 说明税计算可能需要账单或收货地址，这支持本轮先填实际账单、再审核税后金额的顺序。托管第三方页面是否向外暴露 SDK 对象仍需页面证据，不能假定存在通用全局变量。

本轮通过受管 Browser 独立观察了 Stripe 官方公开演示 `https://checkout.stripe.dev/`。外层演示打开成功；点击演示入口后，内嵌支付区未加载，控制台出现 `PassiveCaptchaError: captcha not mounted`。没有填写或提交演示支付，也没有规避验证码。因此此观察只能记录演示加载问题，不能用于声称真实填表已通过。

## 验收证据分层

1. 用户反馈：旧本机 Session 工具在两个自己的账号上登录成功。
2. 本轮本地单元/契约测试：会话借用、输入约束、卡绑定、未知资金结果、报价更新、字段回读和取消等。
3. GitHub Actions：完整 Chrome/Xvfb 的本地 fixture，三次新 profile 验证新的 Go 付款前编排；Cookie 写入与读回独立测试。严格要求相关 runtime 测试真正执行，缺环境不能跳过冒充通过。
4. 未完成的现场验收：真实 Plus 入口、当前真实表单、真实 Zovo 账户和同卡资料、真实最终报价、真实卡的可用性。没有真实付款或续费变更。

最新测试结果和源码提交以 [开发 PR](https://github.com/7yedeng/kwMembership/pull/2) 的 checks 和说明为准；源码存在、合成测试通过和真实链路验收是三种不同证据。

## 后续扩展顺序

先由用户在允许访问的本机用免费账号、既有卡和真实账单资料运行新工具，确认最终报价及字段。针对返回的固定缺失代码修正页面适配，不需要回传凭据。随后补足 Go 契约与卡台产品/库存规划，再将经过验收的节点接入既有 durable worker。实际提交、权益核对、交易清算及取消续费的实盘验收另行进行；单笔稳定后再扩展并发。

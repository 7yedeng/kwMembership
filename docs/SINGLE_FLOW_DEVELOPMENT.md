# 单笔链路开发记录与后续验收

更新：2026-11-14，北京时间。分支：`feature/modular-single-flow-20261114`。

后续迭代补充：[本机安装与空白页/粘贴诊断](SESSION_INSTALLATION_AND_DIAGNOSTICS.md)、[付款前三轮重新启动验收](PREPAYMENT_RESTART_ACCEPTANCE.md)、[付款及清算对账验收](PAYMENT_RECONCILIATION_ACCEPTANCE.md)。这些记录保留真实现场、合成浏览器和业务验收的区别；以下早期执行证据不替代最新 PR 的 CI 结果。

## 用户目标与当前授权

目标是用户提供 Session，配置一个可更换的卡台后，自动执行一笔完整会员充值，再扩展排队和并发。优先 Go 套餐。会话恢复可由用户自己输入材料验收；浏览器显示必须是可拆卸的观察能力。

真实付款仍暂停。本轮不创建实际订单、不操作真实卡、不改变账号续费。已有 ChatGPT host restrictions gate 未获得正式解除；没有用 CLI、其他浏览器或原始请求绕过。Browser 已连接，但 `chrome-extension://` 编辑器的读取又被 URL 协议策略明确拒绝。因此不宣称取得已安装油猴副本。

## 模块与依赖

| 节点 | 所有者/接口 | 本轮结果 | 真实验收状态 |
| --- | --- | --- | --- |
| Session 输入与恢复 | sessionrestore.Browser / Factory；membership-session | 独立核心、CLI、桌面输入界面；Cookie回读、身份匹配、取消和清理 | 用户确认本机两个账号Session测试成功 |
| 浏览器显示 | sessionrestore.Options.ShowBrowser / HoldOpen | 可选观察，不属于卡台或业务状态机 | 用户已验收本机可见模式；服务器模式待验收 |
| 结账建单 | checkout.Executor；既有preflight | 保留Plus建单、Go手工观察范围，不猜测Go SKU | Go自动建单未完成 |
| 金额确认 | checkout.ReviewQuote；membership-review | 整数金额、税/折扣/总额核验，受控确认摘要 | 目前输入证据来自本地，未自动采集实时报价 |
| 卡台接入 | provider.CardPlatformRegistry / CardPlatform | 两内置适配器，统一构造接口，配置key隔离修复 | 未调用真实开卡/充值 |
| 自动填表与推进 | 既有Go/Python浏览器执行器 | 保留已有字段回读与资金动作边界；CI真实Chromium运行合成页面测试通过 | 当前聊天云容器被AF_UNIX限制阻断；真实支付页面未验收 |
| 最终付款 | Go ActionGuard / durable permit | 沿用资金许可与交易基线；保持关闭 | 未付款 |
| 结果核对 | processor reconciliation | 沿用交易与目标权益双证据，未知不盲重试 | 未实盘 |
| 续费操作 | AccountServices / Renewal | 保留接口与读回确认；第三方依赖尚待替换 | 未修改真实订阅 |

账号验收、金额审核和卡台构造均为独立模块；CLI、桌面界面共用核心，不互相复制身份逻辑。生产 worker 仍使用既有 kwRedeem SQLite、账号锁和业务记录，没有第二个数据库、队列或投影服务。

## 已修复的实际问题

1. 油猴页面若存在 `module.exports`，旧版提前退出导致入口不出现。0.1.1区分Node测试环境与浏览器，启动即展示面板；缺少Cookie能力仍可诊断。
2. 卡台数据库配置key与适配器kind混淆：同一种卡台两套账号配置可能返回同一个Key。新包装器Key返回配置标识，Kind返回实现类型；库存/资金绑定用Key，错误分类用Kind。
3. 独立审查发现新登录适配器第一次chromedp.Run使用短生命周期context，会在初始化返回后关闭浏览器。已改用长生命周期启动并补回归。
4. Cookie精确回读补齐opaque分区判断；GUI补齐退出码与全部成功证据，避免把观察后取消误报正常完成。

## 开源与术语线索的使用方式

| 来源 | 可借鉴内容 | 不从名称推断的内容 |
| --- | --- | --- |
| [chromedp/chromedp](https://github.com/chromedp/chromedp) 与 [cookie example](https://github.com/chromedp/examples/blob/main/cookie/main.go) | 浏览器生命周期、Cookie API、Go接口 | 示例成功不证明目标网站接受会话 |
| [microsoft/playwright](https://github.com/microsoft/playwright) 与 [BrowserContext](https://playwright.dev/python/docs/api/class-browsercontext) | 独立context、iframe操作、可见/隐藏运行 | 官方自动化库不提供第三方商户的私有充值协议 |
| [Cookie-Editor](https://github.com/Moustachauve/cookie-editor) | 扩展Cookie导入/导出与权限边界 | 不能把accessToken变成缺失的会话token |
| Zovo 4.3.21 实包 | 会话恢复、getCheckoutUrl、gptPayJump、fillCheckoutTab 的职责划分 | 包内自动填表不提交最终付款；无Go分支 |
| README 的 GptPaymentProtocol / SessionParseService / card-key-portal-java | 指向更接近服务端执行层的候选工程 | 未找到可核验公开源码，不能据类名伪造实现或声称复刻完成 |
| Porter 9月18日原帖 | 先浏览器自动化、后协议路线的作者自述 | 作者明确未公开具体实现；不能推断其卡台或实际请求细节 |

新推测先记入候选项，再用官方文档/实际源码/允许环境中的观察验证。不得把推测的URL、SKU、页面属性或测试fixture写成线上事实。

## 本轮执行证据

[GitHub Actions 第10次运行](https://github.com/7yedeng/kwMembership/actions/runs/43474176604) 对代码提交 `ac533bd2a267e0a20574f9cf4e64c20325a457f4` 的三个作业全部通过：

- Go：16个包通过；油猴38项测试通过，零跳过；桌面控制器6项测试通过。
- Python/Chromium：59项测试通过，零跳过。页面使用本地合成fixture，不是真实ChatGPT或Stripe付款页。
- 完整Chrome/Xvfb：既有生产浏览器启动测试和新增 `TestSessionChromeCookieRuntimeLocal` 均明确pass；后者只停留在about:blank，验证合成Cookie写入/精确回读、浏览器保活和临时profile删除。

上述为早期验收。随后提交 `eb4309f8c4173a982c7499d29f290ab8a94c7047` 的 [CI 第12次运行](https://github.com/7yedeng/kwMembership/actions/runs/43483012192) 三个作业全部通过：16个Go包、41项油猴测试、6项桌面控制器测试、64项Python测试（零跳过）、严格三轮全新浏览器生命周期、7项真实Chromium输入隔离检查，以及完整Chrome/Xvfb启动和本地Cookie回读。新增对账修复及其证据见上方专题文档。

当前聊天云容器仍存在AF_UNIX启动限制；CI通过不代表该容器限制已解除，也不代表真实Go报价或付款已验证。真实登录的两个账号验收来自用户本机反馈。已安装会话EXE仍来自 `36c1af26`，会话核心未在本轮改动；启动脚本与油猴0.1.2单独更新，不能把旧发行包称作本轮全新构建。

## 下一步的单笔验收顺序

1. 已完成用户侧本机会话恢复验收：用户反馈两个账号Session测试成功。该结论不替代后续服务器与真实结账验收，无需再次发送凭据。
2. 在正式允许访问的环境，观察目标Go账号的真实套餐、结账入口、字段与最终总额，建立经过审阅的页面适配器。缺少字段时保持未知。
3. 只接一家卡台，核对产品、卡资料、可用余额和交易查询；使用实际登记账单资料。
4. 先验收到最终付款前，准确读回表单和金额；不得把长链、短链或填卡返回ok当成扣款证明。
5. 付款范围明确后才进行一笔受控实测。付款提交结果未知时对账，禁止直接重试。
6. 核对新卡交易、目标账号权益；需要取消续费时执行并读回状态。
7. 单笔真实闭环稳定后，再扩展队列、并发、每任务固定网络路由和人工接管。

## 持久化与运行方式

源码、接口、测试、开发记录均随Git分支保存。发行包由 `scripts/build-toolkit.py` 从同一源码构建，包含Windows/Linux CLI、同一核心的桌面界面、油猴完整脚本与依赖许可证。

本记录不意味着聊天结束后已有定时后台开发任务。后续会话应从本分支、验收矩阵和实际测试结果继续，不重新猜测已经确认的事实。

# 登录、结账与填表如何共用浏览器

## 已知实现

本机 `membership-session` 使用 Go 的 chromedp/cdproto，通过 CDP 驱动所选择的 Chrome 可执行文件。桌面 Tkinter 界面只把用户输入送入 Go CLI；不控制网页、不保存可长期复用的登录环境。测试完成后，`sessionrestore.Restore` 会关闭并清理本次独立 profile。

用户已反馈两个本人账号的 Session 恢复成功。Playwright 也已在本机 Chrome 的纯本地合成输入页面上运行成功；它是另一种可用的浏览器驱动库。这两个结果均不是受管 Browser 工具的站点授权，也不改变该工具已返回的 host restrictions gate。没有用独立驱动器替代被拒通道访问真实 ChatGPT。

## 当前生产代码已经具备的一次任务生命周期

`internal/checkout/chrome.go` 中 `ChromeExecutor.Execute` 创建一个独立 allocator 与 browser context，设置 Cookie，并把该 context 传给后续方法；只有整个 Execute 返回后才关闭它。现有 `ModeSessionCheckout` 经过 `executeSessionCheckout`、`verifySessionIdentity`、`openPlusCheckout`/`executeUpgrade` 与 `executeCheckout`，在同一次任务中继续使用该浏览器。

因此后续应扩展这一任务执行入口，将已验收的 Session 解析、精确 Cookie 回读与身份约束整理成共享契约。**不应先运行会自动清理的测试 CLI，再尝试接管已经退出的 Chrome。** 当前生产接口主要绑定预期邮箱；测试工具输入含 user.id 时还同时绑定用户 ID，这项更严格约束仍需在后续生产衔接中保持一致。

这段现有逻辑主要服务 Plus/升级。`validateRequest` 和套餐契约不允许把 Go 当成 Plus；Go 自动建单参数及真实页面适配仍未验证。函数存在不表示该套餐已经完整可用。

## 推荐保留的模块边界

| 模块 | 输入与输出 | 是否应持有浏览器 |
| --- | --- | --- |
| Go 任务执行器 | 任务标识、目标身份、套餐和价格契约；输出阶段及结果 | 是，拥有本任务生命周期 |
| Session 核心 | 已有会话材料、目标身份；输出规范 Cookie 和登录证据 | 通过注入的浏览器接口使用，不连接个人浏览器 |
| 结账页面适配器 | 已登录的本任务浏览器、套餐契约；输出实际结账事实 | 复用任务上下文，不另起一个不相关的登录环境 |
| 卡台适配器 | 独立配置 key、卡产品及交易查询；输出卡/账单资料与交易事实 | 否，不接触 Session 或页面 |
| 填表与报价审核 | 结账事实、卡资料、金额预算；输出字段精确回读和审核摘要 | 由任务调用，在付款动作之前完成 |
| 动作许可与对账 | 价格、同卡 baseline、permit、attempt；输出提交及后续证据 | 许可由数据库持有；对账不依赖原浏览器继续存活 |

项目现有 Go worker、共享 kwRedeem SQLite、账号锁、资金许可和交易快照保持唯一所有权。页面驱动可替换，不创建另一份任务队列或让桌面 GUI 成为生产调度器。

## chromedp 与 Playwright 的选择

沿用现有 Go/chromedp 执行器，可以复用同一 Go 主流程的取消、超时、任务绑定与许可；这也是目前项目生产约束。Playwright 适合作为已存在的 Python 页面适配/测试实现，替换时仍应服从相同 Executor/ActionGuard 契约。

不建议一笔任务由两个驱动器同时操作同一页面。另一实现要替换时，在任务开始前选择一个驱动，由它负责整个任务浏览器；跨步骤传递受控任务上下文，不暴露通用远程调试端口、不复制常用浏览器 profile、不把 Cookie 明文写入队列或日志。既有持久化敏感材料继续使用共享加密契约。

可见窗口是验收方式。服务器采用项目既有的完整 Chrome + 私有 Xvfb 运行方式；用户不需要坐在窗口前，程序依然具有真实 DOM、iframe、JavaScript 和正常页面状态。用户本机可见模式通过，不能直接推出服务器/严格 headless 已通过。

## 自动化动作的粒度

稳定的自动化应围绕字段、状态和证据，而非屏幕固定坐标：

1. 按 label、role、name 或已验证的结构定位字段/按钮。
2. 在正确的 iframe 内填写，精确回读预期值，检查校验状态及重渲染。
3. 从实际页面或经过验证的接口取得套餐、币种、小计、税额与最终总额；未知时停止。
4. 付款动作之前取得 durable permit 与同卡交易 baseline；未知提交结果转为对账，不通过重新点击解决。

可将有可靠契约的查询、卡台和对账操作放到 API 层；需要页面 JavaScript、支付 iframe 或交互验证的步骤保留浏览器。能调用浏览器中的接口并不意味着已经掌握无浏览器的完整付款协议，私有请求、SKU 或返回字段必须有实际证据。这里未新增或猜测 Go 的私有结账接口。

官方资料：[chromedp](https://github.com/chromedp/chromedp)、[Playwright BrowserContext](https://playwright.dev/python/docs/api/class-browsercontext)、[Playwright Locators](https://playwright.dev/python/docs/locators)。

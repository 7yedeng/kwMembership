# 账号、隔离浏览器与真实付款的分阶段验收

日期：2026-11-13。本文记录当前实现边界和下一次有授权环境中的测试顺序。没有真实 Session、卡台凭据、开卡结果、商户扣款或会员到账记录。

## 1. `/api/auth/session` 可以验证到哪一步

用户指定的入口是 `https://chatgpt.com/api/auth/session`。它属于当前浏览器登录上下文，返回的数据不能先假定为完整会员账单契约：身份、Session 有效性、免费/付费档位、自动续费、欠费、币种和到期时间需要分别有证据。

本次受管浏览器在导航前被 host restrictions gate 拒绝访问 `https://chatgpt.com`。没有读取到响应，也没有将另一个浏览器、CDP、cookies 或 shell 请求作为绕过方式。因此本轮不声称已查到用户的账号状态。

现有 Session 执行路径需要能构造浏览器 cookie 的 `sessionToken`。只有 `accessToken` 的对象不等价于这个 cookie；不能通过改字段名、使用第三方转换器或伪造到期时间补齐。新 CLI 的 `--inspect-session` 可以在本地检查已知字段类型，不会打印 token、邮箱、用户 ID、未知字段名或原始 JSON；这仍然不是远程登录验证。

免费账号的优先测试方式是用户在独立浏览器窗口直接登录。账号密码、验证码和完整 Session 不应作为聊天消息、仓库配置或 CI 输入。CLI 的输入契约和可用模式见 [STANDALONE_CONTRACT.md](STANDALONE_CONTRACT.md)。

## 2. 如何隔离免费测试账号

普通新标签页仍可能共享同一浏览器 profile 的 cookie；“新开一个 tab”不能作为账号隔离证据。当前 Browser 管理能力没有可调用的 incognito 创建接口，不能用普通窗口冒充无痕窗口。

仓库中 Go 的每次 `Execute` 创建独立 ExecAllocator，未接受现有用户 profile 路径，默认使用新的临时 profile；headless 分支另带 incognito 标志。headed 诊断模式依赖已经配置好的私有显示环境，例如本机 Xvfb；它仍有独立临时 profile，不依赖用户日常 Chrome 登录态。进程结束后由 allocator 清理临时 profile，崩溃残留的运维处理尚需在部署环境验证。

`--interactive-preflight` 用于免费账号登录与结账页检查：仅接收预期身份和价格契约元数据，不接收 Session 或卡资料。浏览器打开后由用户直接登录并自行进入结账页；自动代码核对实际账号与页面事实。它是诊断入口，不是生产履约中的人工步骤，也不能生成付款资格、资金预留或 canary 授权。

真正运行前必须先有该执行环境对目标站点的访问能力。另一环境的离线测试通过不代表当前浏览器访问限制已解除。

## 3. 取消续费的受支持入口

官方帮助页目前说明：

| 购买渠道 | 用户操作路径 | 自动执行器应如何处理 |
| --- | --- | --- |
| ChatGPT 网页个人订阅 | 登录 → 账号菜单 → Settings → Billing → Cancel plan → Cancel | 先确认当前身份、购买渠道和实际页面，完成取消后重新读取自动续费状态。 |
| Apple App Store | 在对应 Apple 订阅管理入口取消 | 返回渠道不受此网页适配器支持，不能冒充已取消。 |
| Google Play | 在对应 Google Play 订阅管理入口取消 | 返回渠道不受此网页适配器支持，不能冒充已取消。 |

来源：[OpenAI：如何取消 ChatGPT 订阅](https://help.openai.com/en/articles/7232927-canceling-your-chatgpt-subscription)、[ChatGPT 与 API 的账单管理](https://help.openai.com/en/articles/9039756-managing-billing-for-chatgpt-and-the-api-platform)。查阅日期为上文日期；最终 selector 和状态证据须以授权账号当前页面验证。

这些资料只证明受支持的操作入口，没有提供可直接实现的、公开且稳定的取消订阅 API 契约。本分支没有凭猜测写一个私有 cancel URL。`AccountService.CancelRenewal` 的真实适配器仍待实现；其工作流已要求操作后再观察，并且 `auto_renew=false` 观察不得早于取消动作完成时间。

“取消续费”不是立即删除账号或自动退款，也不意味着当前订阅立刻失效。免费测试账号没有现存付费订阅时，应该记录没有可取消对象，而不是制造一次成功取消事件。本次只研究路径，没有取消用户当前订阅。

## 4. Chromium 的 IPC 启动故障

IPC 指进程间通信。本次故障发生在完整 Chromium 的 ProcessSingleton 初始化，在任何目标网页加载之前：

```text
FATAL:chrome/browser/process_singleton_posix.cc:297
socket() failed: Operation not permitted (1)
```

普通执行权限下的最小检查观察到：

| 检查 | 当前结果 |
| --- | --- |
| `socket(AF_UNIX, SOCK_STREAM)` | `EPERM` |
| `socket(AF_UNIX, SOCK_DGRAM)` | `EPERM` |
| `AF_UNIX socketpair` | 可用 |
| AF_INET 回环 TCP 监听与连接 | 可用 |
| `/proc/self/status` | `NoNewPrivs=1`、`Seccomp=2`、一个过滤器 |

因此不能概括成“所有 IPC 都关闭”或“所有 localhost 连接都失败”。Chromium 官方 [ProcessSingleton 源码](https://chromium.googlesource.com/chromium/src/+/main/chrome/browser/process_singleton_posix.cc) 确实调用 Unix domain socket，并在失败时终止。这里可以定位到被拒绝的系统调用，但没有取得宿主完整过滤策略，不能只根据 `Seccomp=2` 就断言具体规则由哪一个安全组件设置。

这不是 Chrome 设置页中的开关。关闭 Chromium 自身 sandbox 不会移除外层执行环境的约束；此前申请扩大执行权限也被自动审批拒绝。正确的处理位置是有管理权限的宿主运行策略，或使用允许该测试能力的常规执行环境；本轮没有修改宿主保护设置。[Linux seccomp 官方文档](https://docs.kernel.org/userspace-api/seccomp_filter.html) 说明过滤机制控制系统调用并随子进程继承。

本轮还尝试正常安装官方 Chromium headless shell，但下载返回 0 MiB 的无效 ZIP，因此没有获得可启动文件。这是另一个安装问题，不应写成 headless shell 已复现同样 IPC 故障。

仓库新增 [离线 CI](../.github/workflows/offline-validation.yml)，在 GitHub runner 上安装与 requirements 固定版本匹配的 Playwright 浏览器，用纯合成 HTML 测试真实 DOM。所有页面请求由 route `fulfill` 或 `abort` 处理，不读取真实账号、不访问支付页面、不使用 repo secrets。CI 强制浏览器测试执行，任何 skip 都失败。实际运行结果以 [VALIDATION.md](VALIDATION.md) 及对应 Actions 日志为准。

## 5. 按用户要求的顺序验证

| 阶段 | 执行内容 | 通过证据 | 当前状态 |
| --- | --- | --- | --- |
| 本地输入 | 本地 Session 结构或无 Session 登录元数据检查 | 严格输入契约；结果不冒充远程认证；无敏感输出 | 已有实现与离线测试 |
| 离线浏览器 | 原生输入、受控回写、跨 iframe、Line2、缺失和 invalid 状态 | 真实浏览器 fixture 必须执行；不能以 skip 通过 CI | 代码已加入，实际 CI 结果另记 |
| 免费账号登录 | 独立临时 profile，用户直接登录 | 实际账号匹配预期身份，不能读取日常付费 profile | 当前访问能力受阻，尚未执行 |
| 结账前检查 | 套餐、币种、地区、显示金额、按钮与字段结构 | 现有适配器支持真实页面；金额在明确区间内 | 尚未真实执行 |
| 选择/开卡 | 查询实际产品、开户费、最低注资、可花余额，确定一张测试卡 | 产品与费用明确；开卡或充值的资金意图有稳定幂等键 | 未取得该账号卡台资料，未开卡 |
| 资料与填表 | 同一张卡的姓名、完整地址、卡材料，回读页面接受值 | 平台 key + 卡 ID 绑定，无冲突，无缺失 required 字段 | 解析、绑定、填表代码已有；真实页待验 |
| 一次付款 | 明确订单/账号/卡/金额上限，获得持久化 Permit 后单次点击 | 交易快照与许可一致，点击后未知结果不重提 | 未执行 |
| 结果与后续 | 查卡交易、查新会员状态、按目标取消自动续费并再观察 | 成功扣款和权益可关联；取消有新状态证据 | 真实账号适配器仍缺，未执行 |

自动 Session 代码的价格上下文仍是上游 `Plus / PH / PHP` 契约；交互无付款预检另外允许 `Go / PH / PHP` 并严格隔离目标。它们没有被本轮当作实际在线售价，也不等于可任意改变账号地区、资格或税务信息。真实 Go 页面和自动 Go SKU 尚未核验，详见 [GO_PLAN_PREFLIGHT.md](GO_PLAN_PREFLIGHT.md)。若真实页面不同，先停止并基于实际事实适配，不通过注入 `data-kw-*` 标记、伪造金额或换 billing country 让测试强行通过。

账单资料应从卡详情或该卡明确配置中取得，地址生成器只适合合成测试数据。开卡网页会预填随机姓名、API 要求显式姓名、卡地址字段可能全空，是三个不同事实，详见 [CARD_BILLING_RESEARCH.md](CARD_BILLING_RESEARCH.md)。

# Zovo GPT 插件 4.3.21：实包静态审查

核验日期：2026-11-13。依据是用户桌面的实际 ZIP，以及此前读取的 [Zovo 官方工具页](https://zovocard.com/gpt-tool)。本次只读取和分析文件，没有安装、执行扩展、导入真实 Session、访问账号或付款。

## 1. 结论与证据范围

这个包提供登录状态恢复、结账链接和付款页自动填表的客户端实现。它不是完整独立充值引擎：查询订阅、账单与取消续费依赖 Zovo 后端；没有 Go 套餐分支；自动填表路径不提交最终付款。用户所说的“导入后刷新网页”在 `injectCookies()` 中有明确实现，但静态代码不能证明当前账号已登录，也不改变本会话的 host gate。

| 项目 | 核验结果 |
| --- | --- |
| 文件 | `gpt-helper-extension-4.3.21.zip`，用户 Windows 桌面 |
| 大小 | 58,297 字节 |
| SHA-256 | `08bfafa10672abff629838ba89958a9d15a9021c9ed04a061ec5ccd7bef34356` |
| manifest | MV3；名称 `GPT 小助手 · 太空虛擬卡`；版本 `4.3.21` |
| 包内条目 | 11 项；解包检查无路径越界或符号链接 |
| 可读性 | `credential-utils.js` 为可读源码；`background.js`、`sidepanel.js` 为单行压缩混淆构建产物 |
| 工程完整性 | 没有服务端源码；README 引用的测试文件与 `build.py` 未包含；没有许可证文件 |

来源链是官方页面入口、用户指定的桌面文件、复制前后相同哈希与 manifest 版本一致；没有取得官方签名或公布的校验和。README 部分内容仍写 4.3.20，实际版本以 manifest 为准。本仓库只保存原创分析，不重新分发 ZIP 或复制其构建代码。

包内文件为 `manifest.json`、`credential-utils.js`、`background.js`、`sidepanel.html`、`sidepanel.css`、`sidepanel.js`、`README.md`，以及四个尺寸的图标。以下引用单行 JS 的函数名或消息分支，不虚构格式化后的源码行号。

## 2. 登录恢复、账号绑定与隔离

| 证据位置 | 实际行为 | 对自有实现的含义 |
| --- | --- | --- |
| `background.js`：`injectCookies` | 清理旧会话 Cookie、写入、回读、寻找对应模式标签、刷新，再调用 `verifyChatGPTLogin` | 用户描述的导入后刷新有实现依据；写入、刷新、登录必须分别记结果 |
| `background.js`：`verifyChatGPTLogin` | 以返回用户 email 或 access token 判断成功，没有比对预期账号 | “有一个登录账号”不等于“当前任务的指定账号” |
| `credential-utils.js`：`classifyGptCredential` | `canPay`、`canQuery` 来自本地凭据格式与字段分类 | 不是线上鉴权、余额、套餐资格或付款许可 |
| `manifest.json`：`incognito` | 设置为 `spanning`；代码区分普通/无痕 Cookie store | 不能当作每个订单独立容器 |
| `background.js`：`chatgptTabs` | 找不到指定普通/无痕模式标签时回退到所有匹配标签 | 账号选择可能越过调用者指定的模式边界 |
| `background.js`：`resolvePaymentAccessToken` | 默认优先读取浏览器当前会话；未见与输入目标账号的强制绑定比较 | 不宜直接用于无人值守的多账号并发 |
| `sidepanel.js`：`withLiveSession` | 查询/取消前可用实时页面会话更新输入 | 不能把输入框保留的账号视为稳定任务身份 |

此外，Cookie 回读的 `verified` 主要检查有非空值，没有逐一验证值与输入完全一致。上述都是静态检查结果，没有通过真实账号复现串号或登录成功。

服务器实现继续采用独立 context/profile、每账号锁和明确目标身份。任何实时会话替换都应重新核对账号，不能在找不到目标时退回任意活跃标签。现有项目约束仍见 [AGENTS.md](../AGENTS.md) 与 [服务器隔离审计](SERVER_ISOLATION_AND_IPC.md)。

## 3. 哪些能力仍在 Zovo 后端

| 用户入口 | 客户端代码 | 实际依赖 |
| --- | --- | --- |
| 查询订阅 | `gptCheck`、`gptApiWithSessionRefresh`、`gptApi` | POST 至 Zovo `/gpt/check` |
| 查询账单 | `sidepanel.js` 的 `billBtn`、`renderBill` | 同一 `gptCheck` 响应中的 `invoices` |
| 取消续费 | `gptCancel` | 向 Zovo `/gpt/cancel-renewal` 提交凭据；界面显示服务端结果后再查询订阅 |
| 生成结账链接 | `getCheckoutUrl` 等 | 包含页面调用、扩展请求与 Zovo `/gpt/checkout` 回退；不是单一后端调用 |
| 卡资料 | `recentCards`、`searchCards`、`cardGet`、`cardFillStr` | 查询已有卡及详情；包内未见开卡与给卡充值实现 |

Zovo API 基址为 `https://zovocard.com/api/v1`。查询/取消链路会将包含 Session 或 token 的 `token_input` 发往 Zovo，所以不能把这个插件描述为“凭据全程仅在本机处理”。这是产品调用路径，本次没有审查服务端如何保存或使用数据。

`login` / `loginMfa` / `loginWithToken` 将 Zovo 平台认证资料保存在 `chrome.storage.local`；记住密码使用固定字符串参与的 XOR 与 Base64 可逆混淆，并非外部密钥保护的加密。在检查的存储写入点中，未见将输入的 ChatGPT Session 另行直接写入 `chrome.storage.local`，但它仍会进入浏览器 Cookie、扩展内存/界面与上述平台请求。自有执行器不能照搬侧边栏应用的凭据缓存方式。

manifest 允许的站点包括 `chatgpt.com`、`chat.openai.com`、`zovocard.com`、`pay.openai.com`、`js.stripe.com`、`usaddressgen.com`。声明 host permission 仅说明扩展获准申请这些访问范围，不证明每个域名都实际收到请求。本次没有发送这些请求。

## 4. 自动填表与最终付款的边界

`gptPayJump` 获取链接、打开支付页，再异步获取地址并调用 `fillCheckoutTab`。它可以在填表结束前返回 `ok` / `willFill`，错误处理也不足以形成可靠的任务结果。

`fillCheckoutTab` 跨 frame 重试 `_fillCheckout`。检查的成功标记主要基于字段非空，终止条件不要求所有卡字段完整，也没有完整的预期值相等与页面 validity 校验。已经有值的部分文本字段不会覆盖。因此“返回成功”不能等同于资料准确、填表完成或付款成功。

这条流程没有最终付款按钮点击或表单提交。`sidepanel.html` 的付款界面明确说明自动填卡后由用户核对并付款。包内未见完整支付状态机、幂等任务、未知扣款结果对账、3DS 处理与权益确认闭环。

本项目仍由 Go 控制任务状态，继续使用已有的资金许可、卡交易快照、字段回读和未知结果对账要求；不把扩展返回 `ok` 当作放行依据。

## 5. Go、地区与账单地址的具体缺口

**没有 Go 实现。** `sidepanel.html` 套餐选项只有 Plus、Pro、Pro Lite、Team；`buildCheckoutPayload` 同样没有 Go 分支，未知套餐默认落到 Plus。埃及是国家选项，不是 Go 支持的证据；此包也不能证明 EGP 193 或其他实时价格。现阶段 Go 范围仍以 [非付款预检契约](GO_PLAN_PREFLIGHT.md) 为准。

**地区映射存在不一致。** UI 含瑞典，但 `buildCheckoutPayload` 的国家币种表只有 PH、US、JP、IN、KR、EG，未识别国家回落 PH。Zovo 结账回退还检查返回的 `egress_country` 为 PH。这只能说明该版本的客户端逻辑，不能证明任意地区已实际成功。

**地址不是来自发卡详情。** `fetchUsAddress` / `_readUsAddress` 从 `usaddressgen.com` 读取美国地址，`genName` 随机组合姓名；卡详情填表路径主要取卡号、CVV、到期日。不能把生成资料当作发卡行登记的持卡人或账单地址。自有项目继续保留卡平台配置 key + 卡 ID 绑定的实际资料，缺失和冲突显式处理。

## 6. 本次完成与剩余工作

已完成实际 ZIP 的取得、哈希与 manifest 核验，以及登录恢复、隔离、后端边界、结账填表和 Go 缺口的静态分析。没有将构建包当作完整开源工程，也没有执行其脚本。

浏览器 host gate 的正式解除仍无已验证结果，见 [站点权限核验](EXTENSIONS_AND_HOST_ACCESS.md)。真实账号在线验证、Go 报价、付款、开卡/充值、取消续费和服务端适配器仍待完成；本次文档更新不改变生产或付款开关。

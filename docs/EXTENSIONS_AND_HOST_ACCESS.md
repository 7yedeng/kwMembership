# 浏览器扩展、Cookie 导入与站点权限核验

核验日期：2026-11-13（Asia/Shanghai）。本轮范围是公开源码研究、官方插件入口检查和源码 Fork；没有导入真实 Session、验证真实账号登录或执行付款。

## 1. 当前控制工具与限制边界

当前已连接的本机浏览器由 ChatGPT Browser 工具通过 Chrome 扩展控制；工具接口为 `browser.js`。它与共享云浏览器、云命令执行器中的 Chromium、项目将来部署的 Go/Playwright 执行器是不同的运行入口。

| 层 | 决定的问题 | 当前证据 |
| --- | --- | --- |
| Chromium 运行环境 | 进程能否启动、IPC 与 sandbox 是否可用 | 见[运行验收](BROWSER_RUNTIME_ACCEPTANCE.md)；另一云 runner 已通过完整 Chrome + Xvfb 的 Go 启动测试 |
| Browser 工具站点权限 | 智能体能否观察或操作指定 host | 之前账号访问被 `host restrictions gate` 拒绝；本轮没有证明该限制已经改变 |
| 浏览器登录状态 | 网站是否认可 cookie、会话和账号身份 | 真实 Session 没有在线验证；Cookie 写入不等于服务器认可登录 |
| 支付执行资格 | 是否可进入有资金影响的动作 | 本轮用户明确要求不付款 |

因此，修复 IPC、换浏览器进程或写入登录 Cookie，都不是站点权限已经放行的证据。

### 官方支持的排查入口

[OpenAI 浏览器扩展文档](https://learn.chatgpt.com/docs/chrome-extension) 当前列出的入口是桌面应用 `Settings → Computer Use → 对应浏览器的 Manage`，管理站点允许与阻止列表。[应用设置文档](https://learn.chatgpt.com/docs/app/settings) 也说明 `Browser → Agent permissions` 下的浏览、下载、上传控制；界面名称因产品入口或版本不同可能有差异。应核对目标 host 的配置，不必为此放开所有网站。

官方一般连接排查包括更新应用与扩展、核对浏览器 profile、重启和新建对话。这些步骤用于排除配置或连接问题，**不能保证解除某个特定的 host gate**。组织策略或产品层限制需要对应管理方处理。当前没有可调用的工具来修改或读取这次拒绝所用的完整策略。

### 找到同类公开报告，但不是已确认修复

已通过 GitHub API 核对 [openai/codex #59803](https://github.com/openai/codex/issues/59803)：截至本次核验仍为 open。报告描述 Windows 上能列出已登录 ChatGPT 标签页，但观察内容时出现相同的 host restrictions gate 拒绝。

该 issue 的[用户补充](https://github.com/openai/codex/issues/59803#issuecomment-6726161254) 报告，普通站点权限之外，扩展还有针对 ChatGPT 页面的访问策略检查。另一[用户复现](https://github.com/openai/codex/issues/59803#issuecomment-6736818653) 称设为 Always allow、重启和新建对话后仍失败。

这些是其他用户的第一手报告，不是维护者对本会话的诊断，也没有证明本账号收到的实际动态配置。当前不能断言“改允许列表一定能解决”，也不能给出已经验证有效的关闭开关。若常规设置已经允许，可通过应用 `/feedback` 附上错误、应用和扩展版本及对话标识，引用此 issue；不需要附上 Session 或 Cookie。

## 2. ZovoCard 官方页面确实提供浏览器插件

本轮通过已连接的本机浏览器读取了 [ZovoCard 工具页](https://zovocard.com/gpt-tool)，进入“浏览器插件”选项。官方页面显示：

| 插件 | 页面版本 | 官方页面描述 |
| --- | --- | --- |
| GPT 浏览器插件 | v4.3.21 | ChatGPT 账号注入、订阅查询、账单与支付升级工具 |
| Claude 浏览器插件 | v1.3.2 | Claude 登录、订阅结账和付款资料辅助工具 |

两个项目均有“下载插件(.zip)”按钮。版本是页面当时的显示值；尚未通过实际 ZIP 的 manifest 独立确认。

同页“GPT账号管理工具在线”明确提供：

- 完整 Session 转为 Cookie-Editor 格式的 Cookie；
- 查询订阅与取消续费；
- 生成支付链接；
- 本机页面取链脚本。

这里只读取了功能说明，未提交 Session，未点击查询、取消续费、取链或付款。界面说明只能证明产品提供这些入口，不能证明任意账号的兼容性或实际成功率。

### ZIP 下载尚未完成核验

GPT 插件的下载按钮已尝试，但 Browser 的下载事件等待超时，未返回可读取的文件路径。随后没有在当前用户的标准/系统 Downloads 目录发现本次相关 ZIP；这不能排除浏览器另设下载目录。

查看 `chrome://downloads/` 被 Browser URL policy 明确拒绝，因为此工具仅允许 HTTP/HTTPS 协议；没有再通过其他控制入口读取相同的被拒绝浏览器页面。独立的公开 HTTP GET `/gpt-tool` 则得到 Cloudflare HTTP 403 / error 1010，未取得页面资源或包地址。

因此本次**尚未读到 Zovo 官方插件包的 manifest、background、popup、content script**。不能把其前端可读性、外部请求目的地、本地处理程度或许可证状态写成已审计事实。需要实际下载的 ZIP 或明确的本机 ZIP 文件路径，才能继续离线拆包。

### 不能用同名扩展代替

公开搜索还出现了标注“Built by Zovo”的 cookie 管理项目，但其主体指向 `zovo.one`。没有证据表明它与 `zovocard.com` 为同一项目，本次未将其作为 ZovoCard 官方插件。

## 3. Zovo 官方公开仓库里已有可分析的浏览器脚本

官方帮助中心的[自托管教程](https://help.zovocard.com/zhcn/help/zc-reseller-illustrated/) 指向 [zovocard/zovo_card_cdk_auto](https://github.com/zovocard/zovo_card_cdk_auto)。已有用户 Fork 为 [7yedeng/zovo_card_cdk_auto](https://github.com/7yedeng/zovo_card_cdk_auto)。

核对的上游提交为 `19f41ef347cdb3cce52fde0f2b43e12f70dc6ebe`；本地完整 checkout 工作树干净。它包含以下公开脚本，但不能把这些脚本当作上述 ZIP 插件的源码：

| 路径 | 静态代码证据 |
| --- | --- |
| `automation/lib/session.js` | Session 输入解析与构造浏览器 Cookie 数据的函数 |
| `automation/fill-checkout.js` | 创建浏览器上下文、加载登录材料、导航和跨 frame 填表；另有提交开关 |
| `automation/README.md` | 明确是付款页填表实验，默认测试卡材料，真实卡平台接入为后续事项 |
| `automation/package.json` | Node.js / Playwright 工程依赖，不是浏览器扩展 Manifest |

该目录仍有对外部后端的依赖、有限的硬编码套餐、测试资料生成和可能输出敏感材料的调试日志。它可用于理解模块职责，不能直接称为任意卡台接入即用的完整付款引擎。源码注释对登录成功、反检测等效果的表述也不等于当前线上实测。

## 4. Cookie-Editor 官方源码已下载并 Fork

- [官网](https://cookie-editor.com/) 直接链接 [Moustachauve/cookie-editor](https://github.com/Moustachauve/cookie-editor)。
- [Chrome 商店条目](https://chromewebstore.google.com/detail/cookie-editor/hlkenndednhfkekhgcdicdfddnkalmdm) 与官网、源码仓库相互对应。
- 新增用户 Fork：[7yedeng/cookie-editor](https://github.com/7yedeng/cookie-editor)，保留全部分支；网页显示 13 分支、15 标签，与上游 master 同步。
- 分析的 master 提交：`98633ba78a414937068eb3c33f0696684c6d2440`。
- 已发布的 `v1.13.0` tag 提交：`0d88685ac6fac0768205772023191136cf295caf`。
- 本地完整下载了 174 个 Git 跟踪文件；没有安装依赖、构建或运行扩展。

Chrome 商店当时显示版本 1.13.0。master 虽然 package 版本号仍为 1.13.0，代码却已增加后续修改，不能把 master 新能力当作已发布商店包能力。源码 `package.json` 标注 GPL-3.0-only；本轮没有把扩展实现复制进付款引擎。

### 主要代码与调用关系

| 源码 | 作用 |
| --- | --- |
| `manifest.chrome.json` | MV3；cookies、tabs、storage、sidePanel 权限；optional host permissions；无痕 split |
| `interface/lib/permissionHandler.js` | 查询/申请扩展自己的站点权限，排除内部页面 |
| `interface/lib/jsonFormat.js` | Cookie JSON 的解析和格式化 |
| `interface/lib/headerstringFormat.js` | Cookie header string 格式 |
| `interface/lib/netscapeFormat.js` | Netscape cookie 文件格式 |
| `interface/popup/cookie-list.js` | 导入按钮处理、非空数组检查、逐条写入、界面提示 |
| `interface/lib/genericCookieHandler.js` | 规范化属性后调用浏览器 cookies API |

核心调用关系是“解析输入 → 检查 Cookie 数组 → 逐条规范化并写入 → 刷新扩展列表”。扩展使用的 `cookies.set` 来自获准的浏览器扩展 API；它没有替网站创建一个新的有效会话。

### Session JSON 与 Cookie 数组的关系

用户说的“已有有效登录 Cookie，导入后刷新恢复会话”有技术依据；Zovo 官方工具页也确实提供相应格式转换。但三种输入不能混同：

| 输入 | 本身表达什么 | 不能直接据此证明什么 |
| --- | --- | --- |
| 应用返回的 Session JSON | 应用定义的字段和凭据 | 已经是 Cookie-Editor 可导入数组 |
| Cookie-Editor JSON | 带名称、值及域/路径等属性的 Cookie 对象数组 | 服务端一定接受登录 |
| Playwright storage state | Playwright 定义的 cookies 与 origin 存储状态 | 任意 Session JSON 或完整浏览器 profile |

Cookie-Editor 的 JSON 解析器本质是 `JSON.parse`，导入路径随后要求非空数组。一个能被正常解析的 Session 对象不会自动根据其中的字段转换为目标网站 Cookie。

## 5. 值得保留的实现细节和需要改进的地方

1. **写入成功与登录成功分别记录。** `cookie-list.js` 会捕获单条 Cookie 写入异常，但循环结束后仍显示通用的已导入提示。这不是所有条目成功，更不是账号认证成功。
2. **原导入路径不自动验证网站。** 导入后调用的是 `showCookiesForTab()`，刷新扩展列表；该路径没有站点账号校验或目标页自动 reload。
3. **Cookie 属性是接口的一部分。** 当前 master 处理 domain/path、Secure、HttpOnly、SameSite、到期时间和 partitionKey 等属性；导入时把数据重新绑定到当前目标 tab 的 Cookie store，不直接沿用输入源的 storeId。发布 tag 的分区支持不能与 master 混称。
4. **HttpOnly 的限制对象需要区分。** 它阻止页面脚本读取，获准扩展仍可通过 cookies API 操作。这不解除站点服务端的会话检查。[Chrome 官方 API](https://developer.chrome.com/docs/extensions/reference/api/cookies) 明确要求 cookies 权限及对应 host permissions。
5. **分账号不能只分 tab。** 多个普通 tab 可能共享 Cookie store；无痕 split 也不等于每个任务都拥有独立容器。服务器仍需独立 context/profile 和账号锁。
6. **Header string 不是完整状态快照。** 它只有 name/value，无法完整保留浏览器 Cookie 属性以及其他站点存储。
7. **Netscape 到期字段有静态疑点。** 已读代码中解析器产生 `expiration`，而 `prepareCookie()` 读取 `expirationDate`；这条调用链可能丢失到期值。本轮未运行该路径，不把静态疑点写成已复现的运行故障。

后续自有实现可采用“严格输入契约、逐条结果、属性回读、独立账号状态核验”的模块划分。生产引擎不需要依赖 Cookie-Editor 扩展，也不能把导入提示当作后续支付资格。

## 6. 本轮完成与待核验项

已完成：Zovo 官方插件入口及显示版本核对、Session 转换功能存在性核对、Cookie-Editor 完整源码下载/Fork/静态分析、官方站点权限文档与同类 issue 核查。

仍待核验：Zovo 官方 ZIP 的实际代码；本会话 host gate 的具体生效策略与正式解除；真实 Session 的在线登录；Go 实际结账、付款和取消续费。本轮没有为这些未完成事项提供成功结论。

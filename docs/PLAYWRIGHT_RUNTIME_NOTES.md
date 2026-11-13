# Playwright 运行条件与状态边界

本文围绕项目固定的 **Playwright Python 1.61.0** 整理官方证据，供独立浏览器执行层设计使用。
这是运行条件说明，不是部署记录；不证明真实 Go 页面、登录、价格、付款或取消续费已适配。
本轮只读取官方公开资料，没有使用用户 Session、登录真实账户或执行付款。

## 1. 版本与源码范围

- 官方实现分别位于 [microsoft/playwright][repo-core] 和 [microsoft/playwright-python][repo-python]。
- 下文链接到 `main` 的源码用于解释结构，不能当作项目 1.61.0 的逐行审计结果。
- 官方文档随版本更新；部署应固定 Python 包、浏览器构建及镜像版本，升级后重新核验。
- [发布说明][releases] 中 v1.61 已有 `page.local_storage`、`page.session_storage` 读写 API。
- OPFS 的 `storage_state` 保存选项属于 v1.63，不能算作项目 1.61.0 已有能力。
- v1.57 开始采用 Chrome for Testing；Linux arm64 到 v1.63 才切换，不能把该新行为套给 1.61.0。

## 2. Python 包仍需 driver 和真实浏览器

Python 的 [`_driver.py`][driver] 定位 Node 可执行文件和 `driver/package/cli.js`。
[`_transport.py`][py-transport] 的公开源码索引显示，Python 启动 `run-driver` 子进程，通过 stdin/stdout 与其通信。
随后由 driver 启动或连接真实浏览器；Python 业务脚本不是一个独立的浏览器内核。
常规发行包提供 driver 所需组件；从源码安装时，应按所固定版本检查 Node 的提供方式。
仅能导入 `playwright`，不足以证明浏览器执行文件、系统动态库和子进程运行条件齐备。

## 3. 完整浏览器与 headless shell

[浏览器文档][browsers] 区分以下用法；安装选项必须与启动配置对应：

| 启动选择 | 对应浏览器 | 安装注意 |
| --- | --- | --- |
| 默认无头、未指定 channel | headless shell | `--only-shell` 可省略完整浏览器 |
| `channel="chromium", headless=True` | 完整 Chromium 系浏览器的新无头模式 | `--no-shell` 可省略 shell |
| `headless=False` | 完整浏览器，有头运行 | 不能只安装 shell；还需要显示环境 |

当前 [`chromium.ts`][chromium] 的 `getExecutableName()` 仍体现这个选择，具体发行构建按固定版本确定。
新无头模式不等于有可见窗口；若需要人工查看或接管交互，应另行准备有头显示及交互条件。
两种无头模式可能存在行为差异，换模式后应验证实际页面，不能仅凭名称判断兼容。

## 4. 三种连接方式

| 方式 | 连接对象与边界 |
| --- | --- |
| `chromium.launch()` | 由 Playwright 启动并管理浏览器；正常本地启动默认使用调试 pipe |
| `chromium.connect_over_cdp()` | 通过 CDP 接入已运行的 Chromium 系浏览器，典型入口为 HTTP/WebSocket 端点 |
| `browser_type.connect()` | 接入 Playwright browser server；客户端与服务端的主、次版本应匹配 |

CDP 是控制协议，pipe 是传输方式，不是互斥的“两个协议”。
当前 [`chromium.ts`][chromium] 默认加入 `--remote-debugging-pipe`，并拒绝用户重复提供此参数。
当前 [`browserType.ts`][browser-type-source] 使用子进程的额外 stdio 通道创建 `PipeTransport`。
因此正常本地 `launch()` 不要求自行开放 TCP 调试端口；它与 Python 到 driver 的 stdin/stdout 又是不同一层。
[官方 API][browser-type] 明确：CDP 接入仅支持 Chromium，功能完整程度低于 Playwright 协议连接。
已有浏览器的启动参数也可能影响功能，不能把三个入口当作行为完全相同的替换。

## 5. Session JSON、cookies 与 storage_state

`Session` 只是业务字段名称，不能据此判断其数据属于浏览器的哪一种存储。
[认证文档][auth] 的复用流程是从已认证 context 导出状态，再由新 context 加载。

| 数据 | 能说明什么 | 不能直接替代什么 |
| --- | --- | --- |
| 某个 API 返回的 Session JSON / token | 该 API 的应用数据或凭据 | 完整浏览器登录状态 |
| cookies 列表 | 按 domain/path 等属性限定的 Cookie 状态 | localStorage、IndexedDB 等其他存储 |
| `storage_state` | Playwright 定义的 cookies、按 origin 组织的存储快照 | 任意 Session JSON 或完整磁盘 profile |
| `sessionStorage` | 页面会话中的 Web Storage 数据 | 默认的 `storage_state` 自动保存范围 |

需要 IndexedDB 时，1.61.0 保存状态应显式设置 `indexed_db=True`；不能假设默认已经包含。
sessionStorage 需要按应用实际用法单独保存与恢复；v1.61 的读写 API 不意味着它自动进入快照。
Cookie 的 domain、path、到期时间等属性以及 origin 必须保持真实含义，不能从 token 臆造。
加载某份状态后，仍需检查实际页面的身份和有效性；导入成功不等于已登录目标账户。
官方提醒状态文件可能包含可用于冒用登录的材料，不应提交到代码仓库。

## 6. Context、profile 与服务端账户锁

[BrowserContext 隔离][isolation] 覆盖 cookies、localStorage、sessionStorage 等浏览器状态；多个 context 可共用一个 browser。
非持久化 `browser.new_context()` 不把浏览数据作为持久化 profile 保存，但显式导出快照或记录 trace 是另外的操作。
`launch_persistent_context(user_data_dir)` 则使用磁盘 profile，返回其唯一 context；关闭它也会关闭浏览器。
[官方 API][browser-type] 明确，同一 User Data Directory 不能同时供多个浏览器实例使用，应采用专用自动化目录。

工程推论：context 隔离不隔离服务端账户；导入同一登录状态仍会访问同一份订阅、订单和付款记录。
项目应分别管理 context 生命周期、持久化目录互斥和同一服务端账户的任务锁。
独立 context 不能取代防止重复下单或并发付款的业务约束。

## 7. VPS / Docker 的部署前提

- 使用所固定版本支持的 Linux 发行版与架构，并安装 Python 包、对应浏览器及系统依赖。
- [官方安装说明][browsers] 提供 `playwright install --with-deps chromium`；实际安装需在有相应权限的部署环境进行。
- [官方 Docker 镜像][docker] 包含浏览器和系统依赖，项目的 Playwright Python 包仍要单独安装。
- 镜像版本必须与项目依赖对应；不要直接复制滚动文档中的最新镜像标签给 1.61.0 使用。
- 应提供足够的内存、共享内存及正常子进程运行条件；本文不指定未经实测的最低资源配额。
- Headless 不需要可见桌面；无显示环境的 Linux 要运行 headed，可按 [官方 CI 文档][ci] 配置 Xvfb。
- Xvfb 提供显示环境，本身不提供人工操作界面；有人工接管需求时需另行安排交互入口。

这些条件满足后，仍需分开验证浏览器启动、页面加载、状态恢复、身份核对和业务页面观察。
Playwright 不自带本项目的 Go 套餐合同、真实付款 DOM、3DS 人工交接或扣款结果核验逻辑。
当前离线 fixture 的通过只能证明对应合成输入下的行为，不能标记真实 Go 链路已可用。

## 官方来源

正文链接均指向 Microsoft 官方仓库或 Playwright 官方文档；`main` 与在线文档内容可能随版本变化。

[repo-core]: https://github.com/microsoft/playwright
[repo-python]: https://github.com/microsoft/playwright-python
[driver]: https://github.com/microsoft/playwright-python/blob/main/playwright/_impl/_driver.py
[py-transport]: https://github.com/microsoft/playwright-python/blob/main/playwright/_impl/_transport.py
[chromium]: https://github.com/microsoft/playwright/blob/main/packages/playwright-core/src/server/chromium/chromium.ts
[browser-type-source]: https://github.com/microsoft/playwright/blob/main/packages/playwright-core/src/server/browserType.ts
[browser-type]: https://playwright.dev/python/docs/api/class-browsertype
[browsers]: https://playwright.dev/python/docs/browsers
[releases]: https://playwright.dev/python/docs/release-notes
[auth]: https://playwright.dev/python/docs/auth
[isolation]: https://playwright.dev/docs/browser-contexts
[docker]: https://playwright.dev/python/docs/docker
[ci]: https://playwright.dev/python/docs/ci

# 完整 Session → 本机浏览器会话恢复

版本：0.1.1。日期：2026-11-14（北京时间）。

## 这次修正了什么

- 加载后直接展开面板，标题显示 `会话恢复 · v0.1.1`；可点击“收起”保留右下角按钮。
- 修复可复现的启动问题：网页中存在全局 `module.exports` 时，旧版会误以为自己运行在 Node 测试环境而提前结束。新版只在没有浏览器 DOM 的测试进程导出模块，不向网页对象暴露流程函数。
- 菜单注册失败不再影响页面面板；缺少 Cookie 接口、浏览器安全接口时显示固定错误并禁用恢复按钮。
- 界面初始化异常会显示 `UI_BOOT_FAILED` 提示，不显示原始异常。此处理不能捕获脚本解析前的语法错误。
- 去掉可选链、空值合并和 `replaceAll` 依赖，减少编辑器语法兼容差异；未增加权限或更改登录目标。

### 对截图中语法错误的判断

截图显示 `Unexpected token function`，提示框遮住了 `buildCookies` 前一行。本地原始 0.1.0 文件通过 `node --check`，该位置有完整的闭合大括号。因此目前不能证明安装副本缺少某个括号，也不能把本次修正说成已修复你浏览器里的全部问题。请用本包的**完整文件**替换编辑器全部内容，再保存；不要只替换截图附近几行。

保存后，先确认标题版本为 0.1.1，再正常刷新目标网页。若仍没有面板，检查该脚本的匹配/排除规则与扩展用户脚本运行权限。面板能出现与网站接受登录材料是两个独立的验收步骤。

## 这份脚本的交互

加载后面板自动打开；收起后可通过右下角“会话恢复”重新打开。粘贴**完整 Session JSON**，点击“替换登录并验证”。脚本自行完成：

1. 识别完整 JSON。
2. 提取 `sessionToken` 和目标账号身份。
3. 生成浏览器所需的 Cookie 记录，必要时拆分长值。
4. 验证 HttpOnly 权限，替换指定会话 Cookie，逐项回读比较。
5. 刷新到网站首页。
6. 读取当前会话，比对输入中的用户 ID、账号 ID和邮箱；匹配才显示“登录成功”。

不需要先打开博客转换器，不需要再打开 Cookie-Editor，也不需要自行填写 Cookie 名称。

## 必须先知道的兼容条件

**按 2026-11-13 查到的 Tampermonkey 官方文档，HttpOnly Cookie 目前仍只支持 Beta。普通正式版不是本工具承诺支持的环境。** Cookie-Editor 自身的扩展权限不会自动授予油猴脚本；本脚本使用 Tampermonkey 的 `GM_cookie` 接口。

- 从 [Tampermonkey 官方下载页](https://www.tampermonkey.net/index.php?browser=chrome)选择 **Tampermonkey BETA** 的官方商店入口。
- Chrome 138 及以上版本需要在该扩展的详情页启用 **Allow User Scripts / 允许用户脚本**。这是脚本运行权限，与 HttpOnly Cookie 权限不同。
- Beta 还需要实际允许本脚本操作 HttpOnly Cookie。不同版本的界面名称可能不同；这里不编造未经验证的设置路径。脚本会在改动登录前自动探测实际能力。
- 只在 `https://chatgpt.com/` 的顶层网页运行；不在其他网站或嵌入框架中运行。
- 网站仍须接受这份会话。过期、撤销或被服务端拒绝的凭据不会因为转换成 Cookie 而重新有效。

能力检查使用随机名称的短期测试 Cookie，不包含登录材料；检查写入、属性、删除和删除后的回读。未通过时不会替换登录 Cookie，也不会降级成普通非 HttpOnly Cookie。

## 安装和使用

1. 为测试使用一个专用浏览器配置，并在该配置内安装 Tampermonkey BETA。**普通新标签页不能隔离账号；本操作影响同一配置里的其他 ChatGPT 标签页。**
2. 下载 `session-login.user.js`。在油猴里新建用户脚本，用文本编辑器打开下载文件，复制全部内容替换新建脚本模板，然后保存。这样无需依赖浏览器能否从本地文件自动弹出安装页。
3. 按当前浏览器提示授予该脚本运行所需的权限。不要同时在正式版与 Beta 中各启用一份脚本。
4. 正常打开 ChatGPT 网页。面板应自动展开并显示版本 0.1.1；也可从油猴菜单选择“打开会话恢复面板 · v0.1.1”。缺少 Cookie 能力时仍显示面板及错误信息。
5. 打开面板，粘贴你自己的完整 Session JSON，点击“替换登录并验证”。按钮本身就是启动操作；无需另行手工导入 Cookie。
6. 等待刷新。脚本会自动继续最后的身份核对，显示成功、失败或“无法确认”，以及不含凭据的错误代码。

网站若显示验证、拒绝或无法正常加载，本脚本不会尝试绕过，也不会进行连续自动重试。无法运行时先确认脚本版本和权限；不要把完整 Session 放进截图、报错或公开讨论。

## 哪些输入被接受

- 必须是完整 JSON 对象。
- 必须有非空的顶层 `sessionToken`。
- 必须有可核对的 `user.id` 或 `user.email`。如果输入还有 `account.id`，它也必须在登录后匹配。
- 有 `expires` 时必须能解析且尚未到期；这只是输入检查，不等于服务端已认可。
- 不接受只有 Access Token 的输入，不尝试解密、重签或生成新凭据。

只提取当前功能需要的字段，不持久化完整 Session、Access Token 或 Cookie 快照。

## 如何判定“上号成功”

界面严格区分三件事：

- Cookie 接口调用完成。
- 写后回读的值、分片集合和属性正确。
- 刷新后，网站返回的账号身份与输入身份一致。

只有最后一项通过，才显示“登录成功”。HTTP 200、任意邮箱、任意访问令牌、单纯刷新成功，都不足以单独判定目标账号已登录。

身份检查调用同一网站的当前会话接口。请求固定在该网站，不向 Zovo、博客或其他第三方发送输入。

## 数据与故障处理

- 输入框在启动后、收起面板和离开页面时清空。
- 不把输入写入日志、localStorage、剪贴板、URL 或 GM 的长期存储。
- 为跨越一次刷新，`GM_saveTab` 仅保存两分钟有效的任务版本、随机标记、阶段及账号身份的 SHA-256 摘要；不包含 Token。摘要不是“完全匿名”，但不保存账号明文；记录在核验前被一次性清除。
- 浏览器会话 Cookie 的写入是本工具的目的。脚本没有设置延长服务端有效期的逻辑，也不承诺浏览器关闭必然清除会话——浏览器自身的会话恢复设置可能影响 Cookie 生命周期。
- 只处理明确列出的会话 Cookie 名组。其他 Cookie 不作为清理目标。
- 遇到不支持的 Cookie 路径、分区或异常数量，在开始替换前停止。
- 刷新前发生明确的写入失败时，尝试从内存恢复旧会话并回读检查；若操作超时或发现其他程序改动，则停止，不盲目覆盖。
- 刷新后内存快照不再存在；身份不匹配时不会自动清除新会话，也不会声称旧账号已经恢复。
- 同一个浏览器配置中的脚本任务使用跨标签页锁；它不能阻止其他扩展、网站或人工操作，也不提供服务器批量账号隔离。
- 没有付款、开卡、充值、取消续费或修改订阅功能。

## 常见错误

| 代码 | 含义 |
| --- | --- |
| `GM_UNAVAILABLE` | 所需油猴接口没有提供或没有授权。 |
| `RUNTIME_UNAVAILABLE` | 浏览器未提供所需的安全随机数、摘要或请求控制能力。 |
| `UI_BOOT_FAILED` | 界面初始化异常，尚未启动会话恢复；完整替换脚本后检查版本。 |
| `HTTPONLY_UNAVAILABLE` | 没通过 HttpOnly 实际能力检测；未修改登录 Cookie。 |
| `TOKEN_MISSING` | 输入缺少 `sessionToken`。 |
| `EXPIRED` | 输入记录的有效期已过。 |
| `COOKIE_SCOPE` | 存在未覆盖的会话 Cookie 路径或分区，已停止。 |
| `COOKIE_READBACK` | 写入后的实际 Cookie 不完全符合计划，不能视为成功。 |
| `COOKIE_TIMEOUT` / `UNCERTAIN` | 操作结果未知；没有继续自动写入。 |
| `STATE_BUSY` | 同配置已有脚本任务，或短期任务记录尚未过期。 |
| `HTTP` / `RESPONSE` / `NETWORK` | 网站未正常返回可核对的会话，结果未确认。 |
| `NOT_LOGGED_IN` | 网站没有返回已登录身份。 |
| `SESSION_REJECTED` | 网站报告会话错误或返回的会话已过期。 |
| `IDENTITY_MISMATCH` | 实际账号与输入目标不一致。 |
| `IDENTITY_UNCONFIRMED` | 缺少必要的返回身份字段，不能确认目标账号。 |
| `ROLLBACK_FAILED` | 不能确认旧会话已经恢复，需要手动检查当前状态。 |

反馈问题时提供错误代码、停在哪一步、Chrome 与 Tampermonkey 版本即可。不要发送 Session、Cookie 值或带有凭据的控制台记录。

## 验证范围

此交付包含 31 项核心离线模拟测试和 7 项 UI 启动回归测试。核心测试使用合成的 Session 与模拟 Cookie 存储；UI 测试在 Node VM 的简化 DOM 中检查全局 module、延迟挂载、缺少接口、菜单失败等具体启动场景。VM 测试不等同于真实 Chrome/Tampermonkey 验收。没有使用你的真实会话，没有在 ChatGPT 上实际安装或运行此脚本，也没有执行真实登录或付款。

离线测试命令（需要 Node.js 20+，不需要安装第三方包）：

```text
node --check session-login.user.js
node --test session-login.test.cjs session-login.ui.test.cjs
```

具体测试结果见同包 `TEST_RESULTS.txt`。离线测试不能代替你本机的 Beta 权限和网站实际接受情况。

## 官方参考

- [Tampermonkey GM_cookie API 与 HttpOnly 限制](https://www.tampermonkey.net/documentation.php?q=GM_cookie)
- [Tampermonkey 标签页状态接口](https://www.tampermonkey.net/documentation.php?q=GM_tabs)
- [Tampermonkey 用户脚本执行权限](https://www.tampermonkey.net/faq.php?q=Q209)
- [Chrome Cookie API](https://developer.chrome.com/docs/extensions/reference/api/cookies)

本工具为独立编写的本机工具，未复制或打包 Zovo 的压缩构建产物，也不是 OpenAI 或 Tampermonkey 官方登录客户端。

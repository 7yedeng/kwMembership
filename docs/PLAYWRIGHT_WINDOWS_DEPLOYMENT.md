# Windows Playwright 运行环境部署记录

日期：2026-11-14。范围：安装现有执行器、验证本机浏览器运行环境；没有验证真实账号或真实付款页面。

## 安装结果

- 源码仓库：`7yedeng/kwMembership`。
- 固定源码提交：`9c1c1abb2cb21e8c86cb8decf31698ffd67d1551`，验收后源码工作区干净。
- Windows 安装目录：`%LOCALAPPDATA%\kwMembership-playwright-20261114`。
- 独立 Python venv：Python 3.13.5，Playwright 1.61.0。
- 按该版本 CLI 安装 Chromium / Chrome for Testing 149.0.7827.55（revision 1228）、配套 headless shell、FFmpeg 和 winldd。
- 安装成功后 `pip check` 返回 `No broken requirements found`。
- `python -m python_executor --help` 成功。没有运行会领取任务的 `--once`，没有启动生产消费者。
- 本机原有全局 Playwright 为 1.60.0；本次使用独立 venv，没有用它冒充仓库固定版本。
- 本次环境没有把现有桌面付款前工具的 Go 驱动替换成 Playwright。

安装包、源码、浏览器和日志分别位于 `venv`、`source`、`browsers`、`reports`。安装根目录有 `README_zh-CN.md`、`INSTALLATION.json` 和 `Verify.cmd`。双击后者仅重跑既有本地重启验收，不读取账号或领取订单。

结构化结果：[playwright-windows-runtime-20261114.json](evidence/playwright-windows-runtime-20261114.json)。

## 可核实的实现来源

本地已验证的上游基线是 `varrge/kwMembership` 的 `16ceb97fc15f629c8b59df242e1baee2227e4c77`。本轮没有把该本地 ref 称为最新上游版本。

| 文件 | 来源 |
| --- | --- |
| `python_executor/runner.py` | 与上述上游基线 blob 完全相同。 |
| `python_executor/requirements.txt` | 与上述上游基线完全相同，固定 Playwright 1.61.0。 |
| `python_executor/live.py` | 基于该上游执行器，包含 fork 既有修改。 |
| `scripts/verify-prepayment-restarts.py` 及 restart 测试文件 | 此前 fork 的 `eb4309f8c4173a982c7499d29f290ab8a94c7047` 增补，上游基线不存在这些文件。 |

准确描述是：**安装上游执行器基础上的现有 fork，复用 fork 已有验收脚本**。本轮没有重新编写一套模拟付款页面。

## 实际运行的仓库命令

在安装根目录中创建独立 venv 后，按 README 运行固定依赖和配套浏览器安装。安装与验收子进程设置 `PLAYWRIGHT_BROWSERS_PATH` 指向该安装目录的 `browsers`，临时文件写入同目录的 `tmp`，不依赖全局浏览器缓存。

```text
venv\Scripts\python.exe -m pip install --disable-pip-version-check --no-input --no-cache-dir -r source\python_executor\requirements.txt
venv\Scripts\python.exe -m playwright install chromium
venv\Scripts\python.exe -m pip check --disable-pip-version-check
```

下面命令的工作目录为 `source`，Python 使用上面的独立 venv：

```text
python -m python_executor --help
python -m unittest python_executor.tests.test_client python_executor.tests.test_live python_executor.tests.test_fill_dom python_executor.tests.test_plan_scripts -v
python scripts/verify-prepayment-restarts.py --require-browser --output <reports>/browser-restarts.json
```

## 实测结果与边界

| 检查 | 结果 | 证明的范围 |
| --- | --- | --- |
| 现有离线代码／桥协议测试 | 46 通过 | 本地 HTTP 桥契约、现有脚本与执行步骤。 |
| 现有 restart 验收 | 5 项测试通过，完成 3 次真实 Chromium 生命周期；0 跳过、0 失败、0 错误 | 启动、隔离、现有 iframe 填写／回读、清理与提交前停止。 |
| 完整 Chromium persistent profile 启动 | 通过 | 使用 `launch_persistent_context` 启动到 `about:blank`，空状态、关闭和 profile 删除均确认。 |
| 真实账号／实时账单／正式页面字段 | 未验证 | 本轮未访问真实业务页面。 |
| 生产 worker／订单领取／实际付款 | 未启动／未执行 | 运行环境验收不等于完整履约系统上线。 |

重启测试使用 `.invalid` 本地页面，复用既有生产填表代码；每条页面请求仅本地 fulfill 或 abort，闭合回环代理阻止意外外连。报告中三次生命周期均记录 `unexpectedRequests=0`、`unmarkedResponses=0`、`stoppedBeforeSubmit=true` 和 `browserClosed=true`。

额外的完整 Chromium 启动检查只打开 `about:blank`，使用新建临时 profile、关闭 Service Worker、阻止页面请求，并显式请求 Chromium sandbox。它验证的是完整浏览器的 profile 生命周期，**不能推导真实站点接受该浏览器或登录态**。

所有证据保持：

```json
{
  "workerStarted": false,
  "realAccountVerified": false,
  "liveBillingVerified": false,
  "paymentSubmitted": false
}
```

## 还没有完成的接线

1. 当前独立桌面付款前工具通过 `internal/preparation/run.go` 固定创建 Go ChromeExecutor，没有可直接切换的 Playwright 参数。
2. Python `live.py` 的 `PREPARE_PLUS_JS` 与独立 Go 工具的 `prepayment_entry.js` 分开。安装 Playwright 不会自动获得 v3 账号别名合并修复。
3. 现有 Python runner 面向 Go worker 的任务、lease、材料和 permit 协议，并不实现独立工具的 `PreparePrepayment` 接口。`--once` 会领取任务；live 模式要求付款开关，不能当作独立付款前自检。

后续适配应复用既有生命周期和填表实现，共用资格查询和脱敏诊断，并接入独立付款前契约。真实页面选择器与账单回读仍需单独验证。

完整 worker 还依赖已初始化的 kwRedeem 数据库、配套 JWT_SECRET 和 Go/Python 一致的执行桥密钥。本次没有验证或修改这些生产配置，也没有新造业务数据库。

## Browser host 权限诊断

上一轮 Browser 工具能列出标签页元信息，但读取 ChatGPT 页面内容时返回：

> Browser use could not acquire authority to observe page content.

并明确指出 host restrictions gate 未授予 `https://chatgpt.com` 访问权限。工具没有返回所命中的具体规则，因此不能把这条错误解释成网站 HTTP 拒绝、账号失效、IP 问题或 Chromium IPC 故障。截图中扩展已启用且有 Errors 按钮；没有错误详情，无法建立因果联系。

原始拒绝同时禁止使用另一驱动、CDP 或其他操作面达到同一被拒绝的页面操作。本次安装与测试均限于允许的独立运行环境，没有采用换通道访问真实目标页面。

公开官方资料给出的可排查方向：

- [Chrome 扩展说明](https://learn.chatgpt.com/docs/chrome-extension)：扩展权限之外，ChatGPT 仍有自己的站点权限；桌面设置的 Computer Use → 浏览器 Manage 可管理允许／阻止名单。
- [内置浏览器与 Chrome 使用说明](https://help.openai.com/en/articles/20001277-using-the-built-in-browser-in-the-chatgpt-desktop-app)：网页版设置的 Chrome extension 入口；内置浏览器可保留当前对话的 Deny，官方建议在保留原对话的同时用新对话检查，但网站与工作区限制仍然适用。
- [工作区浏览器策略](https://help.openai.com/en/articles/20001510-manage-browser-and-computer-use-in-your-enterprise-workspace)：管理要求、站点规则和用户允许是不同层级。该资料不能证明本次实际命中了管理策略。
- [联系支持](https://help.openai.com/en/articles/6614161-how-can-i-contact-support)：可以提供发生时间、环境版本、会话标识、截图和脱敏错误，确认具体拒绝原因。

本轮未找到公开解释 `host restrictions gate` 这一具体错误名称的官方页面，也未确认一个适用于本例的关闭开关。应修复或确认实际权限来源，不能把依赖安装成功当成权限已经恢复。

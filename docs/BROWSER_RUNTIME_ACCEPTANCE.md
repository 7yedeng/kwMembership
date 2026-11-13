# 浏览器运行面与完整 Chromium 启动验收

核验日期：2026-11-13。这里的“云命令执行器”“共享云浏览器”“用户本机 Chrome”是不同的执行入口，不能相互替代证据。本文不包含账号、Session、卡片或付款资料。

## 本轮观察

| 运行面 | 实际观察 | 证据范围 |
| --- | --- | --- |
| 当前受管 Linux 命令执行器 | AF_UNIX stream/datagram 在 socket 创建阶段返回 EPERM；socketpair 和 127.0.0.1 TCP 收发正常 | 与此前 IPC 结果一致；“本地”在旧文档中指这个云执行目录，不是用户的 Windows 电脑 |
| 同一命令执行器的 Python 环境 | 安装了 Playwright 1.62.0；路径查询显示默认完整 Chromium 文件不存在；查询结束还有 driver 关闭警告 | 既不是项目固定的 1.61.0，也没有通过浏览器启动；安装 Python 包不能等同安装了浏览器 |
| 共享云浏览器 | about:blank 成功打开；自定义 data URL 被工具协议策略拒绝；官方 HTTPS TodoMVC 示例返回 Site Unavailable | 只证明空白页可启动，未通过完整交互；Site Unavailable 本身不能证明网站反自动化拦截 |
| 用户本机 Chrome 扩展 | 官方 HTTPS TodoMVC 示例成功加载；输入合成任务、回车提交、勾选完成后，页面显示 0 items left | 已验证页面读取、输入、键盘和点击；未验证账号登录、独立 profile、浏览器状态导入或付款 |
| GitHub runner | 此前固定提交的 headless shell 离线 DOM 测试通过 | 不证明当前云命令执行器恢复；也不直接证明完整 Chromium + Xvfb 的生产启动路径 |

本机交互只向 [Playwright 官方测试示例](https://demo.playwright.dev/todomvc/) 写入无敏感信息的合成任务，没有导入用户认证材料。

## 完整 Chromium + Xvfb 验收

新增 [TestVerifyChromeHeadedRuntime](../internal/checkout/chrome_runtime_test.go)，直接调用生产 [VerifyChrome](../internal/checkout/chrome.go)，使用 `visible=true`。该函数启动独立临时 profile，只导航到 `about:blank`，经 CDP 读取浏览器标识并关闭。

[offline-validation.yml](../.github/workflows/offline-validation.yml) 中独立的 `chromium-headed-runtime` job：

1. 在 `ubuntu-24.04` runner 检查镜像原有的正式 `google-chrome-stable` 包、Xvfb 及发行版 Chrome AppArmor 配置。
2. 使用包原有的 `/opt/google/chrome/chrome`，记录实际版本；不移动下载的浏览器、不增加权限规则。
3. 检查该文件可执行、不是符号链接、由 root 持有且不可被组或其他用户写入。浏览器进程本身仍由非 root 运行。
4. 以非 root 身份、独立 Xvfb display 调用实际 Go 启动检查；不增加 `--no-sandbox` 或修改宿主策略。
5. 测试内部预算 40 秒，Go 测试总预算 45 秒。检查 JSON 事件中这个精确测试名确实通过；未匹配、skip 或失败都不能得到成功。

普通本地没有设置浏览器路径时，该集成测试明确 skip。CI 设置 `KWMEMBERSHIP_REQUIRE_CHROME_RUNTIME_TESTS=1`，缺失配置必须失败。配置路径后，root、缺 DISPLAY、路径不是绝对可执行文件均失败。

测试的真实结论以对应提交的 Actions run 为准。仅新增 workflow 或编译测试文件不能算启动验收成功；若正常 sandbox 启动失败，应记录原始原因，而不是移除安全条件取得绿色结果。

首次 [run 43405913872](https://github.com/7yedeng/kwMembership/actions/runs/43405913872) 在提交 `a7055f10b86360f224403d58e31ab418111f3296` 使用 Playwright 1.61.0 下载的完整浏览器，安装与路径解析成功，但生产启动测试实际失败，stderr 为 `No usable sandbox`。同一 run 的 Go fixtures 和 59 项 Python 测试通过，不能用这两个结果覆盖完整浏览器失败。

[Chromium 官方说明](https://chromium.googlesource.com/chromium/src/+/main/docs/security/apparmor-userns-restrictions.md) 区分开发/下载构建与正式 Chrome 安装：Ubuntu 自带适用于 `/opt/google/chrome/chrome` 的策略。后续 job 因此验证 runner 原有正式浏览器及其系统集成，不修改 AppArmor/sysctl，不以下载构建覆盖允许路径，也不关闭浏览器 sandbox。这是不同发行安装方式的明确兼容验收。

该 Go 启动 job 使用 runner 的正式 Chrome 版本，**不声称它是 Playwright 1.61.0 绑定的浏览器版本**。原 `python-browser` job 继续用固定 1.61.0 的官方 headless shell 验证 Python/DOM，两种运行路径分别记录。

在满足条件的 Linux 测试主机，可显式运行：

```sh
KWMEMBERSHIP_CHROME_RUNTIME_PATH=/absolute/path/to/full-browser \
KWMEMBERSHIP_REQUIRE_CHROME_RUNTIME_TESTS=1 \
xvfb-run -a -s '-screen 0 1280x900x24' \
go test -count=1 -timeout=45s -run '^TestVerifyChromeHeadedRuntime$' -v ./internal/checkout
```

这不是账户或付款验收。此测试不读取 worker 配置、数据库、Session、代理配置或真实网站。

## host restrictions gate 与登录状态

原账号操作遇到的是浏览器工具的 host restrictions gate。它与 Linux socket 创建失败、浏览器二进制缺失、网站自己的登录/风控响应分别属于不同层。工具没有暴露具体拒绝规则，因此不能认定是 Session 到期、代理地区、账号状态或某条可自行修改的开关。

OpenAI 官方说明了分别管理 [云浏览器网站权限](https://help.openai.com/en/articles/20001280-using-cloud-browser-in-chatgpt) 和 [Chrome 扩展网站权限](https://help.openai.com/en/articles/20001277-using-the-built-in-browser-in-the-chatgpt-desktop-app) 的入口；这些文档不证明某个特定拒绝可以由用户设置解除。个人设置、工作区规则和产品限制的适用情况仍需权限界面或管理方确认。

没有通过换浏览器、原始 HTTP、CDP、CI 或导入 Session 来完成已被拒绝的账号操作。本轮共享云与用户本机的测试都不涉及 ChatGPT 账号。

Playwright 的状态格式、隔离和部署要求见 [PLAYWRIGHT_RUNTIME_NOTES.md](PLAYWRIGHT_RUNTIME_NOTES.md)；精确 IPC 阶段分析见 [IPC_DIAGNOSIS_FOLLOWUP.md](IPC_DIAGNOSIS_FOLLOWUP.md)。

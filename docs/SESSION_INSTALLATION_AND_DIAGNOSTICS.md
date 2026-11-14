# 本机会话工具、空白页与粘贴问题

## 本轮现场结果

- 已将 Windows 会话验收程序安装到用户指定项目的 outputs 独立目录，并创建桌面“Session 登录测试”快捷方式。
- 安装时按压缩包及各文件 SHA-256 校验，再运行 Windows 原生 `membership-session.exe --inspect`，使用完全合成输入，结果为 `SESSION_INSPECTED`。
- Tkinter 界面初始化通过，随后实际打开独立桌面窗口；进程有主窗口且正常响应。输入初始为空，没有自动恢复账号。
- 本机 `py -3` 启动器指向不存在的路径；现有 `python` 解释器及 Tkinter 可用。启动脚本改为先检测可用的 `python`，再回退 `py -3`。没有修改全局解释器注册或 PATH。
- 安装的会话核心来自提交 `36c1af26f2b60eae46c7cfb426b2191eb39660e8`；会话 EXE SHA-256 为 `a6a55fe40b1ba94381427dec03b921b5f33798fc59994663a5f3b646cf02694b`。本轮独立更新启动脚本和油猴 0.1.2；安装目录的 `INSTALLATION.json` 分别记录组件来源，不能把整套文件冒称同一个二进制构建版本。

真实 Session 由用户自行输入。用户随后明确反馈：已使用自己的两个账号 Session 测试成功。因此“本机会话恢复”记为**通过用户实测（两个账号）**。这不等于助手读取或独立复核过两份账号结果，也不外推为隐藏模式、VPS、Go结账或付款已通过。以上助手执行的安装及合成测试没有读取系统剪贴板、创建订单、绑卡或付款。

## about:blank 与访问限制

`about:blank` 是浏览器的空白文档。程序在创建隔离浏览器、尚未导航时可能短暂显示它。Cookie 基础测试刻意停留在此页面，所以该测试不检查公网出口、ChatGPT 登录有效性或区域报价。

本轮通过原有受管 Browser 通道重新尝试从新标签打开 `https://chatgpt.com/`，返回：

```text
The browser blocked this navigation; this was not a site-status decision or a user rejection.
Browser use could not acquire authority to trigger network egress.
The browser host restrictions gate did not grant access for https://chatgpt.com.
```

这份证据表明控制工具未获准发出本次访问请求，不是 ChatGPT 返回的 HTTP 错误或 IP 信誉判断。该响应没有给出策略配置来源，不能断言它由账号、出口、组织设置或某个具体用户开关造成，也没有已验证的解除方式。

同一工具被明确拒绝后，没有切换到 CLI、其他浏览器或原始请求访问相同目标。上一轮成功的 CI 空白页/合成页面测试，与用户手动访问真实网页，均不能证明这条受管通道已获得访问权限。对空白页本身刷新也不会自动变成目标网页。

## 0.1.2 输入隔离与证据范围

closed Shadow DOM 不会自动阻止 composed UI 事件进入父文档的事件路径。原脚本缺少独立输入文档，存在被页面全局监听器改变焦点的机制。新版将敏感输入框放到初始 `about:blank` 子文档，使用 `sandbox="allow-same-origin"`，不开放脚本、表单、弹窗或导航能力；创建失败会停用恢复，不退回宿主页面输入框。

Windows 已安装的真实 Chrome 上，`tools/userscript/session-login.browser.test.py` 的七个检查全部通过：

1. closed shadow 对照组能复现提前注册的父页面监听器抢焦点。
2. 新版原生打字与 Enter 留在子文档。
3. 合成 composed 键盘/粘贴事件不进入父页面监听器。
4. 焦点、指针、滚动边界成立，父页面没有敏感 textarea 回退。
5. 收起会清空，菜单重新打开会聚焦。
6. 子文档访问失败时停用恢复。
7. Trusted Types 策略下仍可初始化，无 srcdoc/innerHTML 注入。

所有页面由本地 fixture 提供，其他网络请求中止。未使用系统剪贴板；合成 paste 事件不等于真实操作系统 Ctrl+V 验收。该测试证明可复现的事件机制及修复，不能断言用户当时现场的唯一原因，也不证明真实 Tampermonkey 环境的全部兼容性。

参考：[MDN Event.composed](https://developer.mozilla.org/en-US/docs/Web/API/Event/composed)、[MDN iframe](https://developer.mozilla.org/en-US/docs/Web/HTML/Reference/Elements/iframe)、[Chromium 特殊 URL 文档](https://chromium.googlesource.com/chromium/src/+/lkgr/docs/special_case_urls.md)。

## 后续验收

账号验收看 `SESSION_VERIFIED` 与精确 Cookie 回读、远端身份匹配、清理状态，不以浏览器窗口出现作为成功。付款前多轮验证见 [重复启动验收](PREPAYMENT_RESTART_ACCEPTANCE.md)，提交后只读恢复及清算见 [对账验收](PAYMENT_RECONCILIATION_ACCEPTANCE.md)。

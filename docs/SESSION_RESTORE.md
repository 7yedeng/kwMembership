# 独立会话恢复与本机验收工具

本模块只处理“已有会话材料 → 隔离浏览器 → 指定账号身份核验”。不创建结账、不读卡、不绑卡、不付款、不修改续费，也不进入生产 worker。用户界面与 CLI 复用同一 Go 核心；显示浏览器是可拆卸的观察选项。

## 当前验收边界

本次通过了合成材料、浏览器替身和 GUI 输入输出契约测试。没有读取真实 Session，也没有访问 ChatGPT。已有受管工具的 host restrictions gate 尚未解除，不能用这个程序作为绕过该工具限制的路线。实际账号验收由用户在自己有权访问的环境中手动运行。

只有结果同时为 `remoteVerified: true`、`identityMatch: true` 才说明本次观察核对到了指定账号。`SESSION_INSPECTED` 只表示离线材料结构检查通过；Cookie 回读通过也不是账号核验成功。

## 模块边界

| 模块 | 职责 |
| --- | --- |
| `internal/sessionrestore` | 输入分类、目标身份、Cookie 精确回读、执行顺序、生命周期、脱敏结果 |
| `internal/sessionrestore/chrome.go` | chromedp 浏览器适配器，临时 profile，读取限定身份事实 |
| `cmd/membership-session` | 标准输入、CLI 参数、取消信号、JSON 报告 |
| `tools/session-desktop/app.py` | 本机多行输入、显示选项、调用 CLI、阶段提示与取消 |

`Browser` 和 `Factory` 是替换浏览器实现的接口，不与卡台、数据库、付款状态机绑定。现在使用项目固定版本的 chromedp/cdproto，没有额外 Node 服务。核心仍使用 `provider.BrowserSessionFromJSON` 解析既有会话材料。

## 构建与启动

从仓库根目录，使用符合 go.mod 的 Go 版本：

```sh
go build -o bin/membership-session ./cmd/membership-session
python3 tools/session-desktop/app.py
```

Windows 构建：

```powershell
go build -o bin/membership-session.exe ./cmd/membership-session
py -3 tools/session-desktop/app.py
```

也可以双击 `tools/session-desktop/start.cmd`。Linux/macOS 使用 `sh tools/session-desktop/start.sh`。桌面界面需要带 Tkinter 的 Python；不自动安装任何依赖。GUI 会尝试定位 `bin/membership-session(.exe)` 和常见浏览器路径，也可手动选择。它不会读取系统浏览器 Profile、密码或现有 Cookie。

GUI 输入框初始为空。粘贴完整 Session 后，直接按“恢复登录并核验”；恢复任务会先检查输入。“仅检查输入”是可选的独立离线操作，每次启动任务都会清空输入框，因此检查后若要恢复，需重新粘贴 Session。输入只经标准输入管道传给程序，不会进入命令行参数或保存为额外 Session 文件。

## CLI 使用

默认行为仅离线检查：

```sh
membership-session --inspect < my-session.json
```

这里的 `my-session.json` 指用户自己已有的输入文件，程序不会替用户创建它。也可由自己的程序把 JSON 写入 CLI 的 stdin。

显式恢复并显示独立浏览器：

```sh
membership-session --restore --chrome-path /path/to/chrome --show-browser --hold-open 2m < my-session.json
```

不显示浏览器时省略 `--show-browser` 与 `--hold-open`。隐藏模式可能被目标网站拒绝，显示开关本身不证明会话有效，也不会改变身份核验标准。Linux 可使用合法配置的私有显示环境；工具不配置 Xvfb、不改浏览器沙盒、不连接已有个人 profile。

`--timeout` 默认 90 秒，允许 1 秒到 3 分钟。`--hold-open` 仅用于已核验后的可见观察，最多 5 分钟。观察结束后关闭并删除本次临时 profile，不会把登录态留下给另一个长期浏览器。

CLI 使用 Ctrl+C 取消。GUI 使用私有 stdin 控制协议发送取消，无须强杀 Windows 子进程；关闭 GUI 时会等待独立浏览器清理后退出。异常断电或系统强制结束进程可能留下临时文件，不保证系统层完全没有磁盘痕迹。

## 需要哪些 Session 字段

- 必须有真实的 `sessionToken` 和可核对邮箱；只有 `accessToken` 不支持这一恢复路径。
- 如有 `user.id`，实际登录必须同时匹配用户 ID 和邮箱；缺失实际用户 ID 不会退回只比邮箱。
- 顶层 `email` 与 `user.email` 同时存在但冲突时停止。
- `expires` 只作为输入时的过期检查，不修改服务端有效期。
- 不解密、不补造、不重新签名 Session；保留 token 原值并使用既有解析器分块。

浏览器创建后先写 Cookie，再逐项回读比较值、域、路径、Secure、HttpOnly、SameSite、会话属性和分区状态，之后才打开账号页面。页面内读取会话响应，只把有限的用户身份字段返回 Go；不会把完整 API 响应和 accessToken 写到结果中。

## 结果与故障定位

| 结果 | 含义 |
| --- | --- |
| `SESSION_INSPECTED` | 输入结构通过，未访问远端 |
| `SESSION_VERIFIED` | 页面身份与目标一致，且正常完成清理 |
| `SESSION_TOKEN_MISSING` | 缺少用于恢复的 sessionToken |
| `SESSION_COOKIE_MISMATCH` | 写入后的 Cookie 属性或值不一致，未继续导航 |
| `SESSION_SITE_DENIED` | 实际页面会话接口返回 403，不自动切换网络或客户端 |
| `SESSION_REJECTED` | 会话接口拒绝或返回认证错误 |
| `SESSION_IDENTITY_UNCONFIRMED` | 观察不到所需身份字段 |
| `SESSION_IDENTITY_MISMATCH` | 登录到了不同账号，不能作为成功 |
| `SESSION_PROFILE_CLEANUP_FAILED` | 未确认临时 profile 已清理；不能按整体成功结束 |
| `SESSION_CANCELLED` / `SESSION_TIMEOUT` | 已取消或超过预算，读取报告中的已完成证据 |

报告不含邮箱、用户 ID 或 Cookie 值。`targetFingerprint` 是目标身份的短摘要，用于比较两次输入是否指向同一目标，不是登录凭据。`paymentAllowed` 固定为 false。

## 测试

```sh
go test -count=1 ./internal/sessionrestore ./cmd/membership-session
python3 -m unittest discover -s tools/session-desktop -p 'test_*.py'
```

测试使用完全合成的 Session 和浏览器替身，不将本地通过写成真实账号已上号。真实验收需要用户运行工具并确认身份结果，之后才能将该步骤标为业务成功。

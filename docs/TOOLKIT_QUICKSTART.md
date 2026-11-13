# 会话与付款前审核工具包

## 先使用哪个文件

Windows：解压整个包，双击 **打开会话工具.cmd**。需要已安装带 Tkinter 的 Python 3；程序不会安装依赖、修改浏览器设置或接管你正在使用的浏览器。

Linux：运行 `sh open-session-tool.sh`。没有图形桌面时，可以直接使用 `bin/` 中的 CLI。

界面中的 Session 输入框初始为空。不要把真实 Session 填进示例文件、命令行参数或聊天记录。

## 验收登录这一步

1. 在界面中选择本机 Chrome/Chromium 程序路径；通常会自动找到常见安装位置。
2. 粘贴完整 Session，勾选“显示独立浏览器”。
3. 直接点击“恢复登录并核验”。程序会先检查输入，再启动浏览器，无须先点另一个按钮。
4. 程序新建独立临时环境，恢复已有 Cookie、回读，然后打开页面核对账号。如输入含用户 ID，会同时核对 ID 与邮箱。
5. 只有最终代码是 `SESSION_VERIFIED`，且 Cookie 回读、远端身份、账号匹配、临时环境清理全部通过，进程正常退出，才算工具这次任务完成。

“仅检查输入”通过、Cookie 写入成功、浏览器打开成功，都不等于已上号。真实 Session 必须包含 `sessionToken`；只有 `accessToken` 不适用于这个恢复路径。

如果想先做不联网的格式检查，可以单独点击“仅检查输入”。每次启动任务都会清空输入框，之后要恢复登录时需重新粘贴 Session。

显示浏览器选项只供观察。核验后最多观察五分钟，再关闭临时浏览器；它不会把账号永久留在你的常用浏览器里。取消按钮会请求正常结束并清理临时环境。

本包尚未通过真实 ChatGPT 账号验收。当前受管工具的站点访问限制没有改变；本包供你在自己有权使用的环境中操作和验收。

## CLI 入口

所有 Session 都通过 stdin 传入；以下文件名表示你自己的输入来源，程序不会替你保存 Session 文件。

```text
bin/membership-session --inspect < your-session.json
bin/membership-session --restore --chrome-path PATH --show-browser --hold-open 2m < your-session.json
```

Windows 使用对应的 `.exe`。也可以让其他程序直接向 stdin 管道写入 JSON。省略 `--show-browser` 是隐藏运行，但目标站点可能拒绝这种运行方式；不能仅凭开关保证登录成功。

## 审核最终金额

`membership-review` 是独立的离线金额审核器：

```text
bin/membership-review --example > quote-example.json
bin/membership-review < quote-example.json
```

示例是合成 Go/EGP 数据，**不是实时售价**。审核器检查套餐、国家、币种、预算、折扣和税额关系。输入必须为十进制字符串，输出使用整数最小单位，避免把小计当成总价或把含税价格再加一次税。

结果 `reviewReady:true` 只表示输入的金额关系成立；`remoteVerified:false` 与 `paymentAllowed:false` 始终保留。它不会从网站自动读取报价，也不会付款。

## 现有付款前观察工具

`membership-preflight` 保留原有非付款入口。Go/EGP 目前是用户手工登录并进入结账页后的观察模式，真实 DOM 适配仍未验收。不能将它理解成自动 Go 充值。

## 油猴完整替换文件

`userscript/session-login.user.js` 是 0.1.1 完整脚本。全选替换旧脚本内容后保存，不要只在截图附近补括号。它会直接展开版本面板；缺少 Cookie 能力时也会显示明确提示。

旧完整文件原本能够通过语法检查，因此截图中已安装副本的准确语法错误仍未确认。本版修复了可复现的界面启动缺陷，不宣称已修复用户安装副本里未知的所有编辑错误。

## 工具包边界

本包没有自动付款入口，不会开卡、加款、绑卡或修改续费。卡台可替换接口已经在项目源码中实现，生产工作流与此独立验收工具分开。完整进展见 `docs/SINGLE_FLOW_DEVELOPMENT.md`，账号工具说明见 `docs/SESSION_RESTORE.md`。

真实账号材料只进入运行中的进程和临时浏览器 profile；正常退出后清理。系统强制终止或断电可能留下临时文件，所以不能承诺操作系统层绝无磁盘痕迹。

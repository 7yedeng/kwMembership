# 两份原版运行环境

安装目录：`%LOCALAPPDATA%\kwMembership-upstream-20261114`

这次保留作者原版源码，安装与测试记录在 `reports`。两个 Git checkout 均没有合入此前 fork 的业务改动。外层 Verify/Install 文件是此次新增的安装核验包装器。

## kwMembership 作者原版

- 源码：`source`，固定 `varrge/kwMembership@16ceb97fc15f629c8b59df242e1baee2227e4c77`。
- Windows worker：`source\bin\kwmembership-worker`，按原版 build.sh 构建。
- 独立 Python：`venv\Scripts\python.exe`，Python 3.13.5，Playwright 1.61.0。
- 已通过原版 Python 8 项测试、pip check 和补充空白页浏览器启动检查。
- 浏览器文件复用 `%LOCALAPPDATA%\kwMembership-playwright-20261114\browsers`，版本 Chromium 149.0.7827.55；Verify 自动设置路径。

双击本目录的 **Verify.cmd** 可复查。它不领取任务，不开始结账。终端中需要自动退出时：

```bat
Verify.cmd --no-pause
```

**完整 worker 尚不可启动：缺少作者的 kwRedeem 配套。** 原版 `scripts/check.sh` 已实际运行并报缺少 kwRedeem checkout。`.env` 是原版模板，fixture、付款关闭、密钥仍是占位值。

原版要求先取得真正的 kwRedeem 源码并执行其数据库初始化。不能用空目录、假的 package.json 或散落测试表代替。不要把 worker 的 `--help` 或 Python `--once` 当环境检查：前者会进入正常 worker 路径，后者会领取任务。

原版 live payment 会在填表后继续走提交许可与点击；它没有默认“仅填表停住”的通用开关。原版 live preflight 不填写卡资料。

## Zovo 官方填表脚本

- 源码：`zovo-official\source`，固定 `zovocard/zovo_card_cdk_auto@19f41ef347cdb3cce52fde0f2b43e12f70dc6ebe`。
- 工程：`zovo-official\source\automation`，Node 24.15.0，按 lockfile 的 npm ci 安装 Playwright 1.60.0。
- 已通过 9 个 JS 语法检查、无输入停止检查、系统 Chrome 156.0.8078.80 的补充空白页启动检查。

双击 **zovo-official\Verify.cmd** 可复查语法、依赖版本和无输入停止；不执行自动填表。官方原版的无参数命令会提示缺少 Session 或支付链接并退出 1，这是预期结果。

官方脚本默认不点击最终支付，但默认生成随机测试卡和随机账单；`--card` 仅替换卡号，有效期/CVC 写死，且日志会打印敏感材料。因此尚未接好真实卡和对应真实账单。Session 入口依赖的 `/api/v1/admin/checkout/generate` 在这个固定提交的后端中没有找到。

默认 Chrome 查找只包含 Linux/macOS 路径；已有 Playwright launch 分支对应系统 Chrome channel 的启动已验证，但没有验证真实网站表单。

## 真实页面的状态

此次没有读取真实 ChatGPT 账号页、执行真实填单、点击付款或改变订阅。

host 报错已在本机官方扩展 1.26.1102.11740 静态代码中定位到 hostUrl / assertBrowserUrlAllowed 的 URL 权限检查链路；尚未取得账号实际生效的动态策略。源码部署成功不代表页面权限恢复，切换驱动也不能作为恢复证据。

详细来源、哈希和边界见仓库 `docs/UPSTREAM_REPLICATION.md`，以及本机 `reports\runtime-verification.json` 和 `zovo-official\reports\installation.json`。

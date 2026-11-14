# 作者原版与 Zovo 官方填表脚本复刻

核验日期：2026-11-14。目标是先安装、核对现有实现，再依据实际缺口改进。本轮未修改两个上游 checkout 的业务源码，未带入 fork 的资格判断或填表改动。

`scripts/upstream/*.py` 和 `.cmd` 均为本轮新增的外层安装、核验包装器，不属于两份上游源码；安装目录中的两个 `source` checkout 保持原版不变。

## 1. 两份来源分别是什么

| 实现 | 固定上游 | 性质 |
| --- | --- | --- |
| kwMembership | `varrge/kwMembership@16ceb97fc15f629c8b59df242e1baee2227e4c77` | 第三方作者的 Go worker + Python Playwright 执行器；不是 OpenAI 官方充值 SDK |
| Zovo automation | `zovocard/zovo_card_cdk_auto@19f41ef347cdb3cce52fde0f2b43e12f70dc6ebe` | Zovo 卡台官方仓库中的独立 Node 自动填表实验目录；不是 OpenAI 官方充值 SDK |

已实时通过 GitHub 插件分别核对上游 `main` / `master`，仍为上述固定提交。

## 2. Windows 安装位置

统一目录：`%LOCALAPPDATA%\kwMembership-upstream-20261114`。

| 相对路径 | 内容 |
| --- | --- |
| `source` | kwMembership 原版 Git checkout，88 个 tracked 文件 |
| `source\bin\kwmembership-worker` | 上一轮原版 `scripts/build.sh` 生成的 Windows PE，已回读校验 |
| `venv` | 本轮独立安装的 Python 3.13.5 环境、原版 requirements 的 Playwright 1.61.0 |
| `Verify.cmd` / `Verify-Runtime.py` | 本轮新增的部署验收包装器，不属于作者原版；不启动 worker、不领取任务 |
| `reports` | 原版测试、Python 帮助、依赖检查、配置检查与运行报告 |
| `zovo-official\source` | Zovo 固定提交的独立原版 checkout |
| `zovo-official\source\automation` | 按原版 lockfile 安装的 Node 填表工程 |
| `zovo-official\reports` | Zovo 安装、语法检查、缺输入退出及浏览器运行记录 |
| `zovo-official\Verify.cmd` | 本轮新增离线复查入口，只检查语法、版本与无输入停止，不启动填表 |

Python 环境独立于旧 fork 环境；补充启动检查复用了已安装的同版本浏览器文件：`%LOCALAPPDATA%\kwMembership-playwright-20261114\browsers`。该路径通过 `PLAYWRIGHT_BROWSERS_PATH` 传给 Playwright，不复制旧 fork 的 Python 源码。若移动或删除旧浏览器目录，需要重新安装/指定浏览器路径。

本轮只复制原版 `.env.example` 为 `.env`，保留 `fixture`、`LIVE_PAYMENT_ENABLED=false` 和示例密钥；没有配置业务密钥或创建业务数据库。

## 3. kwMembership 实际验收

证据：[Windows 运行报告](evidence/upstream-windows-runtime-20261114.json)。

| 项目 | 结果 |
| --- | --- |
| 原版固定提交及工作树 | `16ceb97`，88 文件，clean |
| Go 原版二进制 | Windows PE；内含版本字符串 `go1.26.11`；原版 build.log 记录 `built bin/kwmembership-worker (16ceb97)` |
| 二进制 SHA-256 | `375380d8560f64cdfe6e43d7719bfb1b3e5265d4aad8e8dbc75e60aa2144cada` |
| Python / Playwright | 3.13.5 / 1.61.0，`pip check` 通过 |
| 作者原版 Python 测试 | 8 项通过，0 失败，0 跳过 |
| 原版 Python CLI | `python -m python_executor --help` 通过 |
| 补充浏览器环境检查 | Chromium 149.0.7827.55，独立 profile，仅 `about:blank`，关闭和清理通过 |
| 原版 `scripts/check.sh` | 退出 1，明确 `KAWANG_PROJECT_ROOT is not a kwRedeem checkout` |
| 完整 worker / 真实充值 | 未启动、未验收 |

原版仅有 `test_client.py`（3 项）和 `test_live.py`（5 项）。此前 fork 的 46 项、浏览器重启脚本和真实本地 DOM 测试不计入这次原版结果。原版本身没有真实浏览器填表测试；本轮的空白页检查只是运行环境检查。

另在 Linux 独立原版 worktree 做了一次 Go 离线测试：4 包的 52 项顶层测试（含子测试 124 条 pass）通过，0 断言失败/跳过；其余 5 包因固定依赖未缓存、`GOPROXY=off` 无法编译，总退出码 1。此结果没有算作 Go 全量通过，也不否定 Windows 已成功构建的证据。见 [Go 离线汇总](evidence/upstream-go-offline-20261114.json) 和 [错误日志](evidence/upstream-go-offline-20261114.log)。

### 完整原版仍缺 kwRedeem

[上游 README](https://github.com/varrge/kwMembership/blob/16ceb97fc15f629c8b59df242e1baee2227e4c77/README.md) 要求先在配套 `kwRedeem` 执行 `npm install` 和 `npm run db:init`，然后共享其 SQLite/WAL、`JWT_SECRET` 和配置。

本轮 `varrge/kwRedeem` 仓库请求仍返回 404，仓库名搜索没有得到可访问的同名项目。该结果不能区分私有、删除或未公开。会员仓库不包含其初始化脚本或完整迁移，不能用测试 DDL 或假的 `package.json` 代替作者配套。

需要的是完整 `kwRedeem` checkout/授权访问或其真实本机路径与初始化实现；当前缺口不是新的 Session 或银行卡材料。

### 原版入口的实际行为

- `scripts/check.sh` / worker 的单个 `--check` 参数才是配置检查，不领取订单；若打开已有 SQLite，`store.Open` 会配置 WAL，并非严格只读数据库审计。
- worker 不支持 `--help` / `--version`：这些参数会落入普通启动路径，不能作自检。
- Python `--help` 是正常帮助；Python `--once` 会领取并执行任务。
- fixture 仅限制 Python 的无扣款执行，不代表整个 Go worker 只读。普通 worker 会推进 intake、库存、资格、付款状态与对账。
- 原版 live preflight 建立并检查 checkout，不填卡；live payment 会填表并继续取得 permit/执行提交。不存在一个默认的“原版 payment 填完就停”开关。

## 4. Zovo 官方脚本的实际边界

本轮实际安装和运行结果见 [Zovo 原版运行报告](evidence/zovo-original-runtime-20261114.json)：Node **24.15.0**，按 lockfile 安装 Playwright **1.60.0**；`npm ci`、`npm ls` 和原版 **9 个 JS 语法检查通过**；无参数入口按预期退出 1，发生在浏览器启动前；系统 Chrome channel 的补充空白页启动检查通过，版本 **156.0.8078.80**。固定提交、lockfile 及源码工作树保持不变。实际业务运行状态为 `NOT_RUN`。

原版 `automation/package.json` 范围为 Playwright `^1.48.0`，lockfile 锁定 **1.60.0**。复刻采用 `npm ci`，不升到 Python 工程的 1.61.0。它没有 `npm test` 或独立 `--help` 实现。

安装和环境验收只运行原版依赖安装、9 个 JavaScript 的 `node --check`、无参数入口以及补充 `about:blank` 检查。没有运行 `_advance.js`、`_fillcard.js` 等调试动作；它们会连接并操作浏览器，不能当离线自检。

| 项目 | 原版源码行为及缺口 |
| --- | --- |
| 是否依赖 kwRedeem | automation 安装及已有 URL 入口不依赖它 |
| 默认最终提交 | 默认 `submit=false`；显式 `--submit` 才走最终支付点击 |
| 默认卡资料 | 随机 Luhn 合法测试卡号、有效期和 CVC |
| `--card` | 只替换 PAN，其他字段固定为 12/30 和 123，不能视为接入完整真实卡资料 |
| 账单 | 默认随机生成美国免税州资料；没有完整真实账单参数 |
| 日志 | 会输出完整卡资料、账单、支付链接；本轮未传入这些材料 |
| Session 取链 | 请求 `/api/v1/admin/checkout/generate`；当前固定提交的后端未找到这个注册路由/handler，不能声称同仓后端启动后全流程可用 |
| 已有 URL 入口 | 不调用上述后端；同时给 Session 参数也不会解析/注入该 Session，因为整个分支被跳过 |
| Windows 默认浏览器发现 | 默认 spawn 路径只枚举 Linux/macOS；原版 `--pw-launch` 对应的系统 Chrome channel 已通过本轮 `about:blank` 启动检查，真实填表未验收。不能将此检查当作默认 spawn 路径已兼容 |
| 填表反馈 | 某字段失败仍可能输出“完成”；没有完整材料、金额和账号核对后才提交的严格检查 |

这些发现用于确定后续复用边界；本轮保留原版源码，不把测试材料用于真实付款，也没有调用被拒绝的真实页面。

## 5. host 问题与继续条件

详见 [本机扩展检查](EXTENSIONS_AND_HOST_ACCESS.md#2026-11-14本机已安装扩展的静态证据)。当前直接证据已经把拒绝路径定位到 ChatGPT 浏览器运行端的 `hostUrl` 检查及官方扩展中的 ChatGPT 页面策略。仍未测量账号实际生效策略，无法区分明确拒绝、配置失败后的默认值或其他异常。

部署、编译和离线测试可以继续；真实页面操作必须先通过受支持的权限路径恢复。切换 Playwright/CDP 不构成恢复凭证。本轮没有重试真实页面，没有提交付款或改变订阅。

下一步可复核的原版缺口分别是：kwMembership 的完整 kwRedeem 配套、Zovo Session 取链后端接口、真实卡及对应账单输入和日志处理、Windows 浏览器入口，以及真实页面许可与单笔业务验收。不要把这几个问题合并成一个“Playwright 不行”。

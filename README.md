# kwMembership

> **7yedeng 自有付款引擎研究分支（2026-11-13）**：本分支增加独立无付款预检 CLI、按卡绑定的账单资料、账号服务注入和支付执行修复。请先阅读 [自有引擎实现与验收](docs/INDEPENDENT_ENGINE.md)、[独立预检与数据库契约](docs/STANDALONE_CONTRACT.md)、[账号与浏览器测试顺序](docs/ACCOUNT_BROWSER_RUNBOOK.md) 和 [验证记录](docs/VALIDATION.md)。当前默认 worker 仍采用下文的 kwRedeem 配套方式；本分支尚不构成脱离配套系统即可实际付款的成品。

新增：[Go 套餐非付款预检与埃及 EGP 契约](docs/GO_PLAN_PREFLIGHT.md)、[只读 worker 数据库契约检查](docs/SCHEMA_CHECK.md)、[服务器会话隔离与 IPC 审计](docs/SERVER_ISOLATION_AND_IPC.md)、[IPC 创建失败与替代方案复核](docs/IPC_DIAGNOSIS_FOLLOWUP.md)。Go 仅开放手动进入页面后的观察路径，EGP 必须显式配对 EG；默认 worker 的 Go 自动付款尚未实现。

独立的 Go 会员自动化 worker。代码与 kwRedeem 分开维护，但直接读取和写入 kwRedeem 的 SQLite/WAL 数据库，不创建第二套会员数据库。

Go worker 负责：

- 自动发现会员订单、校验 Session，并幂等创建履约；
- 会员资格检查、账号串行锁、库存同步与卡片选择；
- 为每个付款阶段签发不可变命令、材料领取和危险动作 Permit，并通过本机协议交给 Python 执行器；
- 资金准备、付款状态推进、未知结果对账与续费保护；
- 将状态、错误码、版本、心跳和 Tick 直接写回 kwRedeem 数据库。

kwRedeem 的 Node API 保留客户入口、Session 恢复、后台展示、配置和 Webhook。Go 是唯一工作流所有者；Python 只串行执行 Go 已授权的浏览器步骤，不能写业务状态或决定重试。当前默认仅启用无扣款 fixture，真实付款保持关闭。

## 本地运行教程

先初始化 kwRedeem 数据库，并确认它自己的 `.env` 已配置 `DATABASE_PATH` 和非默认 `JWT_SECRET`：

```bash
cd /Users/varrge/workspace/kwRedeem
npm install
npm run db:init
```

只有使用 `legacy-go` 诊断模式时才需要 Go 直接发现 Chrome。默认 Python fixture 不启动付款浏览器；live 模式由 Python Playwright 启动独立浏览器：

```bash
google-chrome --version || chromium --version
```

然后构建 Go worker：

```bash
cd /Users/varrge/workspace/kwMembership
cp .env.example .env
# 编辑 KAWANG_PROJECT_ROOT；当前同级目录默认值 ../kwRedeem 可以直接使用
bash scripts/build.sh
bash scripts/check.sh
bash scripts/start-worker.sh
KWMEMBERSHIP_EXECUTOR_SECRET=<与 .env 相同的随机密钥> python3 -m python_executor
```

`scripts/check.sh` 会打开配置的 kwRedeem 数据库，并检查 `VerifySharedSchema` 列出的 17 个表名是否存在；这不等于完整验证所有业务表、列、索引或生产迁移。只有 `legacy-go` 模式会启动一次 `about:blank` Chrome 自检。检查不会获取订单或执行付款。

启动 kwRedeem API 后，进入“会员履约 → 实施状态”，应看到：

```text
运行主体：go
状态：运行中
最近心跳：持续更新
最近 Tick：持续更新
```

## 数据与密钥

kwMembership 的 `.env` 只需要指向 kwRedeem：

```dotenv
KAWANG_PROJECT_ROOT=../kwRedeem
KWMEMBERSHIP_CHECKOUT_EXECUTOR=python
KWMEMBERSHIP_EXECUTOR_LISTEN=127.0.0.1:4312
KWMEMBERSHIP_EXECUTOR_SECRET=<至少 32 位的独立随机密钥>
KWMEMBERSHIP_PYTHON_EXECUTOR_MODE=fixture
KWMEMBERSHIP_LIVE_PAYMENT_ENABLED=false
```

Go 会从该目录的 `.env` 读取：

- `DATABASE_PATH`：同一个 `data/kawang.db`；
- `JWT_SECRET`：解密 kwRedeem 已保存的 Session、Provider 凭据和资金意图；
- `MAINTENANCE_PATH`、`WORKER_POLL_MS`、`DEFAULT_REQUEST_TIMEOUT_MS`、`API_URL`。

Go/Python 私有协议只监听回环 IP，并使用独立 Bearer 密钥。命令表不保存 Session、Cookie、PAN、CVV、付款链接或原始页面内容。

`npm run db:init` 会在共享数据库中持久化首次启用 Go Intake 的时间水位线。Go 只自动接收水位线之后创建的新会员订单；历史订单不会在部署时批量重跑，需要时仍由 kwRedeem 后台逐单补建。

## 生产部署

建议将两个项目放在同一台服务器，例如：

```text
/opt/kwRedeem
/opt/kwmembership
```

在 kwMembership 的 `.env` 使用绝对路径：

```dotenv
KAWANG_PROJECT_ROOT=/opt/kwRedeem
KWMEMBERSHIP_CHECKOUT_EXECUTOR=python
KWMEMBERSHIP_EXECUTOR_LISTEN=127.0.0.1:4312
KWMEMBERSHIP_EXECUTOR_SECRET=<至少 32 位的独立随机密钥>
```

使用与 kwRedeem 相同的 Unix 用户安装，以保证两边可以安全访问同一个 SQLite/WAL 文件：

```bash
sudo KWMEMBERSHIP_USER=<kwRedeem运行用户> bash scripts/install-systemd.sh
```

安装器会创建 `kwmembership-worker.service` 和串行消费者
`kwmembership-python-executor.service`，两者读取同一份受限环境文件。更新时执行：

```bash
sudo bash scripts/update.sh
```

```text
https://apikey.vsakura.top/api/webhooks/spacexcard/card-transactions
```

付款 Gate 默认关闭。新适配器版本为 `python-session-card-checkout-v1`。Go 先检查订阅；发现自动续费时取消并重新查询，只有明确的 `free + auto-renew=false` 才继续。之后 Go 选择卡片、写资金边界、签发命令和 Permit；Python 通过本机协议一次领取 Session 和卡材料并返回结构化事实。`fixture` 只处理无扣款 preflight，遇到 payment 命令会以 `FIXTURE_PAYMENT_DISABLED` 失败关闭；`live` 适配器已实现受限浏览器协议，但仍需要显式设置 live 模式和上线 Gate、安装并验证浏览器依赖，且不会绕过 Go 的付款 Gate、Permit 或未知结果对账。

Live 执行器依赖 Playwright。只在专用执行器主机安装依赖，不要在开发机打开真实付款：

```bash
python3 -m pip install -r python_executor/requirements.txt
python3 -m playwright install chromium
```

### 私有虚拟显示与安全验证

生产服务器使用 Xvfb 是因为 ChatGPT 对同一份有效 Session 接受普通 Chrome、但拒绝严格 headless Chrome。这个模式仍然是无人值守自动化：正常流程不需要管理员打开窗口，也不需要账号密码。Xvfb、x11vnc 和 noVNC 必须只监听服务器本机，不能把 5900/6080 端口暴露到公网。

```dotenv
KWMEMBERSHIP_VISIBLE_BROWSER=true
KWMEMBERSHIP_HUMAN_CHALLENGE_TIMEOUT_MS=300000
```

```bash
ssh -L 6080:127.0.0.1:6080 root@服务器地址
# 本机浏览器打开：
# http://127.0.0.1:6080/vnc.html?autoconnect=true&resize=scale
```

只有 Go 明确认出安全验证页并写入 `CHECKOUT_CHALLENGE_WAIT` 时，管理员才可通过 SSH 隧道访问 noVNC 完成验证。Go 只在重新识别到受支持的结账结构后继续；它不会自动绕过验证，也不会因虚拟显示模式削弱付款 Gate、页面许可或对账边界。

## 自动流程

```text
订单 Session
→ Go 校验账号与库存
→ Go 取消已有自动续费并重新确认 free + auto-renew=false
→ Go 获取卡资料与账单地址，写入串行任务
→ Python 一次领取 Session 和卡材料并校验 Plus / PH / PHP 页面合同
→ 每次可能产生授权的点击前写入 Permit 并快照卡交易
→ 提交后只进入对账，不会因超时盲目重提
→ 确认会员与卡交易后取消自动续费并写回后台
```

每次执行和人工挑战共用固定五分钟硬截止，heartbeat 不能延长。遇到 CAPTCHA、3DS、短信或银行验证时会创建人工介入状态，不尝试绕过安全验证。

### 实验性免托管密码登录预检

只有经管理员明确选中的单笔履约可以进入 `CHECKOUT_LOGIN_READY`。Go 会打开使用独立临时 profile 的可视化窗口并写入 `CHECKOUT_LOGIN_WAIT`；管理员通过上面的 SSH + noVNC 通道直接登录 ChatGPT，并手动进入 Plus 结账页。账号密码、MFA、Passkey 和浏览器 Cookie 不进入 kwRedeem、kwMembership 或普通日志。脱离此共享数据库的诊断入口见 [独立预检 CLI](docs/STANDALONE_CONTRACT.md)。

Go 只在浏览器内读取当前登录邮箱并与订单身份进行内存比对，然后执行既有的 Plus / PH / PHP / 价格区间和结账结构预检。成功后停在 `CHECKOUT_LOGIN_PREFLIGHT_PASSED`，不会进入 `FUNDING_READY`，不会读取卡资料、准备资金或授权支付。窗口关闭后登录上下文即销毁；该实验结果不能用于生产付款或 Canary 资格。

# kwMembership

独立的 Go 会员自动化 worker。代码与 kwRedeem 分开维护，但直接读取和写入 kwRedeem 的 SQLite/WAL 数据库，不创建第二套会员数据库。

Go worker 负责：

- 自动发现会员订单、校验 Session，并幂等创建履约；
- 会员资格检查、账号串行锁、库存同步与卡片选择；
- 调用 SpaceXCard 获取 Checkout 地址、Cookie、卡资料，并控制无界面 Chrome 完成结账；
- 资金准备、付款状态推进、未知结果对账与续费保护；
- 将状态、错误码、版本、心跳和 Tick 直接写回 kwRedeem 数据库。

kwRedeem 的 Node API 只保留后台展示、配置和 Webhook。会员自动化不依赖浏览器扩展，也不需要人工打开浏览器。

## 本地运行教程

先初始化 kwRedeem 数据库，并确认它自己的 `.env` 已配置 `DATABASE_PATH` 和非默认 `JWT_SECRET`：

```bash
cd /Users/varrge/workspace/kwRedeem
npm install
npm run db:init
```

本机需要安装 Google Chrome 或 Chromium。macOS 默认会自动发现 `/Applications/Google Chrome.app`；Linux 可检查：

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
```

`scripts/check.sh` 会确认它打开的是 kwRedeem 数据库、会员表完整，并实际启动一次 `about:blank` 无界面 Chrome 自检；不会获取处理租约、调用外部 Provider 或执行订单。

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
# 自动发现失败时填写：
# KWMEMBERSHIP_CHROME_PATH=/usr/bin/google-chrome
```

Go 会从该目录的 `.env` 读取：

- `DATABASE_PATH`：同一个 `data/kawang.db`；
- `JWT_SECRET`：解密 kwRedeem 已保存的 Session、Provider 凭据和资金意图；
- `MAINTENANCE_PATH`、`WORKER_POLL_MS`、`DEFAULT_REQUEST_TIMEOUT_MS`、`API_URL`。

因此不需要 `SERVICE_TOKEN`、扩展 Token、绑定扩展、回调地址、第二套数据库或数据迁移。

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
# Debian/Ubuntu 常见路径；以实际安装为准
KWMEMBERSHIP_CHROME_PATH=/usr/bin/google-chrome
```

使用与 kwRedeem 相同的 Unix 用户安装，以保证两边可以安全访问同一个 SQLite/WAL 文件：

```bash
sudo KWMEMBERSHIP_USER=<kwRedeem运行用户> bash scripts/install-systemd.sh
```

安装器只创建 `kwmembership-worker.service`。更新时执行：

```bash
sudo bash scripts/update.sh
```

SpaceX Card 交易 Webhook 仍指向 kwRedeem：

```text
https://apikey.vsakura.top/api/webhooks/spacexcard/card-transactions
```

付款 Gate 默认关闭。新执行器版本为 `go-headless-v1`；旧的 `checkout-v1` 扩展资格和自动范围不会被复用。先完成无扣款预检，再为 `go-headless-v1` 创建 Canary 与自动范围，最后从 kwRedeem 后台启用。

## 自动流程

```text
订单 Session
→ Go 校验账号与库存
→ Go 获取一次性 Checkout 地址和 Session Cookie
→ Go 获取 SpaceXCard 卡资料与 kwRedeem 账单地址
→ Go 无界面 Chrome 严格校验套餐、PH/PHP 价格和白名单控件
→ 每次可能产生授权的点击前写入 Permit 并快照卡交易
→ 提交后只进入对账，不会因超时盲目重提
→ 确认会员与卡交易后取消自动续费并写回后台
```

遇到 CAPTCHA、3DS、短信或银行验证时，Go 会停止自动点击并在后台创建人工介入状态；它不会尝试绕过安全验证。

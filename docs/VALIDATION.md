# 本分支验证记录

日期：2026-11-13。验证对象为 `research/independent-executor-20261113` 分支的独立预检、按卡账单绑定、自有依赖接口与付款执行修复。测试使用合成 Session、合成卡资料、本地 HTTP/TLS/CDP 测试服务器和临时 SQLite，不使用真实账户或资金。下表记录本地执行结果；GitHub runner 的真实浏览器结论以对应 [Actions run](https://github.com/7yedeng/kwMembership/actions) 为准，不把本地 skip 记作通过。

## 1. 已执行的验证

| 验证 | 命令 | 结果 |
| --- | --- | --- |
| Go 全量测试 | `go test -count=1 ./...` | 新增 schema-check 命令与包后共 13 个 package，全部通过；使用 Go 1.26.11。 |
| Python 与填表 JS 本地测试 | `python3 -m unittest discover -s python_executor/tests -v` | 共 59 项：46 项实际通过，13 项真实浏览器测试明确 skip；默认官方 headless shell 未安装。其中 7 项套餐/地区脚本测试执行真实生产常量，全部使用本地合成 fetch/DOM。 |
| GitHub 浏览器门禁 | `.github/workflows/offline-validation.yml` | 在独立 runner 安装固定版本 Playwright 的官方浏览器；`KWMEMBERSHIP_REQUIRE_BROWSER_TESTS=1`，缺浏览器和任何 skip 都令 job 失败。此行说明门禁，实际成功/失败见 Actions 日志。 |
| 完整 Chromium / Xvfb 门禁 | `TestVerifyChromeHeadedRuntime` | 独立 job 检查 runner 原有正式 Chrome，非 root、正常 sandbox 调用生产 `VerifyChrome`，只访问 about:blank；精确测试名必须产生 pass 事件。普通本地未配置时明确 skip，required 模式缺配置必须失败。首次下载构建的 sandbox 失败及后续安装方式区别见浏览器运行验收文档，真实启动结果见对应 Actions 日志。 |
| 补丁空白/冲突标记检查 | `git diff --check` | 通过。 |
| 研究清单校对 | 对照非测试 Go SQL 引用、预检列表、文档相对链接 | 35 张业务表集合与源码一致；17 项预检标记一致；源码链接有效。 |
| 相关仓库差异 | 对固定版本的两个 `automation` 目录比较文件集合与 SHA-256 | PPCDK 与 ZovoCard 的该目录均为 12 文件，全部逐字节相同。 |

Go package 范围：`cmd/membership-worker`、`cmd/membership-preflight`、`cmd/membership-schema-check`、`internal/preflight`、`internal/schemacheck`、`internal/checkout`、`internal/config`、`internal/domain`、`internal/executor`、`internal/processor`、`internal/provider`、`internal/secure`、`internal/store`。

首批提交的 Python 结果为 32 项通过、0 skip；加入真正的浏览器 suite 后不能继续沿用这个数字。当前本地 `OK (skipped=13)` 不表示真实浏览器测试已通过。

## 2. 有业务意义的回归场景

### 账号与账单服务

- 自有账号查询/取消失败时不会调用旧第三方服务。删除测试数据库里的旧 token 表后，自有取消与重新观察仍可运行。
- 缺失布尔值、未知套餐、缺失付费币种/到期证据、陈旧观察不会被推断成 free；付费档位即使过期也不会自动变成 free。
- Session 与观察到的邮箱不匹配时进入订单恢复处理，不触发全局会员服务熔断。
- 取消续费后重新观察，拒绝取消前的缓存；普通查询错误不会把失败的半开探测记为成功。
- 账单配置缺必填字段或包含控制字符时在执行前报错；返回内容来自不可变 profile，错误不包含账单值，取消的 context 不返回资料。
- 卡详情在同一次 GET 中保留姓名和完整/缺失/部分地址，包括 Line2；只有姓名不会被解释成完整地址。
- 按平台配置 key 和数字卡 ID 精确取配置，不能跨卡或跨平台借用。完整 issuer 资料优先，缺失项才补齐；已知字段冲突立即停止。
- checkout 接线测试使用临时 SQLite、内存 HTTP transport 与 executor 替身，验证一次卡详情请求的地址一直进入同一请求的付款 Material，且仍带原动作 Guard。该测试不调用 Guard 或点击控件。
- 本地 bridge 材料领取集成测试验证 Line2 和 GB 账单国家保留，价格契约仍为 PH/PHP。

代码：[account_service_test.go](../internal/processor/account_service_test.go)、[configured_billing_test.go](../internal/provider/configured_billing_test.go)、[card_billing_test.go](../internal/provider/card_billing_test.go)、[card_bound_billing_test.go](../internal/provider/card_bound_billing_test.go)、[processor/card_billing_test.go](../internal/processor/card_billing_test.go)、[material_billing_test.go](../internal/executor/material_billing_test.go)。

### 独立预检入口

- 默认 `--check` 和 `--inspect-session` 不构造浏览器，不加载 worker 配置或数据库；本地通过始终保留 `remoteVerified=false`。
- 拒绝封装和价格契约的未知字段、尾随 JSON、超大输入、身份不一致；只有 accessToken 时不伪造 session cookie。
- `--run` 的 fake executor 断言仅使用 `ModeSessionPreflight`，卡材料、Guard、OnHandoff、Binding 均为空；出现 progression 或 submit 报告则失败。
- `--interactive-preflight` 只接受严格元数据，在 headed 独立 profile 中等待用户直接登录，不接收 Session。离线测试只验证请求和 callback 契约，没有访问真实站点。
- 所有错误和输出通过白名单；不回显未知 code、邮箱、token、完整 URL 或底层异常。CLI 专用 browser logger 禁用原始 CDP 异步日志；本地模拟 CDP 服务验证异常事件不会旁路泄露。
- stdin 等待读取支持 context 取消，SIGINT/SIGTERM 不因等待输入 EOF 而挂住。测试使用 pipe 和合成材料。

代码：[main_test.go](../cmd/membership-preflight/main_test.go)、[preflight_test.go](../internal/preflight/preflight_test.go)。完整输入与模式说明：[STANDALONE_CONTRACT.md](STANDALONE_CONTRACT.md)。

### Go 套餐与执行入口隔离

- Go 仅进入无凭据、无卡材料、无 Guard/Binding 的交互预检；Session CLI 会在构造浏览器前拒绝 Go。
- 明确的 Go 与 Plus 页面事实不能互相满足目标；金额必须为有限正数，且在契约区间内。
- Go 浏览器与 Python bridge 入口均按当前阶段核对价格契约，Go 契约不能伪装成 Plus 付款命令。x5/x20 订单先经过 Plus 阶段的原有语义保留。
- 交互身份响应含刷新错误时拒绝；账号来源上新观察失败会清除旧身份确认。
- Node VM 直接执行 Go/Python 两份 Plus 准备脚本：活跃 go/Go/GO 合成订阅的 POST 数为 0；合成免费账号对照仍只请求原 Plus SKU。
- Go 页面检查脚本只识别明确属性，未知或缺失值不会根据正文、标题或 URL 猜套餐。这不是线上 Go 页面适配证据。
- 埃及 Go 仅允许交互观察的 EG/EGP 明确组合；缺国家、错配、Plus 或自动执行模式均被拒绝。旧 PHP 省略 country 和显式 PH 均保留兼容，空 Country 不改变旧 worker JSON。
- 合成埃及页面的套餐、国家、币种、金额与契约分别比对；缺失值不从正文或 URL 补全。测试中的 193 不进入生产默认值，`paymentAllowed` 仍为 false。

代码：[go_preflight_test.go](../internal/checkout/go_preflight_test.go)、[Go 预检测试](../internal/preflight/go_test.go)、[埃及执行器契约测试](../internal/checkout/egypt_contract_test.go)、[埃及 CLI 契约测试](../internal/preflight/egypt_test.go)、[bridge 套餐测试](../internal/executor/plan_contract_test.go)、[test_plan_scripts.py](../python_executor/tests/test_plan_scripts.py)。

### 只读数据库契约检查

- 覆盖 35 张声明表、383 个声明列条目、6 个明确 UPSERT 唯一键 target；生产必需列与仅测试出现的列分开。
- 不存在文件不创建，空文件不初始化；只有 35 个表名而没有所需列的数据库不能通过。
- 只读层拒绝写入，检查前后文件字节、mtime、权限和目录项保持一致；WAL/SHM/journal 文件保留原样并拒绝检查。
- 不读取业务行，不输出输入路径、原始默认值、Session 或任意数据库标识；未知正式约束不误报为生产就绪。
- 部分/表达式/超集唯一索引、旧全局卡 ID 唯一键冲突、lease 类型与默认值不兼容均有回归用例。
- 复审后增加大写 lease 列名、非 SQLite 数值语法默认字符串、生产写入列被生成列替代的回归；314 个明确写入列的证据与类型来源独立声明，只读派生列不误报不可写。

完整证据与限制：[SCHEMA_CHECK.md](SCHEMA_CHECK.md)。

### 卡台连接

- 通过临时 SQLite 与合成加密凭据证明 `membership_card_platforms.base_url` 真正决定请求目的地址。
- 含凭据、query、fragment、不支持的路径或错误端口的 URL 在请求前拒绝。
- 本地 TLS 测试中的 301/302/307/308 不会将 API key 转发到第二个测试服务；拒绝重定向不被解释为确定无写入。
- 对照官方固定版本的余额契约，资金判断使用明确的 `spendable_balance`；覆盖保留额高于当前余额、子账户、不自行相减、缺失/空值/非法可花余额和旧响应兼容。

代码：[card_platform_config_test.go](../internal/processor/card_platform_config_test.go)、[spacex_config_test.go](../internal/provider/spacex_config_test.go)、[spacex_balance_test.go](../internal/provider/spacex_balance_test.go)。

### 支付点击与本地凭据

- 模拟第一次点击已经派发、随后超时：只调用一次 `click()`，上报 `unknown`，不会尝试另一个 selector/frame。
- unknown 上报也失败时仍不会再点击。只有点击前的定位失败才可以切换 selector。
- 带 userinfo 的伪回环 URL 被拒；301/302/303/307/308 均不跟随；环境代理收不到 executor/lease 凭据。
- executor 的提交报告保持兼容 wire status，但诊断状态只表示 `submitted`。

代码：[test_client.py](../python_executor/tests/test_client.py)、[test_live.py](../python_executor/tests/test_live.py)。

### 自动填表回读

Python 生产 `FILL_FRAME_JS` 和 Go 生产 `fillFrameFunction` 分别直接送入 Node VM；DOM 模拟实现输入框、选择框、事件处理器和帧渲染回调，并要求两套脚本结果一致，覆盖：

- 卡号空格、有效期格式化与等价的月份/年份选项；
- 不存在对应选项、输入事件清空值、渲染时替换节点；
- 原生 validity、ARIA invalid、存在但没有资料的可选地址字段；
- Python 在传入卡资料前排除不允许的 frame origin，JS 再检查当前 origin；
- 某 frame 的失败不能被另一个 frame 的成功字段集合掩盖；填表失败后不会申请点击；
- 无候选字段且不运行渲染回调的 frame 返回空字段集合，不阻塞、不提供填写成功证据。
- 地址第二行接受、截断和缺字段；非空 Line2 不可静默丢弃。
- 空州/邮编仅在页面字段非 required、非 aria-required 且 native validity 正常时接受，不伪造默认值。
- 渲染后新增字段重新验证，不因旧 fragment 缺少它而漏过。

测试输出只有字段名与布尔值，不回传卡号、CVC 或账单值。代码：[test_fill_dom.py](../python_executor/tests/test_fill_dom.py)、[fill_dom_double.cjs](../python_executor/tests/fill_dom_double.cjs)。

## 3. 明确未完成的验证

**当前本地完整 Chromium 的离线页面测试没有通过环境启动阶段。** 普通权限下 `socket(AF_UNIX, SOCK_STREAM/DGRAM)` 返回 EPERM，`socketpair` 和 AF_INET 回环 TCP 可用；完整 Chromium 在 ProcessSingleton 创建 Unix socket 时退出。此前扩大执行权限的申请被自动审批拒绝；本轮没有关闭或绕过宿主策略。官方 headless shell 的普通安装下载得到无效 ZIP，尚未取得可启动文件，不能把它写成复现了相同 IPC 故障。

本轮最小探测再次确认错误发生在 `socket()` 构造时，尚未调用 `bind()` 或 `listen()`。路径和抽象地址方案不能改变这一调用；DevTools TCP/pipe 与 ProcessSingleton 是不同通道。实际探测结果、源码依据和推论范围见 [IPC_DIAGNOSIS_FOLLOWUP.md](IPC_DIAGNOSIS_FOLLOWUP.md)。

“本地”在上述记录中指当前受管云命令执行器，不是用户 Windows 电脑。另行验收的共享云浏览器只打开空白页，HTTPS 示例未通过；用户本机 Chrome 的官方 TodoMVC 示例输入、提交、勾选与页面更新通过。两者均未导入账号。完整浏览器与 headless shell、三个浏览器运行面的区别见 [BROWSER_RUNTIME_ACCEPTANCE.md](BROWSER_RUNTIME_ACCEPTANCE.md)。

新增 [test_browser_fill.py](../python_executor/tests/test_browser_fill.py) 和 [browser_fill.html](../python_executor/tests/browser_fill.html) 包含 13 个真实浏览器测试方法。所有 HTTP 请求本地 fulfill 或 abort，fixture 响应必须有标记；未知 HTTP 请求必须被中止且不能取得响应。使用新 context、禁止 service worker，并配置关闭的回环代理防止意外外联。这些是合成 DOM 的真实浏览器测试，不是登录或支付网站实测。

CI 启动与依赖缺失必须失败；本地允许 skip 时会打印具体安装/IPC原因。已通过模拟缺失浏览器的强制门禁检查，证明 required 模式不会以 skip 获得成功。CI 不读取真实账号、卡或 repo secrets，也不上传浏览器 profile、Session、HAR 或页面截图。

Node DOM 模拟不能证明 Chromium/React/Stripe 的真实行为，也不能证明现有 selector、`data-kw-*` 或 meta 标记存在于当前线上页面。没有向真实页面注入自定义标记来制造契约成功。

本轮也没有验证真实账号订阅查询、取消续费、银行 3DS、实际开卡/卡充值、商户扣款、会员到账或真实未知结果恢复。完整 kwRedeem 配套 schema 未取得，本分支也未提供经验证的完整替代迁移，所以没有从空库独立启动完整业务的成功记录。

## 4. 如何复现离线验证

在仓库根目录使用满足 `go.mod` 的 Go 版本、Python 3 和 Node.js：

```bash
go test ./...
python3 -m unittest discover -s python_executor/tests -v
git diff --check
```

Python DOM 模拟测试在没有 Node.js 时会明确 skip；本轮运行环境有 Node.js，16 项填表 DOM 测试与 7 项套餐/地区脚本测试均实际执行。Node.js 仅用于这些测试，没有增加到付款执行器的生产依赖。

在支持浏览器的常规测试主机或 CI 上执行：

```bash
python3 -m pip install -r python_executor/requirements.txt
python3 -m playwright install --with-deps --only-shell chromium
KWMEMBERSHIP_REQUIRE_BROWSER_TESTS=1 python3 -m unittest discover -s python_executor/tests -v
```

这是合成离线页面验证；不应将账号或真实卡配置到此 workflow。账号站点访问权限与完整运行时要求见 [ACCOUNT_BROWSER_RUNBOOK.md](ACCOUNT_BROWSER_RUNBOOK.md)。

真实运行验收的代码、数据和证据缺口见 [INDEPENDENT_ENGINE.md](INDEPENDENT_ENGINE.md) 与 [DEPENDENCY_INVENTORY.md](DEPENDENCY_INVENTORY.md)。

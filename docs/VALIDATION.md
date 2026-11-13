# 本分支验证记录

日期：2026-11-13。验证对象为 `research/independent-executor-20261113` 分支的自有依赖接口与付款执行修复。测试使用合成 Session、合成卡资料、本地 HTTP/TLS 测试服务器和临时 SQLite，不使用真实账户或资金。

## 1. 已执行的验证

| 验证 | 命令 | 结果 |
| --- | --- | --- |
| Go 全量测试 | `go test ./...` | 9 个 package 全部通过；使用 Go 1.26.11。 |
| Python 与填表 JS 离线测试 | `python3 -m unittest discover -s python_executor/tests -v` | 32 项全部通过，0 skip；其中 11 项直接执行生产填表 JS 的 Node DOM 模拟测试。 |
| 补丁空白/冲突标记检查 | `git diff --check` | 通过。 |
| 研究清单校对 | 对照非测试 Go SQL 引用、预检列表、文档相对链接 | 35 张业务表集合与源码一致；17 项预检标记一致；源码链接有效。 |
| 相关仓库差异 | 对固定版本的两个 `automation` 目录比较文件集合与 SHA-256 | PPCDK 与 ZovoCard 的该目录均为 12 文件，全部逐字节相同。 |

Go package 范围：`cmd/membership-worker`、`internal/checkout`、`internal/config`、`internal/domain`、`internal/executor`、`internal/processor`、`internal/provider`、`internal/secure`、`internal/store`。

## 2. 有业务意义的回归场景

### 账号与账单服务

- 自有账号查询/取消失败时不会调用旧第三方服务。删除测试数据库里的旧 token 表后，自有取消与重新观察仍可运行。
- 缺失布尔值、未知套餐、缺失付费币种/到期证据、陈旧观察不会被推断成 free；付费档位即使过期也不会自动变成 free。
- Session 与观察到的邮箱不匹配时进入订单恢复处理，不触发全局会员服务熔断。
- 取消续费后重新观察，拒绝取消前的缓存；普通查询错误不会把失败的半开探测记为成功。
- 账单配置缺必填字段或包含控制字符时在执行前报错；返回内容来自同一不可变 profile，错误不包含账单值，取消的 context 不返回资料。

代码：[account_service_test.go](../internal/processor/account_service_test.go)、[configured_billing_test.go](../internal/provider/configured_billing_test.go)。

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

生产 `FILL_FRAME_JS` 被直接送入 Node VM；DOM 模拟实现输入框、选择框、事件处理器和帧渲染回调，覆盖：

- 卡号空格、有效期格式化与等价的月份/年份选项；
- 不存在对应选项、输入事件清空值、渲染时替换节点；
- 原生 validity、ARIA invalid、存在但没有资料的可选地址字段；
- Python 在传入卡资料前排除不允许的 frame origin，JS 再检查当前 origin；
- 某 frame 的失败不能被另一个 frame 的成功字段集合掩盖；填表失败后不会申请点击；
- 无候选字段且不运行渲染回调的 frame 返回空字段集合，不阻塞、不提供填写成功证据。

测试输出只有字段名与布尔值，不回传卡号、CVC 或账单值。代码：[test_fill_dom.py](../python_executor/tests/test_fill_dom.py)、[fill_dom_double.cjs](../python_executor/tests/fill_dom_double.cjs)。

## 3. 明确未完成的验证

**真实 Chromium 离线页面测试没有通过环境启动阶段。** 浏览器创建本地 IPC socket 被 sandbox 拒绝；请求扩大执行权限后，自动审批策略又拒绝了 sandbox escalation。于是使用上述离线 DOM 模拟验证 JS 逻辑，没有把它写成真实浏览器测试通过。

Node DOM 模拟不能证明 Chromium/React/Stripe 的真实行为，也不能证明现有 selector、`data-kw-*` 或 meta 标记存在于当前线上页面。没有向真实页面注入自定义标记来制造契约成功。

本轮也没有验证真实账号订阅查询、取消续费、银行 3DS、实际开卡/卡充值、商户扣款、会员到账或真实未知结果恢复。完整 kwRedeem 配套 schema 未取得，本分支也未提供经验证的完整替代迁移，所以没有从空库独立启动完整业务的成功记录。

## 4. 如何复现离线验证

在仓库根目录使用满足 `go.mod` 的 Go 版本、Python 3 和 Node.js：

```bash
go test ./...
python3 -m unittest discover -s python_executor/tests -v
git diff --check
```

Python DOM 模拟测试在没有 Node.js 时会明确 skip；本轮运行环境有 Node.js，因此这 11 项均实际执行、没有 skip。Node.js 仅用于这些测试，没有增加到付款执行器的生产依赖。

真实运行验收的代码、数据和证据缺口见 [INDEPENDENT_ENGINE.md](INDEPENDENT_ENGINE.md) 与 [DEPENDENCY_INVENTORY.md](DEPENDENCY_INVENTORY.md)。

# 服务器隔离、IPC 复查与路线绑定设计

记录日期：2026-11-13。本文区分本地实际观察、已核验的历史 CI 和尚未实施的设计。诊断只使用普通受管执行权限、最小 socket 探测和代码只读审阅；没有访问真实 ChatGPT/Zovo 站点，没有读取 Session，没有传入代理，也没有执行真实账号或付款流程。

结论：当前代码已经为每次浏览器执行创建独立 profile；执行队列和 worker 调度仍明确串行。代理配置目前作用于进程或 executor 实例，尚未按订单绑定。此次三次 IPC 探测没有显示本地 Chromium 的启动条件已经恢复。

## 1. 验证证据的范围

| 证据 | 实际结果 | 可以说明什么 |
| --- | --- | --- |
| 本次本地 IPC 复查 | 三次 Unix stream/datagram socket 创建均返回 `EPERM`；三次 Unix socketpair 与 TCP loopback 收发均成功 | 某些 IPC 可用，Unix socket 新建被当前执行边界拒绝；不能笼统说所有 IPC 都不可用 |
| 此前本地完整 Chromium 启动 | 在 `ProcessSingleton` 创建 Unix socket 时退出，未进入离线页面测试 | 本地完整 Chromium 的这次启动未成功；本次 socket 复查没有发现该前置条件恢复 |
| 此前本地 Python 套件 | `52` 项中 `39` 项通过，`13` 项真实浏览器测试明确 skip | 本地单元测试与 Node DOM 模拟通过；没有本地真实浏览器通过证据 |
| 已核验历史 CI | 下述固定提交的 Python `52` 项全部通过、`0` skip，包含 `13` 项真实离线浏览器 DOM；Go `11` 个 package 通过 | 该提交在 GitHub runner 的合成页面测试通过；不代表本地环境恢复或真实网站兼容 |
| 本轮文档及同时进行的后续开发 | 本文编写时尚未取得对应新提交的 CI 结果 | 不能将上一提交的通过结果自动沿用到后续变更 |

历史 CI 的固定证据：

- [Actions run 43395489393](https://github.com/7yedeng/kwMembership/actions/runs/43395489393)，`head_sha=3a3b0f5937fe2da4b8a3b2db05ce9d106870b96e`。
- [Python job 131785266286](https://github.com/7yedeng/kwMembership/actions/runs/43395489393/job/131785266286)：`52 tests`、`OK`、`0 skip`。
- [Go job 131785266708](https://github.com/7yedeng/kwMembership/actions/runs/43395489393/job/131785266708)：`11` 个 package 通过。

上述记录来自上一轮逐项核验，不是本次重新执行。现有 [离线 CI workflow](../.github/workflows/offline-validation.yml) 安装官方 Chromium headless shell，并设置 `KWMEMBERSHIP_REQUIRE_BROWSER_TESTS=1`；缺浏览器或任何 skip 都会令 job 失败。CI 页面请求全部由本地 fixture fulfill 或 abort，不使用真实账号资料。详细测试范围见 [VALIDATION.md](VALIDATION.md) 与 [test_browser_fill.py](../python_executor/tests/test_browser_fill.py)。

## 2. 本次三次 IPC 探测

普通执行工具曾短暂返回 `exec-server transport disconnected`，错误发生在创建进程之前。连接恢复后完成以下一批三次探测；工具连接错误不计作 socket 或浏览器运行结果。

| 操作 | 第 1 次 | 第 2 次 | 第 3 次 |
| --- | --- | --- | --- |
| `socket(AF_UNIX, SOCK_STREAM)` | `errno=1, Operation not permitted` | 同左 | 同左 |
| `socket(AF_UNIX, SOCK_DGRAM)` | `errno=1, Operation not permitted` | 同左 | 同左 |
| `socketpair(AF_UNIX, SOCK_STREAM)`，发送并接收一个字节 | 通过 | 通过 | 通过 |
| `AF_INET`，`127.0.0.1` 临时端口监听、连接、发送并接收一个字节 | 通过 | 通过 | 通过 |

同期 `/proc/self/status` 的非敏感诊断字段为：

```text
NoNewPrivs:      1
Seccomp:        2
Seccomp_filters: 1
```

可以确认执行进程处于 seccomp 过滤环境，并观察到特定 socket 系统调用被拒；未获得宿主规则明细，不能进一步断言具体过滤规则、规则来源或所有浏览器失败原因。

以下为本次实际使用的探测核心。它不读凭据、不连接外网、不改变系统策略，socket 与 listener 在每次探测后关闭。

```python
import json
import socket


def create(family, kind):
    with socket.socket(family, kind):
        return True


def pair():
    a, b = socket.socketpair(socket.AF_UNIX, socket.SOCK_STREAM)
    try:
        a.sendall(b"x")
        return b.recv(1) == b"x"
    finally:
        a.close()
        b.close()


def loopback():
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as listener:
        listener.settimeout(2)
        listener.bind(("127.0.0.1", 0))
        listener.listen(1)
        with socket.create_connection(listener.getsockname(), timeout=2) as client:
            connection, _ = listener.accept()
            with connection:
                client.sendall(b"x")
                return connection.recv(1) == b"x"


operations = {
    "unix_stream": lambda: create(socket.AF_UNIX, socket.SOCK_STREAM),
    "unix_dgram": lambda: create(socket.AF_UNIX, socket.SOCK_DGRAM),
    "unix_socketpair": pair,
    "inet_loopback": loopback,
}
for attempt in range(1, 4):
    result = {"attempt": attempt}
    for name, operation in operations.items():
        try:
            result[name] = {"ok": operation()}
        except OSError as error:
            result[name] = {
                "ok": False,
                "errno": error.errno,
                "error": error.strerror,
            }
    print(json.dumps(result))
```

## 3. 本地浏览器状态与支持的后续环境

本次确认 `/usr/bin/chromium` 仍存在。PATH、默认 Playwright cache、此前专用 `browser-test-runtime` 目录和常用系统安装路径均未发现可执行的 Chromium headless shell。没有重新下载安装。

此前完整 Chromium 的已观察到的关键 stderr 为：

```text
FATAL:chrome/browser/process_singleton_posix.cc:297
Check failed: . socket() failed: Operation not permitted (1)
```

此次 Unix socket 仍连续被拒，因此没有再次启动依赖该前置条件的完整 Chromium，也没有新增本地真实浏览器测试成功记录。此前 headless shell 安装取得的是无效 ZIP，未取得可启动二进制；不能把它描述为启动后复现了完整 Chromium 的 IPC 故障。

可使用现有 GitHub runner 执行纯离线 DOM 验证，或由运行环境提供支持浏览器所需 IPC 的普通测试主机。服务器应使用受支持的非特权浏览器运行方式，保留正常 sandbox；生产 Xvfb/headful Chrome 的启动条件与 headless shell 的离线测试结果分别验收。不以 `--no-sandbox`、修改宿主 seccomp 或重复申请已拒权限作为本环境修复方案。此前权限升级申请已被自动审批拒绝，本轮没有再次申请。

## 4. 当前每个 job 已有的隔离

### Python

[live.py](../python_executor/live.py) 的 `LiveExecutor.execute` 每次执行都会：

1. 用 `tempfile.mkdtemp` 创建新的 `kwmembership-python-` profile 目录。
2. 用这个目录启动新的 `launch_persistent_context`，禁止 service worker。
3. 只加入当前 job 的 Session cookies。
4. 在 `finally` 中关闭 context，并尝试删除该目录。

这里的 persistent context 使用每次新建的目录，不是多订单复用同一个登录 profile。正常生命周期已经分离；`shutil.rmtree(..., ignore_errors=True)` 会忽略清理失败，强制终止进程也可能留下目录，当前没有独立的遗留 profile 清理流程。

### Go

[chrome.go](../internal/checkout/chrome.go) 的 `ChromeExecutor.Execute` 每次建立独立 timeout context、`chromedp.NewExecAllocator` 和 browser context，并注入当前请求的 cookies。`chromeOptions` 没有指定共享 `UserDataDir`。

本地核对了当前依赖 `chromedp v0.16.0` 的 `allocate.go`：`ExecAllocator.Allocate` 在未传 `user-data-dir` 时通过 `os.MkdirTemp` 创建临时 profile，并在进程退出后删除。对应上游固定版本源码：[allocate.go](https://github.com/chromedp/chromedp/blob/v0.16.0/allocate.go)。因此代码具备每次执行独立 profile 的依据，但不能据此宣称已有容器或 OS 用户隔离。

## 5. 当前并发和代理的具体边界

| 范围 | 当前实现 | 后续并发时需要解决的问题 |
| --- | --- | --- |
| Python runner | [runner.py](../python_executor/runner.py) 的 `run_forever` 同步执行一个 lease，再领取下一个 | 尚无 runner 内任务池；仅增加进程仍受全局队列限制 |
| 执行队列 | [queue.go](../internal/executor/queue.go) 的 `Queue.LeaseNext` 统计全表 `leased/action_required`，只要不为零就返回 `ErrNoCommand` | 明确限制为一个活跃任务；需要有意设计容量槽位，不能只删掉此门禁 |
| Processor owner | [lease.go](../internal/store/lease.go) 的 `AcquireLease` 使用 `id='default'`；[main.go](../cmd/membership-worker/main.go) 的 `runLoop` 同步执行 `runTick` | 保留单一调度 owner 的同时，可另行设计有上限的任务派发；不能靠多开相同 worker 绕过 fencing |
| Python 代理 | `LiveExecutor.execute` 从 `KWMEMBERSHIP_CHROME_PROXY_SERVER` 读取一个 server | 进程配置，不是 durable job 路由选择 |
| Go 代理 | `NewChromeExecutor` 接收一个 proxy；`chromeOptions` 对所有执行应用它 | executor 实例配置，不是 job 绑定 |
| 代理输入格式 | [config.go](../internal/config/config.go) 的 `validateChromeProxyServer` 支持无凭据的 `http://host:port`、`socks5://host:port` | 当前没有路由注册表、按 executor 的路由权限或版本匹配；Python 直接读环境，未复用该 Go 校验 |
| 持久化路由 | [types.go](../internal/checkout/types.go) 的 `Request/ExecutionBinding`、`Queue.Command`、[client.py](../python_executor/client.py) 的 `ExecutorLease` 都没有路由字段 | 无法从持久化记录证明某次预检、付款 attempt 与恢复使用了指定配置 |
| 桌面 | [visible-browser.conf](../deploy/kwmembership-visible-browser.conf) 固定 `DISPLAY=:99`；[Xvfb service](../deploy/kwmembership-xvfb.service) 也固定 `:99` | 多任务仍共享显示环境，没有按 job 的桌面隔离 |
| 生命周期 | `LeaseDuration=20s`；Python 在轮询中每 `5s` heartbeat，浏览器启动等同步调用没有在调用处绑定 lease 剩余时长 | 资源拥塞或阻塞时需要监督进程、取消、可靠释放槽位，并保留未知结果的 reconciliation |

Python bridge client 只允许 loopback HTTP，禁用环境 HTTP proxy 并拒绝 redirect，见 `ExecutorClient` 与 `_loopback_base_url`。这支持本机协作；跨服务器 executor 需要另外设计受认证的传输与身份绑定，不能仅放宽监听地址。

本节只做代码审阅，没有实现多任务调度、路由注册表、跨服务器通信或新的部署隔离。

## 6. 路线 ID 绑定的最小 durable 设计（待开发）

### 6.1 绑定范围应早于付款 attempt

不能只给 `membership_fulfillment_attempts` 加字段：`Processor.checkoutPreflight` 在正式付款 attempt 创建前已经使用浏览器，预检 command 的 `AttemptNo` 使用当时的 fulfillment state revision；真正付款 attempt 由 `checkoutPrepareExecution` 后续创建。这两类编号不能当成同一序列。依据：[processor/checkout.go](../internal/processor/checkout.go) 的上述两个函数。

建议第一版采用一个明确限制：**服务端在某 fulfillment 首次浏览器请求之前选定路线，整个 fulfillment 生命周期内保持该路线及配置版本不变。** 这同时覆盖预检、Plus、升级、重试与需要同一浏览器路线的后续观察。Card API 的服务端 HTTP 路径是另一类依赖，不自动继承浏览器代理。

若后续自有 `AccountService` 在 eligibility 阶段提前使用浏览器，绑定点也必须提前到该操作之前，并复用同一绑定；不能让账号观察与付款分别选择路线。本设计针对已有 fulfillment 的 worker 流程，不表示当前无数据库的独立预检 CLI 已具有持久化路线记录。

路线 ID 表示服务器批准的网络配置，不证明代理供应商实际提供固定出口 IP；即使绑定 ID 和版本，供应商若轮换出口，也需要另外的契约才能保证出口稳定。

### 6.2 推荐最小 schema 增量：两张现有表、四列

以下只是设计，不是已执行 DDL 或迁移文件。继续使用当前唯一 SQLite，不建立第二个 membership 数据库或新的派发队列。

| 现有表 | 拟新增列 | 角色 |
| --- | --- | --- |
| `membership_fulfillments` | `browser_route_id TEXT`、`browser_route_version INTEGER` | 订单履约期间唯一、不可变的路线绑定，供 Go 直接执行和 Python bridge 共用 |
| `membership_checkout_commands` | `browser_route_id TEXT`、`browser_route_version INTEGER` | 入队时复制的不可变快照；领取、取材料、permit 与报告时对照 fulfillment 绑定 |

两个字段必须同时为空或同时有效；新执行必须有非空 ID 和正整数版本。ID 应是有长度限制的简单标识符，不接受 URL、userinfo、路径、邮箱、控制字符或调用方提供的代理地址。空值仅代表迁移前尚无可信绑定，**不能解释为 direct 或自动套用当前默认代理**。需要直连时也使用服务端明确配置的路线 ID 和版本。

此最小方案不必再给每个付款 attempt 重复两列，因为 attempt 已关联 fulfillment，且路线在整个 fulfillment 内不允许变更。若将来允许同一 fulfillment 更换路线，则需要独立的不可变路线绑定历史及 attempt/preflight 引用；不能继续原地更新上述两列并声称历史执行仍可追溯。

服务端注册表以 `(route_id, version)` 解析批准的协议、地址和受控配置；配置版本发布后不可被重新解释为另一个地址。数据库仅存标识和版本，不存原始代理凭据、Session、卡材料或用于拼接网络地址的客户端输入。停用路线应令新动作停止，不能静默切换到直连或其他代理。

### 6.3 需要同时改变的契约与事务

1. 在 `checkoutPreflight` 第一次可能启动浏览器之前，使用现有 fenced transaction/状态 revision 做 compare-and-set 绑定；已经绑定的订单只读并验证，不重新选择。若期望 revision 已变化则停止当前执行。
2. `checkoutPrepareExecution`、Go `Request/ExecutionBinding`、Python bridge `CommandInput/Command`、lease/material 契约都携带同一对 route ID/version。原始代理地址由受控运行配置解析，不接受订单输入任意指定地址。
3. `Queue.Enqueue` 的幂等键保持现有 `(fulfillment_id, stage_key, attempt_no, command_kind)`。`INSERT OR IGNORE` 命中旧记录后必须比较路线快照；同一幂等键换路线返回冲突，不能悄悄沿用或覆盖。
4. executor 身份与允许路线由服务器配置绑定。领取任务按这一可信映射筛选，不能只相信调用方自报的 `executorId` 或支持列表。当前共享 bridge secret 适用于同机受信进程；不等于多租户 executor 身份隔离。
5. `ValidateLease`、材料领取、动作 guard 与激活前检查需验证 command、fulfillment、executor 实际配置版本一致；不一致时先停止，不能在领到材料后才尝试另一出口。lease epoch/token、一次性材料领取与原 permit 规则继续有效。
6. 路线绑定本身不证明浏览器实际网络路径。Go 启动参数、Python launch kwargs 与 resolved route 必须一起离线验证；配置不支持、已停用或版本缺失时明确失败。

现有 processor lease、账号/卡锁、资金与配额门禁不因路线功能而改变。此功能可以先在当前单活跃任务模式下实现并验证；实际并发另做容量、清理与操作系统隔离设计。

### 6.4 迁移与恢复不能凭空补历史

正式迁移需要先明确共享数据库所有者和完整 DDL，见 [STANDALONE_CONTRACT.md](STANDALONE_CONTRACT.md)。不能把测试 fixture 拼接为生产 schema，也不能由只读 schema 检查命令偷偷写入新列。

建议部署顺序是先安装兼容的 nullable 列和成对有效约束，再发布理解路线契约的新 worker/executor。新功能必须有 schema capability 检查；旧二进制不理解路线约束时不得继续领取新版本命令。历史无绑定记录需要按状态处理：从未执行浏览器的请求可在首次执行前绑定；既有预检证据若缺路线来源不能直接宣称匹配；存在活动 lease、已激活动作或 unknown outcome 的记录进入原恢复/对账流程，不能用当前配置批量回填来制造历史绑定。

## 7. 路线设计的离线验收点（待开发、未执行）

| 场景 | 必须验证的结果 |
| --- | --- |
| schema 升级与旧数据 | nullable 旧记录仍可读；半空组合、非法版本、非法 ID 被拒；读检查不修改数据库；未绑定记录不能进入新执行 |
| 首次绑定与竞争 | 同一 fulfillment 的并发 CAS 只能产生一个结果；后续 state revision 更新不改变路线；预检先于付款 attempt 也已有绑定 |
| 幂等入队 | 相同 attempt/command key 与相同路线复用；换 ID 或版本返回冲突；不同订单的绑定不串用 |
| Go/Python 一致性 | 两种 executor 使用同一绑定快照；合成 lease/material 和 mock launch 捕获正确配置；不在日志或错误中出现代理地址、凭据或 Session |
| executor 资格 | 不支持路线的 executor 领不到任务；自报其他 executor ID/route 不能取得其材料；队列优先级只在可执行任务中比较 |
| 配置发布与停用 | 同 ID 新版本不改变在途绑定；找不到旧版本、被停用或解析失败均停止；不 fallback direct，不自动换出口重试 |
| stale lease / permit | 旧 epoch、旧 token、路线不匹配或重放材料领取都失败；单 permit 不会因代理失败派发第二次点击 |
| 崩溃与未知结果 | 重启保持原路线记录；丢失活动 context、动作已激活或网络结果未知时继续 reconciliation，不自动重新付款 |
| profile 与生命周期 | 两个合成 job 得到不同 profile 路径；正常/异常取消均回收；清理失败只输出安全诊断，并有可验证的遗留目录处理 |
| 容量行为保持 | 仅加路线绑定时仍保留单活跃任务门禁；未来放宽并发必须单独验证容量、账号/卡互斥和资源释放 |

测试材料全部应为合成数据，网络只使用本地 mock/fixture。路由选择测试不需要真实代理、真实登录或支付网站；真实浏览器测试的通过只证明离线 fixture 行为，不能替代线上兼容性与实际付款结果的证据。

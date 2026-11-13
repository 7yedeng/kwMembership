# IPC 复查：创建 socket 与绑定地址是不同的失败点

核验日期：2026-11-13。范围是普通受管执行环境的最小 IPC 探测，以及 Linux/Chromium 官方文档和源码核对。本轮没有启动真实账号流程、读取 Session、访问真实支付站点、修改宿主策略或浏览器二进制，也没有申请权限升级。

**本次确认的失败点是创建 Unix socket，错误为 `EPERM`（errno `1`）；不是 `bind`、`listen`、路径占用或 socket 文件权限失败。** 因此，抽象命名空间、换临时目录、清理旧 socket 文件不能影响目前这个失败点。修改自有程序的通信方式是另一项可行的工程选择，但不会自动改变第三方 Chromium 内部的 ProcessSingleton 实现。

## 1. 本轮实际结果

本轮进行了一个最小批次，每种操作各一次。此前三次重复探测见 [SERVER_ISOLATION_AND_IPC.md](SERVER_ISOLATION_AND_IPC.md)；两轮结论一致，本轮额外明确记录了失败阶段。

| 操作 | 本轮结果 | 到达的阶段 |
| --- | --- | --- |
| `socket.socket(AF_UNIX, SOCK_STREAM)` | `PermissionError` / `errno=1` / `Operation not permitted` | 构造 socket 失败；没有调用 `bind` 或 `listen` |
| `socket.socket(AF_UNIX, SOCK_DGRAM)` | 同上 | 构造 socket 失败；没有调用 `bind` 或 `listen` |
| `socket.socketpair(AF_UNIX, SOCK_STREAM)` | 两端成功发送、接收一个字节 | 已连接的一对 socket 可用 |
| `AF_INET` / `127.0.0.1` / 操作系统分配临时端口 | 监听、连接、发送和接收一个字节均成功 | TCP loopback 通信可用 |

socket 创建探测的核心代码如下。失败返回后不会继续尝试地址变体；本轮没有测试抽象地址或任何 `/tmp`、`/dev/shm` 路径绑定。

```python
import socket


def unix_create(kind):
    try:
        endpoint = socket.socket(socket.AF_UNIX, kind)
    except OSError as error:
        return {
            "stage": "socket_create",
            "ok": False,
            "errno": error.errno,
            "error": error.strerror,
            "bind_attempted": False,
            "listen_attempted": False,
        }
    endpoint.close()
    return {
        "stage": "socket_create",
        "ok": True,
        "bind_attempted": False,
        "listen_attempted": False,
    }


print(unix_create(socket.SOCK_STREAM))
print(unix_create(socket.SOCK_DGRAM))
```

同期 `/proc/self/status` 只读取了以下三个字段：

```text
NoNewPrivs:      1
Seccomp:        2
Seccomp_filters: 1
```

`Seccomp=2` 表示 filter 模式，`Seccomp_filters=1` 表示该进程附加了一个过滤器。[Linux proc_pid_status(5)](https://man7.org/linux/man-pages/man5/proc_pid_status.5.html)

Linux 的 seccomp 机制可以按系统调用及参数过滤，也可以在不执行系统调用时返回指定 errno；这与观察到的现象相容。但我们没有读取到宿主过滤规则，**不能仅凭 `EPERM` 和这三个字段证明具体是哪一条 seccomp/其他安全规则造成拒绝**。可确定的事实是：在当前受管执行边界内，上述创建操作被拒，而其他两种通信操作可用。[Linux Kernel：Seccomp BPF](https://docs.kernel.org/userspace-api/seccomp_filter.html)

## 2. 为什么路径方案改变不了这一步

`socket(domain, type, protocol)` 创建通信端点，其参数没有文件路径。`bind(fd, address, length)` 随后给已经创建好的端点分配地址。pathname 与 abstract namespace 的区别在地址中体现，而不是通过另一种 `socket()` 地址族创建。[socket(2)](https://man7.org/linux/man-pages/man2/socket.2.html)、[bind(2)](https://man7.org/linux/man-pages/man2/bind.2.html)、[unix(7)](https://man7.org/linux/man-pages/man7/unix.7.html)

对于当前失败，程序在取得可用文件描述符之前就返回了错误；此时还没有选用某个 socket 文件名。这是下面各项判断的依据。判断属于“实际失败阶段 + 官方接口语义”的推论，不是声称本轮逐个执行了这些替代方案。

| 提议 | 它适合处理的问题 | 对本次失败的影响 |
| --- | --- | --- |
| 改为 abstract namespace | Linux 中不依赖文件系统路径的 Unix socket 命名；不受 pathname socket 文件权限的同一套规则影响 | 仍先需要创建 `AF_UNIX` socket；没有现成 fd 就无法绑定抽象地址，因此不能修复本次创建失败 |
| 从 `/tmp` 换到 `/dev/shm`，或反过来 | 选用不同目录，处理路径权限、路径长度或特定文件系统不支持 socket 节点等问题 | 这些差异影响地址/文件层；当前失败发生在地址传入前，换目录不改变该调用 |
| `unlink` 旧 socket 或清理旧锁文件 | 已确认的旧路径残留、地址冲突、单实例锁恢复 | 不会改变 `socket()` 是否获准；本轮不是 `bind` 返回 `EADDRINUSE`，也没有观察到旧 socket 文件是根因 |
| 自有 IPC 改用 `socketpair` | 两端由同一启动流程安排，能够传递/继承已连接描述符的自有协议 | 本环境最小收发已通过，可作为自有程序的设计选择；它不自动提供一个可供任意独立进程按路径连接的命名服务，也不会改写 Chromium |
| 自有 IPC 改用 loopback TCP | 自有客户端与服务器的本机连接 | 本环境最小收发已通过；只证明这条自有通信链路可用，不证明 Chromium 的其他启动依赖可用 |
| 自有父子进程使用匿名 pipe | 自己能够控制两端协议与描述符生命周期的场景 | 属于另一种应用设计；本轮没有新增 pipe 实测，不能把它写成本地浏览器启动成功的证据 |

abstract namespace 与 pathname 的区别见 [unix(7)](https://man7.org/linux/man-pages/man7/unix.7.html)；命名冲突与路径相关错误见 [bind(2)](https://man7.org/linux/man-pages/man2/bind.2.html)；`socketpair()` 创建一对已连接、未命名端点的语义见 [socketpair(2)](https://man7.org/linux/man-pages/man2/socketpair.2.html)。

## 3. 与此前 Chromium 失败的对应关系

此前完整 Chromium 的 stderr 已记录为：

```text
FATAL:chrome/browser/process_singleton_posix.cc:297
Check failed: . socket() failed: Operation not permitted (1)
```

本轮没有重新启动 Chromium；以上是此前实际启动记录，不是新实验结果。官方 `process_singleton_posix.cc` 的 `SetupSocketOnly()` 包含：

```cpp
int sock = socket(PF_UNIX, SOCK_STREAM, 0);
PCHECK(sock >= 0) << "socket() failed";
```

`SetupSocket()` 先调用它，再设置地址；`ProcessSingleton::Create()` 在后续才进行 `bind()` 和 `listen()`。因此这条错误信息与 socket 创建失败一致，不能把它解释为 socket 路径不可写。独立临时 profile 可以避免共用用户资料，仍不等于跳过单实例通信端点的创建。[Chromium：process_singleton_posix.cc](https://chromium.googlesource.com/chromium/src/+/main/chrome/browser/process_singleton_posix.cc)

本次源码核对使用官方 `main` 文件，检索时文件 blob 为 `827a8a94b0a185638678d63e3e8f9f5e380cca28`。它用于核对函数和失败机制；没有据此声称本地安装二进制与该源码版本逐字对应。引用采用函数名，stderr 中的行号只作为历史运行记录保留。

## 4. Remote debugging TCP / pipe 为什么是另一条通道

`--remote-debugging-port` 与 `--remote-debugging-pipe` 改变的是自动化客户端连接 DevTools 的方式。官方 `RemoteDebuggingServer::GetInstance()` 分别启动 HTTP/TCP 服务或 pipe handler；TCP factory 监听 localhost，pipe API 使用读写描述符 `3`、`4`。[Chromium：remote_debugging_server.cc](https://chromium.googlesource.com/chromium/src/+/main/chrome/browser/devtools/remote_debugging_server.cc)、[Chromium：devtools_agent_host.h](https://chromium.googlesource.com/chromium/src/+/HEAD/content/public/browser/devtools_agent_host.h)

ProcessSingleton 处理浏览器 profile 的单实例协调，DevTools 处理自动化通信。根据上述两个独立实现，**单独把 DevTools 从 TCP 换成 pipe，或从 pipe 换成 TCP，都没有替换 `SetupSocketOnly()` 的 Unix socket 创建**。这是源码所支持的结论；本轮没有用这些开关循环重试浏览器，也不声称所有 Chromium 组件只需要一种 IPC。

本项目自有 Go/Python bridge 已通过受限制的 loopback HTTP 通信，见 [ExecutorClient](../python_executor/client.py) 和 [worker 的 executor listener](../cmd/membership-worker/main.go)。这条自有链路与 Chromium 的 ProcessSingleton 不同；改动 bridge 协议不会解决当前浏览器内部的创建失败。

## 5. 本次可以得出的工程结论

对 Gemini 所列方案，应保留它们各自的适用条件：地址命名/目录/清理方案用于地址与文件层错误；socketpair、TCP、pipe 用于能够控制两端的自有 IPC 设计。当前记录不支持把这些方案视为完整 Chromium 已有失败的直接修复。

下一步若要验证完整浏览器，应在本身支持所需 IPC 的普通测试主机或既有 CI 环境中，用独立 profile 和离线 fixture 验收。已有 CI 的真实离线浏览器通过记录证明对应 runner 上的合成 DOM 行为，不证明此受管环境已经恢复，也不证明真实登录或付款页面兼容。历史证据与范围见 [SERVER_ISOLATION_AND_IPC.md](SERVER_ISOLATION_AND_IPC.md)。

把订阅目标改为埃及/ChatGPT Go 属于业务请求与价格契约的调整；本次 socket 错误发生在浏览器启动阶段，两者应分别验证。本文没有修改套餐、价格、账单或付款逻辑。

如需运维复用，建议后续单独增加只输出 `stage/errno/可用性` 的小型诊断命令：有限次数、loopback、及时关闭描述符，不包含 Session、代理或真实站点。应让它报告 `socket_create_failed`，避免笼统记录“IPC 不可用”后重复尝试无关的路径设置。本轮只把实际诊断代码记录在本文，没有增加脚本或修改执行器。

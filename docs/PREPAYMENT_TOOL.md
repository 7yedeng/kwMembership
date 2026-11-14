# Plus 付款前准备工具

这套工具把已经通过用户本机两账号验收的 Session 恢复核心，与独立结账浏览器、Zovo 基础卡片查询、实际账单填写和最终金额核验连接起来。当前目标是 **Plus / PH / PHP**，到付款前审核结束。Go / EGP 尚未在这个入口支持；用户界面不会把 Plus 改名后当作 Go。

工具只有 `check` 与 `run` 两种模式，没有付款、开卡、加款、绑卡提交、取消续费或生产 worker 启动命令。本次新增的是可执行代码与合成环境回归，**不表示真实 Plus 页面、卡台凭据或真实卡片已经由助手完成验收**。

## 1. 本机桌面入口

Windows 包中的入口是 `tools/prepayment-desktop/start.cmd`，主程序为 `bin/membership-prepare.exe`。桌面窗口独立于网页，没有本地 HTTP 服务或网页输入面板。Python 需要带 Tkinter；启动脚本先尝试现有 `python`，再尝试 `py -3`，不修改全局 Python 配置。

界面分三页：

| 页签 | 输入或显示内容 |
| --- | --- |
| Session 与卡台 | 完整 Session JSON、卡台配置别名、Zovo App ID（可选）、App Secret（掩码）、已有卡片的数字 ID、用户允许的 PHP 总额上下限 |
| 实际账单（可选） | 该卡实际登记的姓名和地址；仅补卡台返回的缺失字段，自动绑定上页的配置别名和卡 ID |
| 运行与审核结果 | 账号指纹、实际报价、卡尾号、卡台返回余额、币种是否明确及尚未确认的事项；不显示 Session、API Secret、完整卡号、CVV 或账单姓名地址 |

`仅检查输入` 是离线校验，不创建浏览器或卡台客户端。`运行至付款前` 才启动真实链路。两者使用相同输入结构；敏感输入送入私有进程管道后会从界面清空，因此做完检查后再次运行需要重新填入 Session 和 API 凭据。也可直接点击运行。

默认勾选“显示独立 Chrome”。达到审核状态后，结果会立即显示，浏览器最多保留两分钟用于观察；没有“确认付款”按钮。关闭窗口或点击取消会请求子进程取消，并等待它关闭浏览器，不能把取消后的结果当作正常完成。

**浏览器短暂出现后关闭，不代表已经填表完成。** 两分钟观察只在 `review_ready` 成功后开始；此前任何失败都会立即关闭任务浏览器。诊断版 `20261114.1` 会在结果页保留最终 `stage`，显示固定错误码的解释，并提供“复制脱敏诊断”。每次结束还会覆盖 `tools/prepayment-desktop/logs/last-diagnostic.json`，只保存固定错误码、阶段、校验布尔值、版本和时间，不保存账号指纹、报价、卡片信息、Session 或 API Secret。保存失败时仍可从界面复制。

预算没有默认值：请填入你允许的最终总额范围，税费和折扣也必须来自结账事实。文档和测试中的数字都是合成用例，不代表当前套餐售价。账单国家填写实际发卡资料，不自动改成 PH，也不调用随机地址生成器。

## 2. CLI 输入与模式

凭据只通过标准输入传入，不接受 Session、卡号或 API Secret 命令行参数。输入结构如下，尖括号是待用户填写的占位，不是可运行凭据：

```json
{
  "session": {
    "sessionToken": "<完整原始会话值>",
    "user": {"id": "<目标用户ID>", "email": "<目标邮箱>"}
  },
  "target": {"plan": "plus", "country": "PH", "currency": "PHP"},
  "budget": {"min": "<PHP最低总额>", "max": "<PHP最高总额>"},
  "card": {
    "platformKey": "zovo-primary",
    "kind": "zovocard",
    "cardId": 123,
    "credential": {"appId": "<可选App ID>", "appSecret": "<App Secret>"}
  },
  "billing": {
    "platformKey": "zovo-primary",
    "cardId": 123,
    "address": {
      "name": "<实际登记姓名>",
      "line1": "<实际地址>",
      "line2": "",
      "city": "<实际城市>",
      "state": "<实际州/省，可空>",
      "postalCode": "<实际邮编，可空>",
      "country": "<实际两位国家代码>"
    }
  }
}
```

`session` 中可保留用户原始完整 JSON；只有 accessToken 而没有有效 sessionToken 的材料会停止。`billing` 整块可以省略。提供时，配置别名和卡 ID 必须与 `card` 完全相同，任何与卡台实际资料的冲突都会停止。工具拒绝重复 JSON 字段、未知操作字段、外部 API 地址覆盖和超长输入。

从程序或受控管道提供上述 JSON 后运行：

```sh
membership-prepare --check
membership-prepare --run --chrome-path /path/to/chrome --show-browser --hold-open 2m --timeout 3m
```

没有模式参数时默认 `--check`。普通 CLI 模式读取 stdin 到 EOF；EOF 仅表示输入结束。运行期间使用 SIGINT/SIGTERM 取消。

`--timeout` 范围为 1 秒至 5 分钟；`--hold-open` 范围为 0 至 5 分钟且只允许与 `--show-browser` 同用。**`--show-browser` 选择完整 Chrome 的有显示运行方式。** Linux 服务可以把该显示放在独立私有 Xvfb 中无人值守运行，仍由程序操作，无需人工坐在窗口前。省略该参数会选择严格 headless；项目已有站点拒绝该模式的记录，不能把合成测试或本机可见登录通过外推为 headless 真实站点可用。

这不是生产 worker 的新启动器；本机入口没有接入第二个数据库或队列，也不会接管常用 Chrome profile。生产调度、账号锁、资金许可及付款后对账仍由既有 Go worker 管理。

## 3. 桌面进程协议

桌面程序以 `shell=False` 启动 CLI。参数只包含浏览器路径与非敏感选项，完整输入作为一行紧凑 JSON 写入私有 stdin 管道。

使用 `--gui-protocol` 后，第一行以后的任意输入或 EOF 都表示取消。不存在肯定式付款命令；输入 `pay` 也只会取消。GUI 会保持管道打开，直到取消或子进程完成。

stdout 为脱敏 NDJSON：

```json
{"event":"ready","report":{"code":"PREPAYMENT_REVIEW_READY","readyForConfirmation":true,"paymentAllowed":false}}
{"event":"result","report":{"code":"PREPAYMENT_REVIEW_READY","browserClosed":true,"paymentAllowed":false}}
```

上面只是结构简写。实际成功报告还必须包含 Cookie 精确回读、目标身份、账单、卡字段、卡绑定、完整报价和清理等证据；不能靠这几项简写伪造成功。`ready` 在观察等待之前输出，`result` 在关闭浏览器之后输出。GUI 对字段和错误码再次白名单过滤，不显示子进程原始错误或任意文本。

stderr 仅输出固定的 `PREPARATION_STAGE <阶段>`，不输出原始浏览器协议日志。程序不读取系统剪贴板，不另存凭据、卡资料或 Session 文件。调用方若自行持久化输入，需要自行管理其文件；工具不会把材料复制到仓库或报告。

## 4. 一次任务内部顺序

1. 严格解析输入，绑定 Session 的目标邮箱和可用用户 ID。
2. 创建独立任务 Chrome，调用共享 Session 核心写入 Cookie、精确回读并核验真实账号；后续步骤继续使用这个浏览器。
3. 检查账号资格，进入明确的 Plus / PH / PHP 结账。已有付费订阅或未能确认套餐时停止。
4. 需要账单才能计税时，先读取所选 Zovo 卡快照并填写实际账单资料。
5. 等待实际税费与总额出现，核验套餐、币种、金额算式和用户预算。
6. 从同一份已缓存快照取得完整卡材料，核对卡片归属、ACTIVE 状态、有效期与已知商户禁用规则，填写卡字段并精确回读。
7. 再次读取最终报价与字段。金额仍在预算内、字段未被重置且最终控制已识别时，输出付款前审核结果；不激活支付控制。
8. 观察等待结束或取消后，由任务关闭浏览器并清理临时 profile。

账单阶段和卡字段阶段共用一次 `GetCardSnapshot` 调用。Zovo 适配器内部通过有界卡列表建立本地数字 ID 与上游卡 ID/卡号映射，再读取一次该卡详情并验证归属；不能将其描述成总计只有一次 HTTP GET。最多查询一个明确选择的卡快照，不为了通过而自动换卡或加款。

## 5. 卡台范围与余额判断

这个入口通过 `provider.CardSnapshotReader` 暴露只读卡片快照，无法调用 `OpenCard`、`RechargeCard`、代充、会员价格或自动续费接口。Zovo 基础适配器本身另有开卡/加款能力，但这些方法没有连接到当前 CLI/GUI。所有报告的 `fundingActionsTaken` 为 0，`paymentAllowed` 始终为 false。

卡台详情中的余额币种可能缺失。此时报告保留：

```json
{
  "balanceCurrencyKnown": false,
  "balanceComparable": false,
  "unconfirmed": ["card_balance_currency", "card_balance_sufficiency"]
}
```

卡币种明确但与 PHP 不同时，也不猜测汇率或手续费，不以数值大小判断够不够。只有显式相同币种才能比较观察到的余额与报价；同币种余额不足会停止。缺少完整商户限制列表仍列为未确认，兼容的单条 `google_chatgpt_blocked=false` 不代表所有限制都已核验；任何明确 ChatGPT/OpenAI 禁用都会停止。

付款前 ready 只表示已完成本次登录、结账和表单审核，不代表发卡行一定会批准交易，也不代表未知币种余额已经充足。后续真正付款需要另外的资金许可、余额/汇率证据和提交后的交易/权益对账。

## 6. 验收与当前证据

新增模块的测试只使用合成 Session、合成卡片、fake executor 和卡快照替身。覆盖了重复字段/未知命令、凭据不进参数和报告、绑定冲突、同卡两阶段缓存、余额币种未知、商户禁用、账号与字段证据、最终金额算式、GUI ready/最终结果顺序、取消及 EOF。

本模块已运行 `go test -count=1 -timeout=60s ./internal/preparation ./cmd/membership-prepare`，8 项 preparation 顶层测试和 4 项 CLI 顶层测试通过（含多个子场景）；`python3 -m unittest discover -s tools/prepayment-desktop -p 'test_*.py' -v` 的 9 项桌面控制器测试通过。这里的 GUI 测试没有拉起真实账号浏览器，桌面外观和真实链路需要各自验收。

真实站点访问授权没有因为换 CLI 而自动改变。当前助手没有通过其他驱动访问先前被 host restrictions gate 拒绝的 ChatGPT 目标。用户可在自己的允许环境中自行运行本机入口；这项真实验收的结果应单独记录，不以绿色模拟测试替代。

## 7. 代理、IP 白名单与失败定位

浏览器访问 ChatGPT 与 Go 程序访问 Zovo 是两条独立的网络路径。当前 Chrome 没有显式 `--proxy-server` 覆盖；卡台使用 Go 默认 HTTP 传输器，读取 `HTTP_PROXY` / `HTTPS_PROXY` / `NO_PROXY` 及其小写变量，不自动读取 Windows 的“系统代理”注册表开关，也不使用 `ALL_PROXY`。桌面子进程继承 GUI 启动时的环境，工具不会自动修改这些配置。系统 TUN、VPN 或路由仍可能影响最终出口，因此“未配置 HTTPS_PROXY”不能直接等同于“肯定从本机宽带公网 IP 出口”。

Zovo 白名单应匹配实际 API 请求的公网出口，不能填本地监听地址 `127.0.0.1`。浏览器查到的公网地址也不一定是卡台客户端的出口。先核对运行结果，再决定是否需要将已授权的固定出口加入白名单或明确配置卡台客户端路由；不要仅为排障清空白名单。

| 诊断结果 | 能证明什么 |
| --- | --- |
| `restoring_session` / `opening_checkout` 等较早阶段失败 | 尚未读取 Zovo 卡片；不能归因于卡台 IP 白名单 |
| `PREPARATION_CARD_AUTH_FAILED` | 卡台 HTTP 401，鉴权失败 |
| `PREPARATION_CARD_ACCESS_DENIED` | 卡台 HTTP 403，访问被拒；IP 白名单、应用权限等均需核对，不能仅凭 403 确定原因 |
| `PREPARATION_CARD_RATE_LIMITED` | 卡台 HTTP 429，限流 |
| `PREPARATION_CARD_TIMEOUT` | 卡台读取超时，不能据此判断为白名单拒绝 |
| `PREPARATION_CARD_OPERATION_REJECTED` | 卡台返回非成功业务码；原始文本未进入报告 |
| `PREPARATION_CARD_UNAVAILABLE` | 网络或其他未能细分的读取失败 |

首次 Zovo 请求发生在 `resolving_billing`：会话恢复、结账创建、首次套餐及币种识别已经通过之后。`reviewing_quote` 在填写账单前后都会出现，不能单凭这个阶段名判断是否曾读取卡台。新错误码只保留适配器已识别的固定分类，不透传上游响应、请求 URL 或原始错误字符串。

新浏览器入口没有提交方法，并对已知资金提交端点与原生表单提交安装拦截；若命中提交尝试，报告 `PREPAYMENT_SUBMISSION_BLOCKED` 并撤销 ready。保护安装失败会停止。CLI 与 GUI 的成功条件同时要求 `submissionFenceVerified=true`、`submissionBlocked=false`、`submitActivated=false`。未知第三方私有接口不能仅凭名称证明已被穷尽识别，因此正式站点仍需检查实际页面行为，不能将该工具称作已经完成实盘付款。

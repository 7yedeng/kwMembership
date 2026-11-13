# 会话与付款前测试工具包

## 本轮使用入口

Windows：完整解压后，双击 **打开付款前测试.cmd**。需要已有 Chrome，以及带 Tkinter 的 Python 3。该窗口是独立桌面程序，Session 不会粘贴到网页聊天框里。

Linux：有桌面时运行 `sh open-prepayment-tool.sh`；服务器可以直接运行 `bin/membership-prepare`。

原来的 **打开会话工具.cmd** 仍只负责登录恢复。用户已反馈旧版在本机成功测试两个自己的账号；新的整合付款前流程尚未获得真实网站验收，不能把登录成功等同于结账链路已验证。

## 一次付款前测试

1. 粘贴自己的完整 Session；应包含 `sessionToken` 及账号身份。只有 `accessToken` 不能用于本工具的 Cookie 恢复路径。
2. 填入 Zovo 的 `appId`（可选）、`appSecret` 和一张已存在的卡的本地数字 `cardId`。这些值只送入本机子进程的 stdin，不作为命令行参数。
3. 输入自己接受的预算下限、上限。当前自动入口限定 **Plus / PH / PHP**；工具显示的金额必须来自当次页面，示例预算不是实时售价。
4. 优先使用卡台返回的实际账单资料。若资料缺失，可补入这张卡实际登记的姓名和地址；不生成随机地址，不把套餐市场改写成持卡人地址。
5. 保持“显示独立浏览器”开启，启动付款前流程。工具创建临时 profile，恢复已有 Cookie、核验身份，然后在同一浏览器里进入结账。
6. 工具先填写账单资料并等待税额更新，核对套餐、币种和金额，再填写卡资料。最后再次读回字段和报价，停在最终付款按钮前。

程序没有“确认付款”命令，界面也没有付款按钮。观察期结束或取消后，它关闭自有浏览器并清理临时 profile。不要在观察窗口里手动点击付款：手工动作不属于此工具的自动化验收结果。

## 怎样判定结果

只有 `PREPAYMENT_REVIEW_READY`，且身份、Cookie 回读、账单与卡字段核验均成立，才表示自动流程到达付款前审核。最终结果还应显示正常关闭；中途取消、超时或清理失败不会继续报告最终成功。

报告会分别显示报价、卡状态、商户限制和余额核验结果。卡台没有提供余额币种时，余额是否足够保持未知，不能把卡上的数字直接与 PHP 总价比较。`readyForConfirmation` 表示页面和字段已就绪，不是扣款能力或支付成功保证。

已订阅账号会返回 `PREPAYMENT_ALREADY_SUBSCRIBED`。页面缺金额、税或折扣证据时会返回 `PREPAYMENT_QUOTE_INCOMPLETE`；若结账还需要激活“下一步”等控制，则返回 `PREPAYMENT_PROGRESSION_REQUIRED`。这些都不是通过，不能通过改标签、补零或重复点击来冒充完成。

所有结果都保持 `paymentAllowed:false`、`submitActivated:false`、`fundingActionsTaken:0`。当前测试工具只查卡，不自动开卡、加款或改变续费。

## CLI

完整输入格式见 `docs/PREPAYMENT_TOOL.md`。`your-input.json` 仅代表你自己选择的输入来源；也可由上游程序通过 stdin 直接传入，不必保存凭据文件。

```text
bin/membership-prepare --check < your-input.json
bin/membership-prepare --run --chrome-path PATH --show-browser --hold-open 2m < your-input.json
```

Windows 使用同名 `.exe`。`--check` 默认执行离线格式检查，不开浏览器、不联网。只有显式 `--run` 才执行恢复登录和付款前流程。

`--show-browser` 选择完整有窗口的 Chrome。在 Linux 私有 Xvfb 显示里运行同一模式可以无人值守，不需要有人盯着窗口。省略该参数是严格 headless；项目尚不能保证目标站点接受该模式。生产 worker 的数据库、锁、资金许可和对账机制仍独立保留，验收 CLI 不替代生产队列。

原有工具仍可使用：

```text
bin/membership-session --inspect < your-session.json
bin/membership-session --restore --chrome-path PATH --show-browser --hold-open 2m < your-session.json
bin/membership-review --example
```

`membership-review` 是离线报价审核器；示例为合成数据。`membership-preflight` 是旧观察入口，与新的 `membership-prepare` 不同。

## 可更换的卡台

源码新增独立 `zovocard` 适配器，覆盖基础查卡、开卡、加款、交易查询等接口，不依赖 Zovo GPT 代充或提取支付链接接口。当前付款前工具使用它的只读单卡快照能力。

开卡、加款属于另外的资金操作。它们的底层实现已经提供，但不会由此验收工具调用。未知结果进入核对状态，禁止自动重放；旧 worker 的库存流程仍需解除对商户价格服务的依赖，才能完整换成这个基础适配器。详见 `docs/ZOVO_BASIC_CARD_API.md`。

## 其他文件

- `docs/PREPAYMENT_INTEGRATION_STATUS.md`：本轮实现、验收范围和仍缺的现场证据。
- `docs/PREPAYMENT_BROWSER_CORE.md`：同一浏览器的模块衔接和页面证据要求。
- `userscript/session-login.user.js`：完整油猴替换文件；它不是新付款前工具的依赖。
- `SOURCE.json`：本包对应的源码提交及能力范围。

真实材料只进入当前进程和临时浏览器 profile；正常退出后清理。强制终止或断电仍可能留下临时文件。

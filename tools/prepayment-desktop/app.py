"""Local prepayment UI. Credentials only cross the child process stdin pipe.

No web server, clipboard reader, credential file, remote debugging attachment,
funding command or payment confirmation command is provided.
"""
from __future__ import annotations

import json
from datetime import datetime, timezone
import os
from pathlib import Path
import queue
import re
import shutil
import subprocess
import tempfile
import threading
from typing import Any

MAX_INPUT_BYTES = 384 << 10
MAX_REPORT_BYTES = 32 << 10
DIAGNOSTIC_VERSION = "20261114.1"
ALLOWED_CODES = set("""
PREPARATION_INPUT_CHECKED PREPARATION_FAILED PREPARATION_ARGUMENTS_INVALID
PREPARATION_INPUT_INVALID PREPARATION_INPUT_TOO_LARGE PREPARATION_CARD_CONFIGURATION_INVALID
PREPARATION_CARD_UNAVAILABLE PREPARATION_CARD_BINDING_MISMATCH PREPARATION_CARD_INVALID
PREPARATION_CARD_AUTH_FAILED PREPARATION_CARD_ACCESS_DENIED PREPARATION_CARD_RATE_LIMITED
PREPARATION_CARD_TIMEOUT PREPARATION_CARD_OPERATION_REJECTED
PREPARATION_CARD_EXPIRED PREPARATION_CARD_BLOCKED PREPARATION_BILLING_BINDING_MISMATCH
PREPARATION_BILLING_INVALID PREPARATION_BILLING_CONFLICT PREPARATION_BILLING_INCOMPLETE
PREPARATION_BALANCE_INSUFFICIENT PREPARATION_REPORT_INVALID PREPARATION_BROWSER_CONFIG_INVALID
PREPAYMENT_REVIEW_READY PREPAYMENT_FAILED PREPAYMENT_REQUEST_INVALID PREPAYMENT_TARGET_UNSUPPORTED
PREPAYMENT_BROWSER_UNAVAILABLE PREPAYMENT_BROWSER_CONFIG_INVALID PREPAYMENT_CHECKOUT_UNAVAILABLE
PREPAYMENT_ELIGIBILITY_UNCONFIRMED PREPAYMENT_ALREADY_SUBSCRIBED PREPAYMENT_IDENTITY_CHANGED
PREPAYMENT_CHECKOUT_CONTRACT_CHANGED PREPAYMENT_PAGE_UNSUPPORTED PREPAYMENT_ACTION_REQUIRED
PREPAYMENT_QUOTE_INCOMPLETE PREPAYMENT_QUOTE_CHANGED PREPAYMENT_MATERIAL_UNAVAILABLE
PREPAYMENT_MATERIAL_INVALID PREPAYMENT_BILLING_UNAVAILABLE PREPAYMENT_BILLING_CHANGED
PREPAYMENT_FIELDS_NOT_VERIFIED PREPAYMENT_PROGRESSION_REQUIRED PREPAYMENT_FINAL_CONTROL_UNCONFIRMED
PREPAYMENT_CANCELLED PREPAYMENT_TIMEOUT PREPAYMENT_CLEANUP_FAILED
PREPAYMENT_SUBMISSION_BLOCKED PREPAYMENT_FENCE_UNAVAILABLE
SESSION_INPUT_INVALID SESSION_INPUT_TOO_LARGE SESSION_TOKEN_MISSING SESSION_TOKEN_INVALID
SESSION_EXPIRED SESSION_IDENTITY_INVALID SESSION_IDENTITY_MISSING SESSION_BROWSER_CONFIG_INVALID
SESSION_BROWSER_UNAVAILABLE SESSION_COOKIE_WRITE_FAILED SESSION_COOKIE_READBACK_FAILED
SESSION_COOKIE_MISMATCH SESSION_NAVIGATION_FAILED SESSION_IDENTITY_UNAVAILABLE
SESSION_IDENTITY_UNCONFIRMED SESSION_IDENTITY_MISMATCH SESSION_NOT_LOGGED_IN SESSION_REJECTED
SESSION_SITE_DENIED SESSION_ORIGIN_MISMATCH SESSION_PROFILE_CLEANUP_FAILED SESSION_CANCELLED
SESSION_TIMEOUT SESSION_ARGUMENTS_INVALID SESSION_UNEXPECTED
QUOTE_FAILED QUOTE_INPUT_INVALID QUOTE_INPUT_TOO_LARGE QUOTE_ARGUMENTS_INVALID QUOTE_CANCELLED
QUOTE_TIMEOUT QUOTE_TARGET_UNSUPPORTED QUOTE_TARGET_MISMATCH QUOTE_INCOMPLETE QUOTE_AMOUNT_INVALID
QUOTE_AMOUNT_OVERFLOW QUOTE_TOTAL_MISMATCH QUOTE_BUDGET_INVALID QUOTE_OUTSIDE_BUDGET
""".split())
AMOUNT = re.compile(r"^(?:0|[1-9][0-9]{0,17})(?:\.[0-9]{1,2})?$")
PHASES = {
    "browser_starting": "正在启动本次任务的独立 Chrome…",
    "restoring_session": "正在恢复会话、回读 Cookie 并核对账号…",
    "opening_checkout": "正在建立 Plus 结账并检查页面…",
    "resolving_billing": "正在读取指定卡的实际账单资料…",
    "filling_billing": "正在填写实际账单信息，等待税费更新…",
    "reviewing_quote": "正在核对套餐、币种、税费与预算…",
    "resolving_card": "正在核对同一卡片资料与商户限制…",
    "filling_fields": "正在填写并精确回读卡片字段…",
    "checking_final_state": "正在复查最终金额、字段与确认按钮…",
    "review_ready": "已到付款前审核；本工具不会点击付款。",
    "browser_closed": "浏览器已关闭。",
}
STAGE_LABELS = {
    "input": "校验输入", "input_validated": "本地输入校验通过",
    "browser_starting": "启动独立浏览器", "restoring_session": "恢复会话并核对账号",
    "opening_checkout": "建立 Plus 结账", "reviewing_quote": "识别结账与核对报价",
    "resolving_billing": "读取卡台与实际账单资料", "filling_billing": "填写实际账单",
    "resolving_card": "核对卡片资料", "filling_fields": "填写并回读卡字段",
    "checking_final_state": "复查付款前状态", "review_ready": "付款前审核就绪",
    "browser_closed": "浏览器已关闭",
}
ERROR_HINTS = {
    "PREPARATION_CARD_AUTH_FAILED": "卡台返回 HTTP 401。请核对 App Secret，以及可选 App ID 是否属于同一套应用凭据。",
    "PREPARATION_CARD_ACCESS_DENIED": "卡台返回 HTTP 403。IP 白名单或 API 权限可能导致拒绝；仅凭 403 还不能确定是哪一种。请核对卡台请求的实际公网出口与白名单。",
    "PREPARATION_CARD_RATE_LIMITED": "卡台返回 HTTP 429（限流）。本次已停止，没有自动重试或换卡。",
    "PREPARATION_CARD_TIMEOUT": "读取卡台超时。请检查卡台 API 的网络与代理路由；超时本身不能证明是 IP 白名单拒绝。",
    "PREPARATION_CARD_OPERATION_REJECTED": "卡台返回业务拒绝。请在卡台后台核对凭据权限和卡片状态；这里不显示原始响应内容。",
    "PREPARATION_CARD_UNAVAILABLE": "卡台读取未完成，或未返回可识别的错误类别；尚不能据此确定为白名单问题。",
    "PREPAYMENT_ALREADY_SUBSCRIBED": "此账号已被识别为已有付费订阅，程序在创建新订阅之前停止。",
    "PREPAYMENT_CHECKOUT_UNAVAILABLE": "未能建立或打开结账；此步骤发生在读取 Zovo 卡片之前。",
    "PREPAYMENT_FENCE_UNAVAILABLE": "付款拦截保护未能持续确认，程序已停止并清理浏览器。",
}
UNCONFIRMED = {
    "card_balance_currency": "卡余额币种未由卡台明确返回",
    "card_balance_sufficiency": "卡余额是否足以付款尚未确认",
    "card_fx_conversion": "卡币种与结账币种的换算未确认",
    "merchant_restrictions": "卡台未明确返回商户限制规则",
}
QUOTE_FIELDS = ("minorUnitScale", "subtotalMinor", "taxMinor", "discountMinor", "totalMinor", "budgetMinMinor", "budgetMaxMinor")
BOOL_FIELDS = ("inputValid", "cookieReadbackVerified", "identityMatch", "billingVerified", "fieldsVerified",
               "readyForConfirmation", "browserOpened", "browserClosed", "paymentAllowed", "submitActivated", "submissionBlocked", "submissionFenceVerified")


class InputError(ValueError):
    pass


def no_duplicate_keys(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    result: dict[str, Any] = {}
    seen: set[str] = set()
    for key, value in pairs:
        normalized = key.lower()
        if normalized in seen:
            raise ValueError("duplicate key")
        seen.add(normalized)
        result[key] = value
    return result


def encode_input(session: str, settings: dict[str, str], billing: dict[str, str] | None = None) -> bytes:
    if not session.strip() or len(session.encode("utf-8")) > 256 << 10:
        raise InputError("请输入不超过 256 KiB 的完整 Session JSON。")
    try:
        parsed = json.loads(session, object_pairs_hook=no_duplicate_keys)
    except (ValueError, TypeError):
        raise InputError("Session JSON 无效或含重复字段；尚未启动程序。") from None
    if not isinstance(parsed, dict):
        raise InputError("Session 必须是 JSON 对象。")
    if not AMOUNT.fullmatch(settings.get("min", "")) or not AMOUNT.fullmatch(settings.get("max", "")):
        raise InputError("请填写 PHP 预算上下限，使用最多两位小数的金额。")
    card_id = settings.get("cardId", "")
    if not re.fullmatch(r"[1-9][0-9]{0,18}", card_id) or int(card_id) > 9223372036854775807:
        raise InputError("卡 ID 必须是卡台中已有卡片的正整数 ID。")
    platform_key = settings.get("platformKey", "")
    if not re.fullmatch(r"[A-Za-z0-9_.-]{1,128}", platform_key):
        raise InputError("卡台配置别名只能包含字母、数字、点、横线或下划线。")
    if not settings.get("appSecret"):
        raise InputError("请填写 Zovo App Secret。")
    payload: dict[str, Any] = {
        "session": parsed,
        "target": {"plan": "plus", "country": "PH", "currency": "PHP"},
        "budget": {"min": settings["min"], "max": settings["max"]},
        "card": {"platformKey": platform_key, "kind": "zovocard", "cardId": int(card_id),
                 "credential": {"appId": settings.get("appId", ""), "appSecret": settings["appSecret"]}},
    }
    if billing is not None:
        payload["billing"] = {"platformKey": platform_key, "cardId": int(card_id), "address": billing}
    encoded = json.dumps(payload, ensure_ascii=True, separators=(",", ":")).encode("ascii")
    if len(encoded) > MAX_INPUT_BYTES:
        raise InputError("规范化后的输入过大，已停止。")
    return encoded + b"\n"


def command(binary: str, chrome: str, execute: bool, show: bool) -> list[str]:
    if not Path(binary).is_file():
        raise InputError("请选择 membership-prepare 程序。")
    arguments = [str(Path(binary).resolve()), "--gui-protocol"]
    if execute:
        if not Path(chrome).is_file():
            raise InputError("请选择本机 Chrome 可执行文件。")
        arguments += ["--run", "--chrome-path", str(Path(chrome).resolve()), "--timeout", "3m"]
        if show:
            arguments += ["--show-browser", "--hold-open", "2m"]
    else:
        arguments += ["--check"]
    return arguments


def safe_report(value: Any) -> dict[str, Any]:
    if not isinstance(value, dict) or not isinstance(value.get("code"), str) or value["code"] not in ALLOWED_CODES:
        return {"code": "PREPARATION_REPORT_INVALID"}
    result: dict[str, Any] = {"code": value["code"]}
    if isinstance(value.get("stage"), str) and value["stage"] in STAGE_LABELS:
        result["stage"] = value["stage"]
    for key in BOOL_FIELDS:
        if type(value.get(key)) is bool:
            result[key] = value[key]
    if type(value.get("fundingActionsTaken")) is int and 0 <= value["fundingActionsTaken"] <= 100:
        result["fundingActionsTaken"] = value["fundingActionsTaken"]
    fingerprint = value.get("targetFingerprint")
    if isinstance(fingerprint, str) and re.fullmatch(r"[a-f0-9]{16}", fingerprint):
        result["targetFingerprint"] = fingerprint
    for key, allowed in (("evidenceSource", ("none", "visible_dom", "structured_checkout_session")),
                         ("countrySource", ("requested_checkout_market",))):
        if value.get(key) in allowed:
            result[key] = value[key]
    quote = value.get("quote")
    if isinstance(quote, dict) and (quote.get("plan"), quote.get("country"), quote.get("currency")) == ("plus", "PH", "PHP"):
        if all(type(quote.get(key)) is int and 0 <= quote[key] <= 9223372036854775807 for key in QUOTE_FIELDS) and type(quote.get("taxIncluded")) is bool:
            result["quote"] = {key: quote[key] for key in (*QUOTE_FIELDS, "plan", "country", "currency", "taxIncluded")}
    card = value.get("card")
    if isinstance(card, dict):
        clean: dict[str, Any] = {}
        if isinstance(card.get("last4"), str) and re.fullmatch(r"[0-9]{4}", card["last4"]):
            clean["last4"] = card["last4"]
        if isinstance(card.get("availableAmount"), str) and re.fullmatch(r"(?:0|[1-9][0-9]{0,20})(?:\.[0-9]{1,18})?", card["availableAmount"]):
            clean["availableAmount"] = card["availableAmount"]
        if isinstance(card.get("currency"), str) and re.fullmatch(r"(?:[A-Z]{3})?", card["currency"]):
            clean["currency"] = card["currency"]
        for key in ("cardBindingVerified", "balanceCurrencyKnown", "balanceComparable", "balanceSufficient", "merchantRestrictionsKnown"):
            if type(card.get(key)) is bool:
                clean[key] = card[key]
        if card.get("billingSource") in ("provider", "provider_and_bound_input"):
            clean["billingSource"] = card["billingSource"]
        result["card"] = clean
    for key, allowed in (("unconfirmed", UNCONFIRMED), ("missingFacts", ("plan", "country", "currency", "subtotal", "tax", "discount", "total", "taxIncluded"))):
        values = value.get(key)
        if isinstance(values, list):
            result[key] = [item for item in values[:16] if isinstance(item, str) and item in allowed]
            if key == "missingFacts" and len(result[key]) != len(values):
                result[key].append("unrecognized")
    return result


def diagnostic_report(value: Any, last_phase: str = "") -> dict[str, Any]:
    """Fixed diagnostic facts only; omit account, quote, card and credential data."""
    clean = safe_report(value)
    result = {key: clean[key] for key in ("code", "stage", *BOOL_FIELDS, "fundingActionsTaken") if key in clean}
    result["diagnosticVersion"] = DIAGNOSTIC_VERSION
    if isinstance(last_phase, str) and last_phase in PHASES:
        result["lastProgressStage"] = last_phase
    return result


def save_diagnostic(value: Any, last_phase: str = "", directory: Path | None = None) -> Path:
    """Persist only the minimal allowlisted result, using an atomic replacement."""
    report = diagnostic_report(value, last_phase)
    report["savedAtUTC"] = datetime.now(timezone.utc).isoformat(timespec="seconds")
    folder = directory if directory is not None else Path(__file__).resolve().parent / "logs"
    folder.mkdir(parents=True, exist_ok=True)
    target = folder / "last-diagnostic.json"
    temporary: Path | None = None
    try:
        with tempfile.NamedTemporaryFile(mode="w", encoding="utf-8", dir=folder, prefix=".diagnostic-", delete=False) as stream:
            temporary = Path(stream.name)
            json.dump(report, stream, ensure_ascii=True, indent=2)
            stream.write("\n")
        os.replace(temporary, target)
        temporary = None
        return target
    finally:
        if temporary is not None:
            try: temporary.unlink()
            except OSError: pass


def safe_event(raw: bytes) -> tuple[str, dict[str, Any]]:
    if len(raw) > MAX_REPORT_BYTES:
        return "invalid", {"code": "PREPARATION_REPORT_INVALID"}
    try:
        value = json.loads(raw, object_pairs_hook=no_duplicate_keys)
    except (ValueError, TypeError):
        return "invalid", {"code": "PREPARATION_REPORT_INVALID"}
    if not isinstance(value, dict) or value.get("event") not in ("ready", "result"):
        return "invalid", {"code": "PREPARATION_REPORT_INVALID"}
    return value["event"], safe_report(value.get("report"))


def review_complete(report: dict[str, Any], *, final: bool) -> bool:
    required = ("inputValid", "cookieReadbackVerified", "identityMatch", "billingVerified", "fieldsVerified", "readyForConfirmation", "browserOpened", "submissionFenceVerified")
    card, quote = report.get("card", {}), report.get("quote", {})
    if not isinstance(quote, dict) or not all(type(quote.get(key)) is int and quote[key] >= 0 for key in QUOTE_FIELDS):
        return False
    if type(quote.get("taxIncluded")) is not bool or quote["minorUnitScale"] != 2:
        return False
    subtotal, tax, discount, total = (quote[key] for key in ("subtotalMinor", "taxMinor", "discountMinor", "totalMinor"))
    if discount > subtotal or total != subtotal - discount + (0 if quote["taxIncluded"] else tax):
        return False
    if quote["taxIncluded"] and tax > total or not 0 < quote["budgetMinMinor"] <= total <= quote["budgetMaxMinor"]:
        return False
    if (quote.get("plan"), quote.get("country"), quote.get("currency")) != ("plus", "PH", "PHP"):
        return False
    if not isinstance(card, dict) or not isinstance(card.get("availableAmount"), str) or type(card.get("balanceCurrencyKnown")) is not bool:
        return False
    if card["balanceCurrencyKnown"] != bool(card.get("currency")) or type(card.get("balanceComparable")) is not bool:
        return False
    if card["balanceComparable"] and (card.get("currency") != "PHP" or card.get("balanceSufficient") is not True):
        return False
    if not card["balanceComparable"] and "balanceSufficient" in card:
        return False
    return (report.get("code") == "PREPAYMENT_REVIEW_READY" and all(report.get(key) is True for key in required)
            and report.get("paymentAllowed") is False and report.get("submitActivated") is False and report.get("submissionBlocked") is False
            and type(report.get("fundingActionsTaken")) is int and report["fundingActionsTaken"] == 0 and not report.get("missingFacts")
            and isinstance(report.get("targetFingerprint"), str) and re.fullmatch(r"[a-f0-9]{16}", report["targetFingerprint"]) is not None
            and card.get("cardBindingVerified") is True and bool(card.get("last4"))
            and report.get("evidenceSource") in ("visible_dom", "structured_checkout_session")
            and report.get("countrySource") == "requested_checkout_market"
            and quote.get("minorUnitScale") == 2 and isinstance(quote.get("totalMinor"), int)
            and (not final or report.get("browserClosed") is True))


def completion_message(report: dict[str, Any], exit_code: int) -> str:
    code = report.get("code", "PREPARATION_REPORT_INVALID")
    if exit_code == 0 and code == "PREPARATION_INPUT_CHECKED" and report.get("inputValid") and not report.get("browserOpened"):
        return "本地输入检查通过；尚未验证登录、卡台或真实结账。"
    if exit_code == 0 and review_complete(report, final=True):
        return "付款前准备完成，浏览器已关闭；未开卡、未加款、未提交付款。"
    if code == "PREPAYMENT_CANCELLED":
        return "已取消；本次未按正常完成结束。" + ("浏览器已关闭。" if report.get("browserClosed") else "浏览器清理尚未确认。")
    return "尚未完成付款前验收：" + str(code)


def review_text(report: dict[str, Any]) -> str:
    lines = ["状态：" + report.get("code", "PREPARATION_REPORT_INVALID")]
    stage = report.get("stage")
    if stage in STAGE_LABELS:
        lines.append("结果阶段：" + STAGE_LABELS[stage] + "（" + stage + "）")
    hint = ERROR_HINTS.get(report.get("code"))
    if hint:
        lines.append(hint)
    if report.get("readyForConfirmation") is not True and report.get("code") != "PREPARATION_INPUT_CHECKED":
        if stage in ("input", "input_validated", "browser_starting", "restoring_session", "opening_checkout"):
            lines.append("该阶段尚未查询 Zovo 卡台；本次停止不能归因于卡台请求被白名单拒绝。")
        if report.get("browserOpened") and report.get("browserClosed"):
            lines.append("失败后已立即关闭任务浏览器；两分钟观察仅在成功到达付款前审核后开始。")
    quote = report.get("quote")
    if quote and quote.get("minorUnitScale") == 2:
        money = lambda value: f"{value // 100}.{value % 100:02d}"
        lines += ["套餐：Plus；结账市场 PH；币种 PHP",
                  f"小计 {money(quote['subtotalMinor'])}　税费 {money(quote['taxMinor'])}　折扣 {money(quote['discountMinor'])}",
                  "最终总额：PHP " + money(quote["totalMinor"])]
    card = report.get("card")
    if card:
        lines += ["卡片尾号：" + card.get("last4", "未确认"),
                  "卡台返回余额：" + card.get("availableAmount", "未确认") + " " + (card.get("currency") or "（币种未确认）")]
    for item in report.get("unconfirmed", []):
        if item in UNCONFIRMED:
            lines.append("未确认：" + UNCONFIRMED[item])
    lines += ["本工具只准备并核对。没有付款按钮，也不会自动开卡或加款。", "", json.dumps(report, ensure_ascii=False, indent=2)]
    return "\n".join(lines)


def default_binary() -> str:
    name = "membership-prepare.exe" if os.name == "nt" else "membership-prepare"
    here = Path(__file__).resolve().parent
    return next((str(path) for path in (here / name, here.parents[1] / "bin" / name, here.parent / "bin" / name) if path.is_file()), "")


def default_chrome() -> str:
    paths = [shutil.which(name) for name in ("google-chrome", "chromium", "chromium-browser", "chrome")]
    for root in (os.environ.get("PROGRAMFILES"), os.environ.get("PROGRAMFILES(X86)"), os.environ.get("LOCALAPPDATA")):
        if root:
            paths.append(str(Path(root) / "Google/Chrome/Application/chrome.exe"))
    paths.append("/Applications/Google Chrome.app/Contents/MacOS/Google Chrome")
    return next((path for path in paths if path and Path(path).is_file()), "")


class Application:
    def __init__(self, root: Any, tk: Any, ttk: Any, filedialog: Any) -> None:
        self.root, self.tk, self.ttk, self.filedialog = root, tk, ttk, filedialog
        self.events: queue.Queue[tuple[str, Any]] = queue.Queue()
        self.process: subprocess.Popen[bytes] | None = None
        self.running = self.cancel_requested = self.cancel_sent = self.closing = False
        self.last_phase = ""
        self.last_diagnostic: dict[str, Any] = {}
        root.title("Anagnoraxis · 付款前准备")
        root.geometry("900x850")
        root.minsize(760, 720)
        outer = ttk.Frame(root, padding=18)
        outer.pack(fill="both", expand=True)
        ttk.Label(outer, text="Plus 付款前准备 · 诊断版 " + DIAGNOSTIC_VERSION, font=("Segoe UI", 18, "bold")).pack(anchor="w")
        ttk.Label(outer, text="当前支持 Plus / PH / PHP。恢复登录、查已有卡、填写并核对，停在最终付款之前。", wraplength=820).pack(anchor="w", pady=(6, 12))
        self.binary, self.chrome = tk.StringVar(value=default_binary()), tk.StringVar(value=default_chrome())
        for label, variable in (("程序", self.binary), ("Chrome", self.chrome)):
            row = ttk.Frame(outer); row.pack(fill="x", pady=3)
            ttk.Label(row, text=label, width=9).pack(side="left")
            ttk.Entry(row, textvariable=variable).pack(side="left", fill="x", expand=True, padx=6)
            ttk.Button(row, text="选择", command=lambda v=variable: self.pick(v)).pack(side="right")
        self.tabs = ttk.Notebook(outer); self.tabs.pack(fill="both", expand=True, pady=12)
        self.main_tab, self.billing_tab, self.result_tab = (ttk.Frame(self.tabs, padding=12) for _ in range(3))
        for frame, title in ((self.main_tab, "Session 与卡台"), (self.billing_tab, "实际账单（可选）"), (self.result_tab, "运行与审核结果")):
            self.tabs.add(frame, text=title)
        ttk.Label(self.main_tab, text="完整 Session JSON（只在本机内存中使用）").pack(anchor="w")
        self.input = tk.Text(self.main_tab, height=9, wrap="word", undo=False)
        self.input.pack(fill="both", expand=True, pady=(5, 12))
        self.settings: dict[str, Any] = {}
        for label, name, default in (("卡台配置别名", "platformKey", "zovo-primary"), ("Zovo App ID（可选）", "appId", ""),
                                     ("Zovo App Secret", "appSecret", ""), ("已有卡片 ID", "cardId", "")):
            row = ttk.Frame(self.main_tab); row.pack(fill="x", pady=3)
            variable = tk.StringVar(value=default); self.settings[name] = variable
            ttk.Label(row, text=label, width=22).pack(side="left")
            ttk.Entry(row, textvariable=variable, show="•" if name == "appSecret" else "").pack(side="left", fill="x", expand=True)
        row = ttk.Frame(self.main_tab); row.pack(fill="x", pady=(10, 4))
        for label, name in (("最低总额 PHP", "min"), ("最高总额 PHP", "max")):
            variable = tk.StringVar(value=""); self.settings[name] = variable
            ttk.Label(row, text=label).pack(side="left", padx=(0, 6))
            ttk.Entry(row, textvariable=variable, width=16).pack(side="left", padx=(0, 18))
        ttk.Label(self.main_tab, text="预算必须由你填写；这里不预填或猜测实际套餐价格。卡台只读查询，不会开卡、加款。", wraplength=780).pack(anchor="w", pady=6)
        self.use_billing = tk.BooleanVar(value=False)
        ttk.Checkbutton(self.billing_tab, text="补充这张卡的实际登记账单资料（仅补缺；与卡台返回冲突会停止）", variable=self.use_billing).pack(anchor="w")
        ttk.Label(self.billing_tab, text="账单国家填写发卡资料中的实际国家，不必与结账市场 PH 相同。资料自动绑定上页卡台别名与卡 ID。", wraplength=780).pack(anchor="w", pady=10)
        self.billing: dict[str, Any] = {}
        for label, name in (("登记姓名", "name"), ("地址第一行", "line1"), ("地址第二行", "line2"), ("城市", "city"), ("州/省", "state"), ("邮编", "postalCode"), ("实际国家（二位代码）", "country")):
            row = ttk.Frame(self.billing_tab); row.pack(fill="x", pady=5)
            variable = tk.StringVar(value=""); self.billing[name] = variable
            ttk.Label(row, text=label, width=22).pack(side="left")
            ttk.Entry(row, textvariable=variable).pack(side="left", fill="x", expand=True)
        self.result = tk.Text(self.result_tab, wrap="word", state="disabled", undo=False)
        self.result.pack(fill="both", expand=True)
        self.copy_button = ttk.Button(self.result_tab, text="复制脱敏诊断（不含账号或卡片资料）", command=self.copy_diagnostic, state="disabled")
        self.copy_button.pack(anchor="w", pady=(8, 0))
        self.show = tk.BooleanVar(value=True)
        ttk.Checkbutton(outer, text="显示独立 Chrome（到审核状态后最多保留 2 分钟，仅用于观察）", variable=self.show).pack(anchor="w")
        row = ttk.Frame(outer); row.pack(fill="x", pady=10)
        self.check_button = ttk.Button(row, text="仅检查输入", command=lambda: self.start(False)); self.check_button.pack(side="left")
        self.run_button = ttk.Button(row, text="运行至付款前", command=lambda: self.start(True)); self.run_button.pack(side="left", padx=10)
        self.cancel_button = ttk.Button(row, text="取消并清理", command=self.cancel, state="disabled"); self.cancel_button.pack(side="right")
        self.status = tk.StringVar(value="等待输入。程序不会提交付款，也不会更改续费。")
        ttk.Label(outer, textvariable=self.status, wraplength=820).pack(anchor="w")
        root.protocol("WM_DELETE_WINDOW", self.close)
        root.after(100, self.poll)

    def pick(self, variable: Any) -> None:
        chosen = self.filedialog.askopenfilename()
        if chosen: variable.set(chosen)

    def set_running(self, value: bool) -> None:
        self.running = value
        self.check_button.configure(state="disabled" if value else "normal")
        self.run_button.configure(state="disabled" if value else "normal")
        self.cancel_button.configure(state="normal" if value else "disabled")

    def start(self, execute: bool) -> None:
        if self.running: return
        try:
            payload = encode_input(self.input.get("1.0", "end-1c"), {name: var.get() for name, var in self.settings.items()},
                                   {name: var.get() for name, var in self.billing.items()} if self.use_billing.get() else None)
            args = command(self.binary.get(), self.chrome.get(), execute, bool(self.show.get()))
        except InputError as error:
            self.status.set(str(error)); return
        self.cancel_requested = self.cancel_sent = False
        self.process = None
        self.last_phase = ""
        self.last_diagnostic = {}
        self.copy_button.configure(state="disabled")
        self.set_running(True)
        self.input.delete("1.0", "end")
        self.settings["appSecret"].set("")
        self.settings["appId"].set("")
        self.display({})
        self.status.set("正在启动本地程序…")
        threading.Thread(target=self.worker, args=(args, payload), daemon=True).start()

    def worker(self, args: list[str], payload: bytes) -> None:
        process = None
        report = {"code": "PREPARATION_FAILED"}
        exit_code = 1
        try:
            process = subprocess.Popen(args, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                                       shell=False, creationflags=getattr(subprocess, "CREATE_NO_WINDOW", 0) if os.name == "nt" else 0)
            assert process.stdin and process.stdout and process.stderr
            process.stdin.write(payload); process.stdin.flush(); payload = b""
            self.events.put(("started", process))
            def read_progress() -> None:
                assert process is not None and process.stderr is not None
                while True:
                    line = process.stderr.readline(4096)
                    if not line: return
                    text = line.decode("ascii", errors="ignore").strip()
                    if text.startswith("PREPARATION_STAGE ") and text[18:] in PHASES:
                        self.events.put(("phase", text[18:]))
            threading.Thread(target=read_progress, daemon=True).start()
            final_received = ready_received = False
            while True:
                line = process.stdout.readline(MAX_REPORT_BYTES + 1)
                if not line: break
                kind, value = safe_event(line)
                if kind == "ready" and not final_received and not ready_received and review_complete(value, final=False):
                    ready_received = True
                    self.events.put(("ready", value))
                elif kind == "result" and not final_received:
                    report, final_received = value, True
                else:
                    report = {"code": "PREPARATION_REPORT_INVALID"}
                    process.stdin.close()
                    break
            exit_code = process.wait()
            if not final_received:
                report = {"code": "PREPARATION_REPORT_INVALID"}
        except (OSError, ValueError, AssertionError):
            report = {"code": "PREPARATION_FAILED"}
        finally:
            payload = b""
            if process is not None:
                if process.stdin:
                    try: process.stdin.close()
                    except OSError: pass
                if process.poll() is None:
                    try:
                        process.wait()
                    except OSError:
                        report = {"code": "PREPAYMENT_CLEANUP_FAILED"}
            self.events.put(("done", {"report": report, "exitCode": exit_code}))

    def send_cancel(self) -> None:
        if self.process is None or self.cancel_sent: return
        self.cancel_sent = True
        try:
            if self.process.stdin:
                self.process.stdin.write(b"cancel\n"); self.process.stdin.flush()
        except (OSError, ValueError): pass

    def cancel(self) -> None:
        if not self.running: return
        self.cancel_requested = True
        self.status.set("正在取消并等待浏览器清理…")
        self.send_cancel()

    def close(self) -> None:
        if self.running:
            self.closing = True; self.cancel()
        else:
            self.root.destroy()

    def display(self, report: dict[str, Any]) -> None:
        self.result.configure(state="normal"); self.result.delete("1.0", "end")
        if report: self.result.insert("1.0", review_text(report))
        self.result.configure(state="disabled")

    def copy_diagnostic(self) -> None:
        if not self.last_diagnostic:
            return
        self.root.clipboard_clear()
        self.root.clipboard_append(json.dumps(self.last_diagnostic, ensure_ascii=False, indent=2))
        self.status.set("已复制脱敏诊断：只有固定错误码、阶段和校验标志，不含 Session、API Secret、账号、金额或卡片资料。")

    def poll(self) -> None:
        while True:
            try: kind, value = self.events.get_nowait()
            except queue.Empty: break
            if kind == "started":
                self.process = value
                if self.cancel_requested: self.send_cancel()
            elif kind == "phase" and self.running and not self.cancel_requested:
                self.last_phase = value
                self.status.set(PHASES[value])
            elif kind == "ready" and not self.cancel_requested:
                self.display(value); self.tabs.select(self.result_tab)
                self.status.set("已到付款前审核，未提交。请查看最终总额与未确认项；可取消关闭。")
            elif kind == "done":
                self.set_running(False); self.process = None
                self.display(value["report"]); self.tabs.select(self.result_tab)
                self.last_diagnostic = diagnostic_report(value["report"], self.last_phase)
                self.copy_button.configure(state="normal")
                message = completion_message(value["report"], value["exitCode"])
                try:
                    save_diagnostic(value["report"], self.last_phase)
                    message += " 脱敏诊断已保存在本工具 logs/last-diagnostic.json。"
                except OSError:
                    message += " 诊断文件未能保存；仍可用上方按钮复制脱敏诊断。"
                self.status.set(message)
                if self.closing: self.root.destroy(); return
        self.root.after(100, self.poll)


def main() -> None:
    try:
        import tkinter as tk
        from tkinter import filedialog, ttk
    except ImportError:
        raise SystemExit("需要带 Tkinter 的 Python 3，或直接使用 membership-prepare CLI。") from None
    root = tk.Tk()
    Application(root, tk, ttk, filedialog)
    root.mainloop()


if __name__ == "__main__":
    main()

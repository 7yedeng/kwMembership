"""Small local UI for membership-session; all session logic stays in Go.

No web server, clipboard access, credential file or third-party dependency.
The child process receives the Session only through its private stdin pipe.
"""
from __future__ import annotations

import json
import os
from pathlib import Path
import queue
import re
import shutil
import subprocess
import threading
from typing import Any

MAX_SESSION_BYTES = 256 << 10
MAX_REPORT_BYTES = 16 << 10
CODE = re.compile(r"^SESSION_[A-Z_]{1,64}$")
PHASES = {
    "browser_starting": "正在打开独立浏览器…",
    "cookie_writing": "正在恢复已有会话…",
    "cookie_readback": "正在核对 Cookie 写入结果…",
    "navigating": "正在打开账号页面…",
    "identity_checking": "正在核对实际登录账号…",
    "identity_verified": "指定账号核验通过。",
    "browser_visible": "核验通过；可在独立浏览器中查看，最多保留两分钟。",
    "closing_browser": "正在关闭浏览器并清理临时资料…",
    "browser_closed": "独立浏览器已关闭。",
}


class InputError(ValueError):
    pass


def encode_session(value: str) -> bytes:
    """Collapse arbitrary pasted JSON whitespace into one private protocol line."""
    if not value.strip():
        raise InputError("请输入完整 Session JSON。")
    if len(value.encode("utf-8")) > MAX_SESSION_BYTES:
        raise InputError("输入超过 256 KiB，已停止。")
    try:
        parsed = json.loads(value)
    except (ValueError, TypeError):
        raise InputError("输入不是有效 JSON；没有启动浏览器。") from None
    if not isinstance(parsed, dict):
        raise InputError("Session 必须是 JSON 对象。")
    encoded = json.dumps(parsed, ensure_ascii=True, separators=(",", ":")).encode("ascii")
    if len(encoded) > MAX_SESSION_BYTES:
        raise InputError("规范化后的输入超过 256 KiB，已停止。")
    return encoded + b"\n"


def command(binary: str, chrome: str, restore: bool, show: bool) -> list[str]:
    if not Path(binary).is_file():
        raise InputError("请选择已构建的 membership-session 程序。")
    args = [str(Path(binary).resolve()), "--gui-protocol"]
    if restore:
        if not Path(chrome).is_file():
            raise InputError("请选择 Chrome 或 Chromium 的可执行文件。")
        args += ["--restore", "--chrome-path", str(Path(chrome).resolve()), "--timeout", "90s"]
        if show:
            args += ["--show-browser", "--hold-open", "2m"]
    else:
        args.append("--inspect")
    return args


def safe_report(raw: bytes) -> dict[str, Any]:
    if len(raw) > MAX_REPORT_BYTES:
        return {"code": "SESSION_UNEXPECTED"}
    try:
        value = json.loads(raw)
    except (ValueError, TypeError):
        return {"code": "SESSION_UNEXPECTED"}
    if not isinstance(value, dict) or not isinstance(value.get("code"), str) or not CODE.fullmatch(value["code"]):
        return {"code": "SESSION_UNEXPECTED"}
    result: dict[str, Any] = {"code": value["code"]}
    for key in ("remoteVerified", "cookieReadbackVerified", "identityMatch", "userIdRequired", "browserOpened", "profileRemoved", "paymentAllowed"):
        if type(value.get(key)) is bool:
            result[key] = value[key]
    fingerprint = value.get("targetFingerprint", "")
    if isinstance(fingerprint, str) and re.fullmatch(r"[a-f0-9]{16}", fingerprint):
        result["targetFingerprint"] = fingerprint
    return result


def completion_message(report: dict[str, Any], exit_code: int) -> str:
    complete = (exit_code == 0 and report.get("code") == "SESSION_VERIFIED" and
                all(report.get(key) is True for key in ("remoteVerified", "identityMatch", "cookieReadbackVerified", "profileRemoved")))
    if complete:
        return "指定账号已通过真实身份核验，独立浏览器已关闭并清理。"
    code = report.get("code", "SESSION_UNEXPECTED")
    if exit_code == 0 and code == "SESSION_INSPECTED" and not report.get("remoteVerified"):
        return "输入结构检查通过；尚未验证真实登录。"
    if code == "SESSION_CANCELLED":
        observed = "取消前曾核对到指定账号" if report.get("identityMatch") else "尚未完成账号核验"
        cleanup = "临时资料已清理" if report.get("profileRemoved") else "临时资料清理未确认"
        return "任务已取消：" + observed + "；" + cleanup + "。本次未按正常完成结束。"
    if code == "SESSION_PROFILE_CLEANUP_FAILED":
        return "临时浏览器资料清理失败；即使身份曾匹配，也未通过整体验收。"
    return "未完成真实登录验收：" + str(code)


def default_binary() -> str:
    name = "membership-session.exe" if os.name == "nt" else "membership-session"
    here = Path(__file__).resolve().parent
    for candidate in (here / name, here.parents[1] / "bin" / name, here.parent / "bin" / name):
        if candidate.is_file():
            return str(candidate)
    return ""


def default_chrome() -> str:
    candidates = []
    for name in ("chrome", "google-chrome", "chromium", "chromium-browser", "msedge"):
        found = shutil.which(name)
        if found:
            candidates.append(found)
    for root in (os.environ.get("PROGRAMFILES", ""), os.environ.get("PROGRAMFILES(X86)", ""), os.environ.get("LOCALAPPDATA", "")):
        if root:
            candidates.extend((str(Path(root) / "Google/Chrome/Application/chrome.exe"), str(Path(root) / "Microsoft/Edge/Application/msedge.exe")))
    candidates.append("/Applications/Google Chrome.app/Contents/MacOS/Google Chrome")
    return next((path for path in candidates if Path(path).is_file()), "")


class Application:
    def __init__(self, root: Any, tk: Any, ttk: Any, filedialog: Any) -> None:
        self.root, self.tk, self.ttk, self.filedialog = root, tk, ttk, filedialog
        self.events: queue.Queue[tuple[str, Any]] = queue.Queue()
        self.process: subprocess.Popen[bytes] | None = None
        self.running = False
        self.cancel_requested = False
        self.cancel_sent = False
        self.closing = False
        root.title("Anagnoraxis · 会话恢复验收")
        root.geometry("780x700")
        root.minsize(660, 600)
        outer = ttk.Frame(root, padding=20)
        outer.pack(fill="both", expand=True)
        ttk.Label(outer, text="会话恢复验收", font=("Segoe UI", 18, "bold")).pack(anchor="w")
        ttk.Label(outer, text="只恢复并核对本人账号。本工具不会创建结账、绑卡、付款或修改续费。", wraplength=700).pack(anchor="w", pady=(8, 16))
        self.binary = tk.StringVar(value=default_binary())
        self.chrome = tk.StringVar(value=default_chrome())
        for label, variable in (("程序位置", self.binary), ("浏览器位置", self.chrome)):
            row = ttk.Frame(outer)
            row.pack(fill="x", pady=4)
            ttk.Label(row, text=label, width=12).pack(side="left")
            ttk.Entry(row, textvariable=variable).pack(side="left", fill="x", expand=True, padx=6)
            ttk.Button(row, text="选择", command=lambda v=variable: self.pick(v)).pack(side="right")
        ttk.Label(outer, text="完整 Session JSON", font=("Segoe UI", 11, "bold")).pack(anchor="w", pady=(16, 4))
        ttk.Label(outer, text="在下方粘贴。只有 accessToken 的 JSON 不能替代缺失的 sessionToken。", wraplength=700).pack(anchor="w", pady=(0, 6))
        self.input = tk.Text(outer, height=11, wrap="word", undo=False)
        self.input.pack(fill="both", expand=True)
        self.show = tk.BooleanVar(value=True)
        ttk.Checkbutton(outer, text="显示独立浏览器（核验通过后保留最多两分钟，可随时取消关闭）", variable=self.show).pack(anchor="w", pady=10)
        buttons = ttk.Frame(outer)
        buttons.pack(fill="x")
        self.inspect_button = ttk.Button(buttons, text="仅检查输入", command=lambda: self.start(False))
        self.inspect_button.pack(side="left")
        self.restore_button = ttk.Button(buttons, text="恢复登录并核验", command=lambda: self.start(True))
        self.restore_button.pack(side="left", padx=10)
        self.cancel_button = ttk.Button(buttons, text="取消并关闭浏览器", command=self.cancel, state="disabled")
        self.cancel_button.pack(side="right")
        self.status = tk.StringVar(value="等待输入。先做本地检查，再进行真实登录验收。")
        ttk.Label(outer, textvariable=self.status, wraplength=700).pack(anchor="w", pady=(14, 6))
        self.result = tk.Text(outer, height=7, wrap="word", state="disabled", undo=False)
        self.result.pack(fill="x")
        ttk.Label(outer, text="Session 不另存文件、不进入命令行参数。临时浏览器资料会在退出时清理。", wraplength=700).pack(anchor="w", pady=(10, 0))
        root.protocol("WM_DELETE_WINDOW", self.close)
        root.after(100, self.poll)

    def pick(self, variable: Any) -> None:
        chosen = self.filedialog.askopenfilename()
        if chosen:
            variable.set(chosen)

    def set_running(self, value: bool) -> None:
        self.running = value
        state = "disabled" if value else "normal"
        self.inspect_button.configure(state=state)
        self.restore_button.configure(state=state)
        self.cancel_button.configure(state="normal" if value else "disabled")

    def start(self, restore: bool) -> None:
        if self.running:
            return
        try:
            payload = encode_session(self.input.get("1.0", "end-1c"))
            args = command(self.binary.get(), self.chrome.get(), restore, bool(self.show.get()))
        except InputError as error:
            self.status.set(str(error))
            return
        self.cancel_requested = self.cancel_sent = False
        self.process = None
        self.set_running(True)
        self.status.set("正在启动本地程序…")
        # Remove the visible copy immediately after taking the in-memory input.
        self.input.delete("1.0", "end")
        self.display({})
        threading.Thread(target=self.worker, args=(args, payload), daemon=True).start()

    def worker(self, args: list[str], payload: bytes) -> None:
        process = None
        report = {"code": "SESSION_BROWSER_UNAVAILABLE"}
        exit_code = 1
        try:
            process = subprocess.Popen(args, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                                       shell=False, creationflags=getattr(subprocess, "CREATE_NO_WINDOW", 0) if os.name == "nt" else 0)
            assert process.stdin and process.stdout and process.stderr
            process.stdin.write(payload)
            process.stdin.flush()
            payload = b""
            self.events.put(("started", process))
            def read_progress() -> None:
                assert process is not None and process.stderr is not None
                while True:
                    line = process.stderr.readline(4096)
                    if not line:
                        return
                    text = line.decode("ascii", errors="ignore").strip()
                    if text.startswith("SESSION_STAGE "):
                        phase = text[len("SESSION_STAGE "):]
                        if phase in PHASES:
                            self.events.put(("phase", phase))
            threading.Thread(target=read_progress, daemon=True).start()
            raw = process.stdout.read(MAX_REPORT_BYTES + 1)
            exit_code = process.wait()
            report = safe_report(raw)
        except (OSError, ValueError, AssertionError):
            # Never display raw OS or child errors; they may contain a path or material.
            report = {"code": "SESSION_BROWSER_UNAVAILABLE"}
        finally:
            payload = b""
            if process is not None:
                if process.stdin:
                    try:
                        process.stdin.close()  # EOF requests graceful cancellation.
                    except OSError:
                        pass
                # If input/output failed, closing stdin requests cancellation.
                # Do not announce completion while our child still owns Chrome.
                if process.poll() is None:
                    try:
                        process.wait()
                    except OSError:
                        report = {"code": "SESSION_PROFILE_CLEANUP_FAILED"}
            self.events.put(("done", {"report": report, "exitCode": exit_code}))

    def send_cancel(self) -> None:
        if self.process is None or self.cancel_sent:
            return
        self.cancel_sent = True
        try:
            if self.process.stdin:
                self.process.stdin.write(b"cancel\n")
                self.process.stdin.flush()
        except OSError:
            pass

    def cancel(self) -> None:
        if not self.running:
            return
        self.cancel_requested = True
        self.status.set("正在取消任务并清理独立浏览器，请稍候…")
        self.send_cancel()

    def close(self) -> None:
        if self.running:
            self.closing = True
            self.cancel()
        else:
            self.root.destroy()

    def display(self, report: dict[str, Any]) -> None:
        self.result.configure(state="normal")
        self.result.delete("1.0", "end")
        if report:
            self.result.insert("1.0", json.dumps(report, ensure_ascii=False, indent=2))
        self.result.configure(state="disabled")

    def poll(self) -> None:
        while True:
            try:
                kind, value = self.events.get_nowait()
            except queue.Empty:
                break
            if kind == "started":
                self.process = value
                if self.cancel_requested:
                    self.send_cancel()
            elif kind == "phase" and not self.cancel_requested:
                self.status.set(PHASES[value])
            elif kind == "done":
                self.set_running(False)
                self.process = None
                self.display(value["report"])
                self.status.set(completion_message(value["report"], value["exitCode"]))
                if self.closing:
                    self.root.destroy()
                    return
        self.root.after(100, self.poll)


def main() -> None:
    try:
        import tkinter as tk
        from tkinter import filedialog, ttk
    except ImportError:
        raise SystemExit("此 Python 未安装 Tkinter；请使用带 Tk 的 Python，或直接运行 membership-session CLI。") from None
    root = tk.Tk()
    Application(root, tk, ttk, filedialog)
    root.mainloop()


if __name__ == "__main__":
    main()

from __future__ import annotations

import argparse
import logging
import os
import socket
import time
from typing import Any

from .client import ExecutorAPIError, ExecutorClient, ExecutorLease
from .live import LiveExecutor


LOGGER = logging.getLogger("kwmembership.python_executor")
ADAPTER_VERSION = "python-session-card-checkout-v1"


class FixtureExecutor:
    """No-charge executor used for queue and contract qualification."""

    def execute(self, client: ExecutorClient, lease: ExecutorLease) -> None:
        if lease.command_kind != "preflight":
            raise ExecutorAPIError("FIXTURE_PAYMENT_DISABLED", 409)
        material = client.material(lease)
        if lease.adapter_version != ADAPTER_VERSION:
            raise ExecutorAPIError("EXECUTOR_ADAPTER_MISMATCH", 409)
        if not isinstance(material.get("session"), dict):
            raise ExecutorAPIError("SESSION_INVALID", 409)
        contract = material.get("checkoutContract")
        if contract != {
            "country": "PH",
            "currency": "PHP",
            "uiMode": "hosted",
            "entryPoint": "all_plans_pricing_modal",
            "allowedOrigins": ["https://chatgpt.com", "https://pay.openai.com"],
        }:
            raise ExecutorAPIError("CHECKOUT_CONTRACT_INVALID", 409)

        page = _fixture_page(lease)
        validated = client.page_facts(lease, page)["page"]
        client.report(
            lease,
            "success",
            validated,
            diagnostic={"phase": "fixture", "stateId": str(validated["stateId"]), "status": "passed"},
        )


def _fixture_page(lease: ExecutorLease) -> dict[str, Any]:
    amount = float(lease.price_contract["MinAmount"])
    plan = "plus" if lease.stage == "plus" else ("prolite" if lease.target_tier == "x5" else "pro")
    return {
        "stateId": "PAYMENT_FINAL_READY",
        "origin": "https://pay.openai.com",
        "routeTemplate": "/pay/{id}",
        "plan": plan,
        "country": "PH",
        "currency": "PHP",
        "displayedAmount": amount,
        "stateMarker": "review",
        "fields": {
            "cardNumber": lease.command_kind == "payment",
            "expiry": lease.command_kind == "payment",
            "cvc": lease.command_kind == "payment",
            "billingName": lease.command_kind == "payment",
            "billingCountry": lease.command_kind == "payment",
            "billingPostal": lease.command_kind == "payment",
        },
        "controls": {"submit": "payment-submit" if lease.command_kind == "payment" else ""},
        "structuralHash": "",
    }


def run_forever(client: ExecutorClient, mode: str, poll_seconds: float = 1.0) -> None:
    executor = FixtureExecutor() if mode == "fixture" else LiveExecutor()
    while True:
        lease = client.lease()
        if lease is None:
            time.sleep(poll_seconds)
            continue
        started = time.monotonic()
        try:
            executor.execute(client, lease)
            LOGGER.info("execution=%s stage=%s status=reported elapsed_ms=%d", lease.execution_id, lease.stage, int((time.monotonic() - started) * 1000))
        except ExecutorAPIError as error:
            LOGGER.warning("execution=%s stage=%s status=failed code=%s", lease.execution_id, lease.stage, error.code)
            try:
                client.report(lease, "failed", error_code=error.code, diagnostic={"phase": "executor", "errorCode": error.code, "status": "failed"})
            except ExecutorAPIError:
                pass
        except Exception:
            code = "EXECUTOR_RUNTIME_FAILURE"
            LOGGER.warning("execution=%s stage=%s status=failed code=%s", lease.execution_id, lease.stage, code)
            try:
                client.report(lease, "failed", error_code=code, diagnostic={"phase": "executor", "errorCode": code, "status": "failed"})
            except ExecutorAPIError:
                pass


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--once", action="store_true")
    args = parser.parse_args()
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")
    mode = os.environ.get("KWMEMBERSHIP_PYTHON_EXECUTOR_MODE", "fixture").strip()
    if mode not in {"fixture", "live"}:
        raise SystemExit("KWMEMBERSHIP_PYTHON_EXECUTOR_MODE must be fixture or live")
    if mode == "live" and os.environ.get("KWMEMBERSHIP_LIVE_PAYMENT_ENABLED") != "true":
        raise SystemExit("live payment remains disabled")
    client = ExecutorClient(
        os.environ.get("KWMEMBERSHIP_EXECUTOR_URL", "http://127.0.0.1:4312"),
        os.environ["KWMEMBERSHIP_EXECUTOR_SECRET"],
        os.environ.get("KWMEMBERSHIP_EXECUTOR_ID", f"python-{socket.gethostname()}-{os.getpid()}"),
    )
    if args.once:
        lease = client.lease()
        if lease is not None:
            (FixtureExecutor() if mode == "fixture" else LiveExecutor()).execute(client, lease)
        return
    run_forever(client, mode)


if __name__ == "__main__":
    main()

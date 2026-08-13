from __future__ import annotations

import json
import urllib.error
import urllib.request
from dataclasses import dataclass
from typing import Any


class ExecutorAPIError(RuntimeError):
    def __init__(self, code: str, status: int) -> None:
        super().__init__(code)
        self.code = code
        self.status = status


@dataclass(frozen=True)
class ExecutorLease:
    execution_id: str
    executor_id: str
    lease_epoch: int
    lease_token: str
    hard_deadline_at: str
    command_kind: str
    stage: str
    attempt_no: int
    target_tier: str
    adapter_version: str
    price_contract: dict[str, Any]


class ExecutorClient:
    def __init__(self, base_url: str, secret: str, executor_id: str, timeout: float = 10.0) -> None:
        if not base_url.startswith("http://127.0.0.1:") and not base_url.startswith("http://[::1]:"):
            raise ValueError("executor API must use a loopback HTTP address")
        if len(secret) < 32 or any(char.isspace() for char in secret):
            raise ValueError("executor secret is invalid")
        if not executor_id or len(executor_id) > 100:
            raise ValueError("executor id is invalid")
        self._base_url = base_url.rstrip("/")
        self._secret = secret
        self._executor_id = executor_id
        self._timeout = timeout

    def lease(self) -> ExecutorLease | None:
        status, payload = self._request("/internal/v1/executions/lease", {"executorId": self._executor_id})
        if status == 204:
            return None
        return ExecutorLease(
            execution_id=str(payload["executionId"]),
            executor_id=str(payload["executorId"]),
            lease_epoch=int(payload["leaseEpoch"]),
            lease_token=str(payload["leaseToken"]),
            hard_deadline_at=str(payload["hardDeadlineAt"]),
            command_kind=str(payload["commandKind"]),
            stage=str(payload["stage"]),
            attempt_no=int(payload["attemptNo"]),
            target_tier=str(payload["targetTier"]),
            adapter_version=str(payload["adapterVersion"]),
            price_contract=dict(payload["priceContract"]),
        )

    def heartbeat(self, lease: ExecutorLease) -> dict[str, Any]:
        return self._leased_request(lease, "heartbeat", {})

    def material(self, lease: ExecutorLease) -> dict[str, Any]:
        return self._leased_request(lease, "material", {})

    def page_facts(self, lease: ExecutorLease, page: dict[str, Any]) -> dict[str, Any]:
        return self._leased_request(lease, "page-facts", {"page": page})

    def prepare_action(
        self, lease: ExecutorLease, kind: str, control_id: str, page: dict[str, Any]
    ) -> dict[str, Any]:
        return self._leased_request(
            lease, "actions/prepare", {"kind": kind, "controlId": control_id, "page": page}
        )

    def activate_action(self, lease: ExecutorLease, permit_id: str) -> dict[str, Any]:
        return self._leased_request(lease, f"actions/{permit_id}/activate", {})

    def action_result(self, lease: ExecutorLease, permit_id: str, outcome: str) -> dict[str, Any]:
        return self._leased_request(lease, f"actions/{permit_id}/result", {"outcome": outcome})

    def handoff(
        self, lease: ExecutorLease, handoff_type: str, page: dict[str, Any], diagnostic: dict[str, str]
    ) -> dict[str, Any]:
        return self._leased_request(
            lease, "handoff", {"type": handoff_type, "page": page, "diagnostic": diagnostic}
        )

    def report(
        self,
        lease: ExecutorLease,
        status: str,
        page: dict[str, Any] | None = None,
        error_code: str = "",
        diagnostic: dict[str, str] | None = None,
    ) -> dict[str, Any]:
        return self._leased_request(
            lease,
            "result",
            {
                "status": status,
                "errorCode": error_code,
                "page": page or {},
                "diagnostic": diagnostic or {},
            },
        )

    def _leased_request(self, lease: ExecutorLease, suffix: str, payload: dict[str, Any]) -> dict[str, Any]:
        _, response = self._request(
            f"/internal/v1/executions/{lease.execution_id}/{suffix}",
            payload,
            {
                "X-Executor-ID": lease.executor_id,
                "X-Lease-Epoch": str(lease.lease_epoch),
                "X-Lease-Token": lease.lease_token,
            },
        )
        return response

    def _request(
        self, path: str, payload: dict[str, Any], headers: dict[str, str] | None = None
    ) -> tuple[int, dict[str, Any]]:
        encoded = json.dumps(payload, separators=(",", ":")).encode("utf-8")
        request = urllib.request.Request(
            self._base_url + path,
            data=encoded,
            method="POST",
            headers={
                "Authorization": f"Bearer {self._secret}",
                "Content-Type": "application/json",
                **(headers or {}),
            },
        )
        try:
            with urllib.request.urlopen(request, timeout=self._timeout) as response:
                status = response.status
                raw = response.read(65537)
        except urllib.error.HTTPError as error:
            status = error.code
            raw = error.read(65537)
        except (urllib.error.URLError, TimeoutError, OSError) as error:
            raise ExecutorAPIError("EXECUTOR_REQUEST_UNAVAILABLE", 503) from error
        if len(raw) > 65536:
            raise ExecutorAPIError("EXECUTOR_RESPONSE_TOO_LARGE", status)
        if status == 204:
            return status, {}
        try:
            parsed = json.loads(raw)
        except (UnicodeDecodeError, json.JSONDecodeError) as error:
            raise ExecutorAPIError("EXECUTOR_RESPONSE_INVALID", status) from error
        if not isinstance(parsed, dict):
            raise ExecutorAPIError("EXECUTOR_RESPONSE_INVALID", status)
        if status < 200 or status >= 300:
            raise ExecutorAPIError(str(parsed.get("code") or "EXECUTOR_REQUEST_FAILED"), status)
        return status, parsed

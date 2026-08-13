from __future__ import annotations

import json
import threading
import unittest
import urllib.error
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from unittest import mock

from python_executor.client import ExecutorAPIError, ExecutorClient


SECRET = "python-executor-test-secret-000000000001"


class Handler(BaseHTTPRequestHandler):
    requests: list[dict[str, object]] = []

    def do_POST(self) -> None:
        length = int(self.headers.get("Content-Length", "0"))
        body = json.loads(self.rfile.read(length) or b"{}")
        self.requests.append({"path": self.path, "headers": dict(self.headers), "body": body})
        if self.path.endswith("/lease"):
            payload = {
                "executionId": "exec-1",
                "executorId": "python-test",
                "leaseEpoch": 4,
                "leaseToken": "opaque-token",
                "hardDeadlineAt": "2026-08-13T00:05:00Z",
                "commandKind": "preflight",
                "stage": "plus",
                "attemptNo": 0,
                "targetTier": "plus",
                "adapterVersion": "python-session-card-checkout-v1",
                "priceContract": {"ID": "price", "Version": 1, "MinAmount": 1000},
            }
            self.send_response(200)
        else:
            payload = {"accepted": True}
            self.send_response(202)
        raw = json.dumps(payload).encode()
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def log_message(self, _format: str, *_args: object) -> None:
        return


class ClientTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        cls.thread = threading.Thread(target=cls.server.serve_forever, daemon=True)
        cls.thread.start()

    @classmethod
    def tearDownClass(cls) -> None:
        cls.server.shutdown()
        cls.server.server_close()

    def setUp(self) -> None:
        Handler.requests.clear()
        self.client = ExecutorClient(
            f"http://127.0.0.1:{self.server.server_port}", SECRET, "python-test"
        )

    def test_lease_and_bound_result_headers(self) -> None:
        lease = self.client.lease()
        self.assertIsNotNone(lease)
        self.client.report(lease, "failed", error_code="FIXTURE_FAILURE")
        report = Handler.requests[-1]
        headers = report["headers"]
        self.assertEqual(headers["X-Executor-Id"], "python-test")
        self.assertEqual(headers["X-Lease-Epoch"], "4")
        self.assertEqual(headers["X-Lease-Token"], "opaque-token")
        self.assertEqual(headers["Authorization"], f"Bearer {SECRET}")

    def test_rejects_non_loopback_endpoint(self) -> None:
        with self.assertRaises(ValueError):
            ExecutorClient("https://example.com", SECRET, "python-test")

    def test_wraps_transport_failure_in_safe_error_code(self) -> None:
        with mock.patch("urllib.request.urlopen", side_effect=urllib.error.URLError("offline")):
            with self.assertRaises(ExecutorAPIError) as raised:
                self.client.lease()
        self.assertEqual(raised.exception.code, "EXECUTOR_REQUEST_UNAVAILABLE")


if __name__ == "__main__":
    unittest.main()

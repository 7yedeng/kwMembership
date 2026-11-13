from __future__ import annotations

import json
import os
import threading
import unittest
import urllib.error
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from unittest import mock

from python_executor.client import ExecutorAPIError, ExecutorClient


SECRET = "python-executor-test-secret-000000000001"


class Handler(BaseHTTPRequestHandler):
    requests: list[dict[str, object]] = []
    redirect_status: int | None = None
    redirect_target = ""

    def do_POST(self) -> None:
        length = int(self.headers.get("Content-Length", "0"))
        body = json.loads(self.rfile.read(length) or b"{}")
        self.requests.append({"path": self.path, "headers": dict(self.headers), "body": body, "method": "POST"})
        if self.redirect_status is not None:
            self.send_response(self.redirect_status)
            self.send_header("Location", self.redirect_target)
            self.send_header("Content-Length", "0")
            self.end_headers()
            return
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

    def do_GET(self) -> None:
        self.requests.append({"path": self.path, "headers": dict(self.headers), "method": "GET"})
        self.send_response(204)
        self.end_headers()

    def log_message(self, _format: str, *_args: object) -> None:
        return


class RedirectSink(BaseHTTPRequestHandler):
    requests: list[dict[str, object]] = []

    def do_GET(self) -> None:
        self.requests.append({"path": self.path, "headers": dict(self.headers)})
        self.send_response(204)
        self.end_headers()

    do_POST = do_GET

    def log_message(self, _format: str, *_args: object) -> None:
        return


class ClientTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        cls.thread = threading.Thread(target=cls.server.serve_forever, daemon=True)
        cls.thread.start()
        cls.sink = ThreadingHTTPServer(("127.0.0.1", 0), RedirectSink)
        cls.sink_thread = threading.Thread(target=cls.sink.serve_forever, daemon=True)
        cls.sink_thread.start()

    @classmethod
    def tearDownClass(cls) -> None:
        cls.server.shutdown()
        cls.server.server_close()
        cls.sink.shutdown()
        cls.sink.server_close()

    def setUp(self) -> None:
        Handler.requests.clear()
        Handler.redirect_status = None
        Handler.redirect_target = ""
        RedirectSink.requests.clear()
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

    def test_accepts_only_explicit_numeric_loopback_endpoints(self) -> None:
        for endpoint in ("http://127.0.0.1:4312", "http://127.0.0.1:4312/", "http://[::1]:4312"):
            with self.subTest(endpoint=endpoint):
                ExecutorClient(endpoint, SECRET, "python-test")

    def test_rejects_non_loopback_and_ambiguous_endpoints(self) -> None:
        for endpoint in (
            "https://example.com",
            "http://example.com:4312",
            "http://localhost:4312",
            "http://127.0.0.1:4312@evil.example",
            "http://[::1]:4312@evil.example",
            "http://user:password@127.0.0.1:4312",
            "http://127.0.0.1:4312/path",
            "http://127.0.0.1:4312?destination=elsewhere",
            "http://127.0.0.1:4312#fragment",
            "http://127.0.0.1",
            "http://127.0.0.1:0",
            "http://127.0.0.1:65536",
            "http://[::1",
            " http://127.0.0.1:4312",
            "http://127.0.0.1:4312\n",
        ):
            with self.subTest(endpoint=endpoint), self.assertRaises(ValueError):
                ExecutorClient(endpoint, SECRET, "python-test")

    def test_redirects_never_forward_executor_or_lease_credentials(self) -> None:
        lease = self.client.lease()
        self.assertIsNotNone(lease)
        # Both destinations are local test servers: no request can reach the
        # Internet even if this regression test exposes a redirect bug.
        for target in (
            f"http://127.0.0.1:{self.server.server_port}/redirect-target",
            f"http://127.0.0.1:{self.sink.server_port}/redirect-target",
        ):
            for status in (301, 302, 303, 307, 308):
                with self.subTest(target=target, status=status):
                    Handler.requests.clear()
                    RedirectSink.requests.clear()
                    Handler.redirect_status = status
                    Handler.redirect_target = target
                    with self.assertRaises(ExecutorAPIError) as raised:
                        self.client.report(lease, "failed", error_code="FIXTURE_FAILURE")
                    self.assertEqual(raised.exception.code, "EXECUTOR_REDIRECT_REJECTED")
                    self.assertEqual(len(Handler.requests), 1)
                    self.assertTrue(Handler.requests[0]["path"].endswith("/result"))
                    self.assertEqual(RedirectSink.requests, [])

    def test_environment_proxy_does_not_receive_local_credentials(self) -> None:
        proxy = f"http://127.0.0.1:{self.sink.server_port}"
        with mock.patch.dict(os.environ, {
            "http_proxy": proxy, "HTTP_PROXY": proxy, "all_proxy": proxy, "ALL_PROXY": proxy,
            "no_proxy": "", "NO_PROXY": "",
        }):
            client = ExecutorClient(f"http://127.0.0.1:{self.server.server_port}", SECRET, "python-test")
            self.assertIsNotNone(client.lease())
        self.assertEqual(len(Handler.requests), 1)
        self.assertEqual(RedirectSink.requests, [])

    def test_submission_report_does_not_claim_payment_completion(self) -> None:
        lease = self.client.lease()
        self.assertIsNotNone(lease)
        self.client.report_submitted(lease, {"stateId": "PAYMENT_FINAL_READY"})
        body = Handler.requests[-1]["body"]
        # Go accepts success for command completion; only reconciliation can
        # mark the payment/order complete. Keep this distinction on the wire.
        self.assertEqual(body["status"], "success")
        self.assertEqual(body["diagnostic"], {
            "phase": "live", "stateId": "PAYMENT_FINAL_READY", "status": "submitted",
        })

    def test_wraps_transport_failure_in_safe_error_code(self) -> None:
        with mock.patch.object(self.client._opener, "open", side_effect=urllib.error.URLError("offline")):
            with self.assertRaises(ExecutorAPIError) as raised:
                self.client.lease()
        self.assertEqual(raised.exception.code, "EXECUTOR_REQUEST_UNAVAILABLE")


if __name__ == "__main__":
    unittest.main()

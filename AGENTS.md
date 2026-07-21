# Project Rules

- This repository owns the Go implementation of membership intake, eligibility, inventory, funding, payment progression, reconciliation, and processor lifecycle.
- The worker must read and write the configured kwRedeem SQLite database directly. Do not introduce a second membership database, dispatch queue, or projection callback.
- Encrypted fields and account locks use the `JWT_SECRET` loaded from kwRedeem because both processes share the same durable records.
- Never log Session payloads, card material, provider tokens, checkout URLs, or decrypted funding requests.
- The production checkout executor is Go-controlled headless Chrome. Do not reintroduce a browser extension, visible-browser operator step, or Node dispatch dependency into membership fulfillment.
- Every progression/final checkout control activation must cross a durable permit and card-transaction snapshot before the click.
- Payment submission remains rollout-gated. Unknown outcomes must reconcile from evidence and must never be blindly resubmitted.

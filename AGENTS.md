# AGENTS.md

Guidance for AI coding agents working in this repository.

## What this is

`saurally-order-relay` is a small Go webhook relay for an e-commerce store
(saurally.com). It receives commerce events from external platforms, fans
them out to notification/CRM systems, and persists everything to disk for
auditability and crash-safe idempotency. There is no database — the
filesystem under `/data/storage` is the only state.

The entire service is one file: [main.go](main.go). It has zero third-party
dependencies (stdlib only — see [go.mod](go.mod)). Keep it that way unless
there's a strong reason not to; the project's stated philosophy (see the
comments above `main()`) is "no middleware magic, no hidden behavior."

## Request flow

```
WooCommerce ──POST /woocommerce───▶ woocommerceHandler ──▶ Listmonk upsert
                                                         ├─▶ Telegram alert (orders channel)
                                                         └─▶ WhatsApp via Fast2SMS
Medusa ───────POST /medusa-order──▶ medusaOrderHandler ──▶ Listmonk upsert (same list as WooCommerce)
                                                         ├─▶ Telegram alert (same channel as WooCommerce)
                                                         └─▶ WhatsApp via Fast2SMS
GoKwik ABC ───POST /abc───────────▶ abcHandler          ──▶ Listmonk upsert
                                                         └─▶ Telegram alert (ABC channel, only if is_abandoned)
```

Both handlers always persist the raw inbound payload to disk before/after
processing, and both are built to degrade gracefully — a failure in one
integration (e.g. Listmonk down) is logged but does not block the others or
fail the HTTP response.

### Endpoints (routed via `http.ServeMux` in `main()`)

| Path           | Source      | Notes                                                        |
|----------------|-------------|---------------------------------------------------------------|
| `POST /woocommerce` | WooCommerce | Order lifecycle: `processing` → notify, `completed`/`shipped` → fulfilled WhatsApp |
| `POST /medusa-order` | Medusa    | Order lifecycle: `order.placed` → notify, `order.shipped` → fulfilled WhatsApp, `order.canceled` → stored only. Gated by `MEDUSA_ORDER_ENABLED` (returns 503 while off — still being tested as of 2026-08-29) |
| `POST /abc`         | GoKwik      | Abandoned cart capture; always returns 200 to stop GoKwik retries |
| `GET /health`       | internal    | For a load balancer, currently unused in prod                |
| `* /`               | anything else | 404 + logged (blocks bot scans of the root path)          |

### Integrations

- **Fast2SMS (WhatsApp)** — `sendWhatsApp()`. Template-based messages keyed
  by `MESSAGE_ID_ORDER_RECEIVED` / `MESSAGE_ID_ORDER_SHIPPED` /
  `MESSAGE_ID_ORDER_SHIPPED_WITH_TRACKING`. Response bodies are stored under
  `storage/whatsapp/`.
- **Telegram** — `sendTelegram()`. Fire-and-forget (runs in a goroutine),
  HTML-formatted messages, two separate chat IDs for order vs. abandoned-cart
  alerts. No-ops silently if `TELEGRAM_ENABLED` isn't `"true"`.
- **Listmonk** — `listMonkUpsert()`. Search-by-email, then create or update
  (merging `lists` and `attribs` rather than overwriting). `abc_stage` is a
  Listmonk attribute consumed by an external n8n automation (stage 0 =
  abandoned cart entry point, stage 3+ = became a paying customer) — this
  repo doesn't own that logic, just writes the field.
- **Mautic** — referenced in [.env.example](.env.example) but was fully
  removed from the code (see commit `639bc20`, "removed all traces of
  mautic"). The `MAUTIC_*` vars in `.env.example` are stale; don't
  reintroduce Mautic code based on them without checking with the user first.

### Idempotency & storage

Every state transition is gated by a marker file so re-delivered webhooks
(WooCommerce/GoKwik both retry) are safe:

- `isDuplicateEvent(key)` — event-level dedup, `storage/events/<key>`, e.g.
  `order_51281_processing`.
- `flagPath(orderID, state)` / `flagExists()` / `createFlag()` — separate
  WhatsApp-send dedup, `storage/flags/<orderID>_<state>`, so a Listmonk/
  Telegram retry doesn't double-send a WhatsApp message.

Raw payloads land in `storage/gokwik/` and `storage/woocommerce/`; WhatsApp
API responses in `storage/whatsapp/`. Nothing is kept in memory — the process
can be killed and restarted at any point without losing dedup state.

## Configuration

All config is environment variables read once at package-var init time in
`main.go` (see the `var (...)` block at the top) — there's no config
reload. [.env.example](.env.example) documents most of them; cross-check
against `main.go` directly since the example file has drifted (see the
Mautic note above and the Listmonk vars, which exist in code but aren't
listed in `.env.example`: `LISTMONK_ENABLED`, `LISTMONK_URL`,
`LISTMONK_USER`, `LISTMONK_PASS`, `LISTMONK_LIST_ID_ABC`,
`LISTMONK_LIST_ID_ORDERS`). [README.md](README.md) has a more accurate env
var listing.

## Build & run

- `make build` / `make run` / `make test` / `make fmt` / `make vet` — thin
  wrappers around `go build|run|test|fmt|vet`. Note: there are currently no
  `_test.go` files, so `make test` is a no-op.
- **Local Docker Compose is the primary deployment path** and does *not*
  use the multi-stage [Dockerfile](Dockerfile). Instead
  [docker-compose.yml](docker-compose.yml) mounts the host source directory
  into a `golang:1.25-alpine` container and runs `go build` on every
  container start/restart (see the `command:` block). Deploying is
  `git pull && docker compose restart`. The `Dockerfile` (distroless,
  non-root, static binary) exists as an alternative/production-hardened
  build path but isn't what's wired up in compose today.
- A stray `main` binary may show up untracked at the repo root from local
  `go build`/`go run` — it's a build artifact, not part of the repo (it's
  untracked in git and isn't covered by `.gitignore`'s `saurally-order-relay`
  pattern, so don't assume it's meant to be committed).

## Things to know before making changes

- **No auth on `/woocommerce` or `/abc` today.** Both trust whatever hits
  them. If you add signature verification there, it's a deliberate new
  feature, not a bug fix.
- **`/medusa-order` is the one exception — signature verification is
  required and live.** Every request must carry
  `X-Medusa-Signature: sha256=<hex hmac-sha256>`, computed over the raw
  request body with the shared secret `ORDER_RELAY_SECRET`. Verified by
  `verifyMedusaSignature()` in `main.go` using `hmac.Equal` (constant-time)
  against the **raw bytes**, before JSON decoding — decode-then-re-encode
  would silently break the signature on key ordering/spacing. Missing/bad
  signature → `401`; `ORDER_RELAY_SECRET` unset → `500` (fails closed).
  The secret itself must never be committed or written into any doc in this
  repo — it lives only in `.env` (gitignored) on whichever host actually
  runs the service.
- **`abcHandler` always returns HTTP 200** (via `defer`) regardless of
  processing outcome, specifically to stop GoKwik from retrying. Don't
  "fix" this into proper status codes without checking that GoKwik's retry
  behavior is actually the reason.
- **[rough.txt](rough.txt)** (gitignored, present locally) is the integration
  spec that `medusaOrderHandler` (`main.go`) was built from — payload shapes
  for `order.placed` / `order.shipped` / `order.canceled`. Medusa sends no
  auth and never retries (single attempt, fire-and-forget per the spec), so
  the handler doesn't need to defend against redelivery storms the way
  `abcHandler` does for GoKwik.
- **Medusa endpoint is intentionally gated off** via `MEDUSA_ORDER_ENABLED`
  (unset/false by default) while it's being tested against the real store —
  it 503s until flipped on. Don't remove the gate without being asked.
- Medusa orders reuse WooCommerce's downstream config on purpose: same
  `LISTMONK_LIST_ID_ORDERS`, same `TELEGRAM_CHAT_ID_ORDERS`, same
  `MESSAGE_ID_ORDER_*` WhatsApp templates — a single unified "orders" stream
  regardless of which storefront the order came from. The one behavioral
  difference: `pcod` (partial-COD) orders show `cod_balance` (amount still
  due) in the WhatsApp "order received" message instead of `order.total`,
  since the advance was already paid online.
- `order.id` (Medusa's internal id, e.g. `order_01JXYZABCD1234`) is used for
  dedup/flag-file keys; `order.display_id` (the human-facing number) is what
  customers/team actually see in WhatsApp and Telegram messages.
- Logging is a single `*log.Logger` writing to both stdout and
  `LOG_FILE`, using a flat `LEVEL | component | key=value` text convention
  (not structured JSON) — follow that convention if you add log lines.
- The code favors verbose inline error logging over returning wrapped
  errors up the stack; handlers generally log-and-continue rather than
  aborting, so that one bad field in a payload doesn't drop the whole
  webhook. Match that style rather than introducing early-return-on-any-
  error patterns.

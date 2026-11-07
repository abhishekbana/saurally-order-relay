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
Shiprocket/fastrr POST /abc-src───▶ abcSrcHandler        ──▶ Listmonk upsert (ABC list)
                                                         ├─▶ Leads CMS (postLeadToHelpdesk) — CMS notifies Telegram on success
                                                         ├─▶ Telegram fallback (only if leads CMS call failed)
                                                         └─▶ WhatsApp ABC1 (always — unaffected by leads outcome)
```

All handlers always persist the raw inbound payload to disk before/after
processing, and all are built to degrade gracefully — a failure in one
integration (e.g. Listmonk down) is logged but does not block the others or
fail the HTTP response.

### Endpoints (routed via `http.ServeMux` in `main()`)

| Path           | Source      | Notes                                                        |
|----------------|-------------|---------------------------------------------------------------|
| `POST /woocommerce` | WooCommerce | Order lifecycle: `processing` → notify, `completed`/`shipped` → fulfilled WhatsApp |
| `POST /medusa-order` | Medusa    | Order lifecycle: `order.placed` → notify, `order.shipped` → fulfilled WhatsApp, `order.canceled` → stored only, `order.email_corrected` → move Listmonk list + admin Telegram note, `order.gst_updated` → merge GST attribs only. Gated by `MEDUSA_ORDER_ENABLED` (returns 503 while off — still being tested as of 2026-08-29) |
| `POST /abc-src`     | Shiprocket/fastrr | Abandoned cart capture (checkout, not order); always returns 200 |
| `GET /health`       | internal    | For a load balancer, currently unused in prod                |
| `* /`               | anything else | 404 + logged (blocks bot scans of the root path)          |

### Integrations

- **Fast2SMS (WhatsApp)** — `sendWhatsApp()`. Template-based messages keyed
  by `MESSAGE_ID_ORDER_RECEIVED` / `MESSAGE_ID_ORDER_SHIPPED` /
  `MESSAGE_ID_ORDER_SHIPPED_WITH_TRACKING` / `MESSAGE_ID_ABC1` (abandoned-cart
  nudge, sent by `abcSrcHandler`). Response bodies are
  stored under `storage/whatsapp/`.
- **Telegram** — `sendTelegram()`. Fire-and-forget (runs in a goroutine),
  HTML-formatted messages, two separate chat IDs for order vs. abandoned-cart
  alerts. No-ops silently if `TELEGRAM_ENABLED` isn't `"true"`.
- **Listmonk** — `listMonkUpsert()`. Search-by-email, then create or update
  (merging `lists` and `attribs` rather than overwriting). `abc_stage` is a
  Listmonk attribute consumed by an external n8n automation (stage 0 =
  abandoned cart entry point, stage 3+ = became a paying customer) — this
  repo doesn't own that logic, just writes the field.
- **Removed integrations** — Mautic (CRM) and GoKwik (`/abc` abandoned
  carts) are no longer used and have been removed from code, config and
  docs. Don't reintroduce either without checking with the user first.
  Requests to `/abc` now fall through to the `/` catch-all (404).

### Idempotency & storage

Every state transition is gated by a marker file so re-delivered webhooks
(WooCommerce retries) are safe:

- `isDuplicateEvent(key)` — event-level dedup, `storage/events/<key>`, e.g.
  `order_51281_processing`.
- `flagPath(orderID, state)` / `flagExists()` / `createFlag()` — separate
  WhatsApp-send dedup, `storage/flags/<orderID>_<state>`, so a Listmonk/
  Telegram retry doesn't double-send a WhatsApp message.

Raw payloads land in `storage/shiprocket/`,
`storage/woocommerce/` and `storage/medusa/`; WhatsApp API responses in
`storage/whatsapp/`; leads CMS request/response pairs in `storage/leads/`
(keyed by `cart_id`, from `/abc-src` only). Nothing is kept in memory — the
process can be killed and restarted at any point without losing dedup state.

Note: `abcSrcHandler` doesn't call `isDuplicateEvent()`. It has a
**30-minute email-or-phone window** instead
(`abcSrcSeenRecently()` / `markAbcSrcSeen()`, markers in
`storage/abc-src-recent/{email,phone}_<sha256 prefix>`, age = file mtime,
phone normalized to last 10 digits): if either the email or the phone was
processed within `abcSrcRecentWindow`, the leads CMS call, Telegram fallback
and WhatsApp ABC1 are skipped. Listmonk upsert and raw storage still run.
Markers are only written for processed hits (fixed window, not sliding), so
a cart that pings every 20 minutes still gets a lead every 30+ minutes.

## Configuration

All config is environment variables read once at package-var init time in
`main.go` (see the `var (...)` block at the top) — there's no config
reload. [.env.example](.env.example) documents most of them; cross-check
against `main.go` directly since the example file has drifted (see the
Listmonk vars, which exist in code but aren't
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

- **No auth on `/woocommerce` or `/abc-src` today.** All trust
  whatever hits them. If you add signature verification there, it's a
  deliberate new feature, not a bug fix.
- **`/abc-src` (Shiprocket/fastrr) has no `is_abandoned` flag** in its
  payload. Every request is currently treated as an
  abandoned-cart event and always triggers Listmonk + WhatsApp ABC1 — this
  assumes fastrr only calls this endpoint for incomplete checkouts. If that
  assumption turns out to be wrong (e.g. fastrr also fires it for completed
  payments), this will need a gate on `payment_status` or `latest_stage` —
  check with the user before adding one speculatively.
  `abcSrcHandler` uses `msgABC1` for WhatsApp. The Listmonk list is
  conditionally segmented — see `isMobilePlaceholderEmail()` below.
- **`postLeadToHelpdesk()`** (`main.go`) — `abcSrcHandler` posts each cart to
  an external leads CMS (`LEADS_ENDPOINT_URL`, `x-api-key: LEADS_API_KEY`)
  right after the Listmonk upsert. Synchronous, single attempt, no retry.
  **On success (2xx) the relay's own `telegramChatIDABC` alert is skipped
  entirely** — the CMS sends its own Telegram notification when it creates
  a lead, so sending both would double-notify. Any failure (non-2xx,
  network error, timeout, or `LEADS_API_KEY` empty) falls back to the
  relay's own Telegram alert, unchanged from before this existed. The CMS
  doesn't dedupe; the relay suppresses repeat hits for the same email or
  phone within 30 minutes (see the idempotency note above). Repeats after
  the window create a new lead. WhatsApp ABC1 is unaffected by the leads
  call's outcome, but it's skipped together with the lead in the 30-minute
  window.
- **`isMobilePlaceholderEmail()`** (`main.go`) detects fastrr's placeholder
  email for mobile-only checkouts (no real email given): `<mobile>@fastrr.com`,
  matched by suffix, case-insensitive. Only checked in `abcSrcHandler` and
  `medusaOrderHandler` (fastrr is the only source that produces these) —
  when it matches, the Listmonk upsert uses `LISTMONK_LIST_ID_MOBILE_ABC` /
  `LISTMONK_LIST_ID_MOBILE_ORDERS` instead of the normal
  `LISTMONK_LIST_ID_ABC` / `LISTMONK_LIST_ID_ORDERS`. Processing is otherwise
  unaffected — Telegram, WhatsApp, storage, dedup all proceed as usual
  regardless of which list the subscriber lands in. One exception: the
  `/medusa-order` admin Telegram "New Order" message gets an extra line
  ("No email on this order — send tracking link manually via WhatsApp")
  appended when `isMobileOnly` is true — admin-facing only, doesn't touch
  the customer-facing WhatsApp message itself.
- **`order.email_corrected`** (`medusaOrderHandler`) — Medusa's admin-side
  correction flow for an order that was placed with a wrong/placeholder
  email. Same envelope shape as `order.placed`, plus a required top-level
  `old_email` field (validated before the dedup marker is written, like
  `order`/`customer` missing — so a malformed first attempt doesn't
  permanently block a corrected retry via `isDuplicateEvent`). Handling:
  `listMonkRemoveFromList(old_email, ...)` — a new function, factored out
  of `listMonkUpsert` via the shared `listMonkFindSubscriber()` helper —
  drops list membership only (never deletes the subscriber record) from
  `LISTMONK_LIST_ID_MOBILE_ORDERS` if `old_email` matches the fastrr
  placeholder pattern, else `LISTMONK_LIST_ID_ORDERS` (so this also fixes a
  plain typo'd real email, not just the placeholder case, per explicit
  request). Then `listMonkUpsert()`s the corrected `customer.email` onto
  `LISTMONK_LIST_ID_ORDERS` with full attribs (same construction as
  `order.placed`), and posts a quiet Telegram note to `TELEGRAM_CHAT_ID_ORDERS`.
  No WhatsApp — phone didn't change, those messages already went out.
- **`order.gst_updated`** (`medusaOrderHandler`) — customer adds/corrects GST
  details after the order was placed. Same envelope as `order.placed`, with
  `order.gst_number` / `order.gst_company_name` optionally present — **can
  arrive twice for the same order** (company name first, GST number later),
  each delivery potentially carrying only one of the two fields. Only the
  fields actually present in `gstAttribs` get merged into Listmonk (never
  send an empty/missing field as an explicit attrib key — `listMonkUpsert`'s
  merge would overwrite, i.e. erase, a value the other delivery already
  set). No Telegram, no WhatsApp, no list change — Listmonk upsert only.
  **Dedup is NOT the usual `medusa_order_<id>_<event>` key** — it's folded
  into the key via `sha256(gstNumber + "|" + gstCompanyName)` (first 12 hex
  chars), hashed rather than concatenated raw since `gst_company_name` is
  free text and would otherwise end up unsanitized in a filesystem path.
  This means two deliveries with *different* GST fields for the same order
  are correctly treated as distinct events, not a duplicate — if this had
  reused the plain `<id>_<event>` key like every other event, the second
  (more complete) delivery would've been silently dropped by
  `isDuplicateEvent`. Don't "simplify" this back to the plain key.
- **`IGNORED_CUSTOMER_EMAILS`** (comma-separated, case-insensitive, parsed by
  `parseEmailSet()`) — emails to silently skip on `/medusa-order` and
  `/abc-src` only (not `/woocommerce`). Checked
  immediately after the customer email is extracted, before dedup/logging/
  storage — a matching request does nothing but log one INFO line and
  respond `200`. Exists to keep the developer's own test traffic on the
  Shiprocket/fastrr checkout flow (Medusa orders + `/abc-src` abandoned
  carts) out of real Listmonk/Telegram/WhatsApp, since that flow has been
  tested repeatedly against prod with a real personal email.
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
- **[rough.txt](rough.txt)** (gitignored, present locally) is the integration
  spec that `medusaOrderHandler` (`main.go`) was built from — payload shapes
  for `order.placed` / `order.shipped` / `order.canceled`. Medusa sends no
  auth and never retries (single attempt, fire-and-forget per the spec), so
  the handler doesn't need to defend against redelivery storms.
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
- **`order.checkout_source` gates a COD-verification warning, not
  `order.payment_method`.** `payment_method` (`cod`/`prepaid`/`pcod`) only
  describes *how* an order claims to have been paid and is present
  regardless of whether a gateway actually confirmed anything — never treat
  it as proof of payment. `checkout_source == "native_fallback"` means no
  gateway touched the order at all; the `order.placed` admin Telegram
  message appends "🔴 FULL COD ORDER #\<display_id\> — Manual verification
  required before shipping" in that case, regardless of `payment_method`.
  `checkout_source == "shiprocket"` means Shiprocket's own gateway handled
  it, so `payment_method` can be trusted as gateway-confirmed and no note is
  added. Stacks with the mobile-only-email note above (both can apply to
  the same order) via the `adminNotes` slice in `medusaOrderHandler`.
- Logging is a single `*log.Logger` writing to both stdout and
  `LOG_FILE`, using a flat `LEVEL | component | key=value` text convention
  (not structured JSON) — follow that convention if you add log lines.
- The code favors verbose inline error logging over returning wrapped
  errors up the stack; handlers generally log-and-continue rather than
  aborting, so that one bad field in a payload doesn't drop the whole
  webhook. Match that style rather than introducing early-return-on-any-
  error patterns.

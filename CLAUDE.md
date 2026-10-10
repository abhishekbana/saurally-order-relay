# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

```bash
# Build
make build          # go build -o saurally-order-relay
make run            # go run main.go (local dev)

# Quality
make fmt            # go fmt ./...
make vet            # go vet ./...

# Docker (production)
make docker-up      # docker compose up -d  (builds inside container on start)
make docker-down
make docker-logs    # tail container logs
```

There are no tests (`make test` runs `go test ./...` but none exist yet).

**Deploy cycle** — source is mounted into the container; the container builds on every start:
```bash
git pull && docker compose restart
```

Local override: `docker-compose.override.yml` (gitignored) exists for pointing the bind mounts at local paths instead of the server's `/mnt/SSD256/...` — used when running this off the production host. Never edit `docker-compose.yml` itself for local-only path changes.

## Architecture

Everything lives in a single file: `main.go` (~1500 lines, stdlib only, no external deps).

### Request flow

```
POST /woocommerce   →  woocommerceHandler
POST /medusa-order  →  medusaOrderHandler   (HMAC-signed, gated by MEDUSA_ORDER_ENABLED)
POST /abc           →  abcHandler           (GoKwik)
POST /abc-src       →  abcSrcHandler        (Shiprocket/fastrr checkout)
GET  /health        →  200 ok
/*                  →  404 (bot trap)
```

### Idempotency

**`storage/events/<key>`** — `isDuplicateEvent()`. Used by `woocommerceHandler` (`order_<ID>_<normalizedStatus>`) and `medusaOrderHandler` (`medusa_order_<order.id>_<event>`). Marks the whole event (Listmonk + Telegram + WhatsApp) as done.

**`storage/flags/<key>`** — `flagPath` / `flagExists` / `createFlag`. WhatsApp-send dedup, keyed `<orderID>_<state>`. Used inside the WhatsApp block of both `woocommerceHandler` and `medusaOrderHandler` (`medusa_<order.id>` as the orderID prefix there) — belt-and-suspenders on top of `isDuplicateEvent`, not dead code for Medusa.

**`abcHandler` and `abcSrcHandler` do not dedupe at all** — every abandoned-cart webhook is processed independently, every time. This is intentional existing behavior, not an oversight.

### Status normalization (WooCommerce only)

`normalizeStatus()` is the single place that decides what gets processed for `/woocommerce`:
- `"processing"` → `"processing"` (Listmonk + Telegram + WhatsApp order-received)
- `"shipped"` or `"completed"` → `"fulfilled"` (Listmonk + WhatsApp shipped)
- anything else → `""` → early return, nothing runs

Medusa doesn't use this — `medusaOrderHandler` switches directly on the payload's `event` field (`order.placed` / `order.shipped` / `order.canceled`).

### Storage layout (under `DATA_DIR=/data`)

```
/data/
├── storage/
│   ├── events/       idempotency markers (woocommerce, medusa)
│   ├── flags/        whatsapp dedup markers (woocommerce, medusa)
│   ├── gokwik/       raw GoKwik ABC payloads
│   ├── shiprocket/   raw fastrr/Shiprocket checkout ABC payloads
│   ├── woocommerce/  raw order payloads
│   ├── medusa/       raw Medusa order payloads
│   └── whatsapp/     Fast2SMS API responses
└── logs/app.log
```

`storeJSON(category, name, payload)` writes to `storage/<category>/<name>.json`.

### Listmonk upsert pattern

`listMonkUpsert()` always does search-then-create-or-update. On update it **merges** lists and attribs (existing fields are preserved; new fields overwrite). The `email` field is the lookup key.

**Mobile-only list segmentation**: `isMobilePlaceholderEmail()` detects fastrr's placeholder email for no-email checkouts (`<mobile>@fastrr.com`, suffix match, case-insensitive). When it matches, `abcSrcHandler` and `medusaOrderHandler` upsert into `LISTMONK_LIST_ID_MOBILE_ABC` / `LISTMONK_LIST_ID_MOBILE_ORDERS` instead of the normal `LISTMONK_LIST_ID_ABC` / `LISTMONK_LIST_ID_ORDERS`. Nothing else about processing changes. `medusaOrderHandler`'s admin Telegram message also appends a note in this case ("no email — send tracking link manually via WhatsApp").

### WhatsApp (Fast2SMS)

`sendWhatsApp(orderID, phone, templateID, variables, state string)` — variables are pipe-separated (`"name|value1|value2"`). Response is stored to `whatsapp/<orderID>_<state>.json`. Templates: `MESSAGE_ID_ORDER_RECEIVED` / `_SHIPPED` / `_SHIPPED_WITH_TRACKING` (orders, both WooCommerce and Medusa) and `MESSAGE_ID_ABC1` (abandoned-cart nudge, both `abcHandler` and `abcSrcHandler`).

### Telegram

`sendTelegram()` fires in a goroutine (fire-and-forget). Guarded by `telegramEnabled`. Destinations:
- New orders → `TELEGRAM_CHAT_ID_ORDERS` — WooCommerce (`processing` status) and Medusa (`order.placed`, tagged `[medusa-order]` in the title so it's distinguishable in the shared channel)
- Abandoned carts → `TELEGRAM_CHAT_ID_ABC` — GoKwik (only when `is_abandoned=true`) and fastrr/`/abc-src` (always, no such flag exists in that payload)

### GoKwik ABC handler (`/abc`)

- Listmonk upsert runs for **every cart** (including non-abandoned)
- Telegram + WhatsApp ABC1 fire only when `is_abandoned == true`
- Raw cart stored as `gokwik/<email>_<HHMMSS>.json`

### Shiprocket/fastrr ABC handler (`/abc-src`)

- Flat payload (not wrapped in a `carts` array like GoKwik); customer fields at the top level (`email`, `phone_number`, `first_name`, `last_name`), city/state from `billing_address`
- No `is_abandoned` equivalent — every webhook always triggers Listmonk + Telegram + WhatsApp ABC1 (assumes fastrr only calls this for incomplete checkouts)
- Reuses GoKwik's `telegramChatIDABC` and `msgABC1` — not source-segmented on Telegram/WhatsApp, only on the Listmonk list (mobile-only check, see above)
- Cart URL field is `checkout_url` (vs. GoKwik's `abc_url`)
- Raw cart stored as `shiprocket/<email>_<HHMMSS>.json`

### Medusa order handler (`/medusa-order`)

- **Signature verification is required and live**, unlike every other endpoint here. `X-Medusa-Signature: sha256=<hex hmac-sha256>` over the raw request body, keyed with `ORDER_RELAY_SECRET`, verified with `hmac.Equal` (constant-time) *before* JSON decoding. Missing/bad signature → `401`; secret unset → `500` (fails closed).
- Gated by `MEDUSA_ORDER_ENABLED` (off by default) — `503` while disabled.
- `order.id` (Medusa's internal id) is the dedup/flag key; `order.display_id` (human-facing number) is what's shown in Telegram/WhatsApp.
- `order.canceled` is stored only — no Listmonk/Telegram/WhatsApp.
- `pcod` (partial-COD) orders show `cod_balance` (amount still due), not `order.total`, in the WhatsApp "order received" message.
- `order.checkout_source` (not `order.payment_method`) decides whether a gateway confirmed payment. `payment_method` (`cod`/`prepaid`/`pcod`) just describes the claimed method and is present either way. `checkout_source == "native_fallback"` → no gateway touched it → the `order.placed` admin Telegram message appends "🔴 FULL COD ORDER #\<display_id\> — Manual verification required before shipping" regardless of `payment_method`. `checkout_source == "shiprocket"` → Shiprocket's gateway handled it → `payment_method` is trustworthy, no note added. This note and the mobile-only-email note are collected in an `adminNotes` slice and can both appear on the same message.
- Shares WooCommerce's `LISTMONK_LIST_ID_ORDERS`, `TELEGRAM_CHAT_ID_ORDERS`, and WhatsApp template IDs — one unified "orders" stream, except for the mobile-only-email split above.
- **`order.email_corrected`** — admin correction flow when an order was placed with a wrong/placeholder email (order ID unchanged). Same envelope as `order.placed` plus a required top-level `old_email`, validated before dedup so a bad first attempt doesn't block a corrected retry. Removes `old_email` from `LISTMONK_LIST_ID_MOBILE_ORDERS` (if it matches the fastrr placeholder) or `LISTMONK_LIST_ID_ORDERS` (plain typo case) via the new `listMonkRemoveFromList()` — list-membership only, doesn't delete the subscriber — then upserts the corrected email onto `LISTMONK_LIST_ID_ORDERS` with full attribs and posts a quiet Telegram note. No WhatsApp (phone unchanged, already sent). `listMonkUpsert()` and `listMonkRemoveFromList()` share a `listMonkFindSubscriber()` search helper.
- **`order.gst_updated`** — customer adds/corrects GST details post-order. Same envelope as `order.placed`, with `order.gst_number` / `order.gst_company_name` optionally present. Can arrive twice for the same order (name first, number later) — only present fields are merged into Listmonk attribs (an absent field is never sent, so it can't overwrite a value the other delivery already set). Listmonk upsert only, no Telegram/WhatsApp/list change. **Dedup key is content-aware**, not the usual `medusa_order_<id>_<event>` — it's suffixed with `sha256(gst_number + "|" + gst_company_name)[:12]` so a second delivery with different GST fields isn't dropped as a duplicate of the first. Don't revert this to the plain key.

### Test-traffic exclusion

`IGNORED_CUSTOMER_EMAILS` (comma-separated, case-insensitive, `parseEmailSet()`) — on `/medusa-order` and `/abc-src` only. A matching email skips everything (no Listmonk/Telegram/WhatsApp/storage/dedup), just one log line and `200`. Exists to keep test traffic on the fastrr checkout flow out of real channels.

## Key env vars

| Variable | Purpose |
|---|---|
| `DATA_DIR` | Root for all storage (`/data` in container) |
| `LISTMONK_LIST_ID_ABC` / `LISTMONK_LIST_ID_ORDERS` | Integer list IDs |
| `LISTMONK_LIST_ID_MOBILE_ABC` / `LISTMONK_LIST_ID_MOBILE_ORDERS` | Lists for fastrr no-email (`<mobile>@fastrr.com`) checkouts |
| `MESSAGE_ID_ORDER_RECEIVED` / `_SHIPPED` / `_SHIPPED_WITH_TRACKING` / `_ABC1` | Fast2SMS template IDs |
| `TELEGRAM_ENABLED` | Must be `"true"` string to enable |
| `MEDUSA_ORDER_ENABLED` | Must be `"true"` to un-503 `/medusa-order` |
| `ORDER_RELAY_SECRET` | Shared HMAC secret for `/medusa-order` — never commit the real value |
| `IGNORED_CUSTOMER_EMAILS` | Comma-separated test emails to skip on `/medusa-order` and `/abc-src` |

All datetime values sent to Listmonk must be `time.RFC3339` — non-ISO dates cause Listmonk 500 errors.

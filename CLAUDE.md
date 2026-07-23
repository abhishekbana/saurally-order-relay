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

## Architecture

Everything lives in a single file: `main.go` (~1100 lines, stdlib only, no external deps).

### Request flow

```
POST /woocommerce  →  woocommerceHandler
POST /abc          →  abcHandler
GET  /health       →  200 ok
/*                 →  404 (bot trap)
```

### Idempotency — two separate systems

**`storage/events/<key>`** — used in `isDuplicateEvent()`, called at the top of `woocommerceHandler`. Marks the entire event (Listmonk + Telegram + WhatsApp) as done. Key format: `order_<ID>_<normalizedStatus>`.

**`storage/flags/<key>`** — used by `flagPath` / `flagExists` / `createFlag`. Currently only referenced inside the WhatsApp block of `woocommerceHandler`, but since `isDuplicateEvent` already gates the whole handler, this flag system is effectively dead code.

### Status normalization

`normalizeStatus()` is the single place that decides what gets processed:
- `"processing"` → `"processing"` (Listmonk + Telegram + WhatsApp order-received)
- `"shipped"` or `"completed"` → `"fulfilled"` (Listmonk + WhatsApp shipped)
- anything else → `""` → early return, nothing runs

### Storage layout (under `DATA_DIR=/data`)

```
/data/
├── storage/
│   ├── events/     idempotency markers (woocommerce)
│   ├── flags/      whatsapp dedup markers (currently unused)
│   ├── gokwik/     raw ABC payloads
│   ├── woocommerce/ raw order payloads
│   └── whatsapp/   Fast2SMS API responses
└── logs/app.log
```

`storeJSON(category, name, payload)` writes to `storage/<category>/<name>.json`.

### Listmonk upsert pattern

`listMonkUpsert()` always does search-then-create-or-update. On update it **merges** lists and attribs (existing fields are preserved; new fields overwrite). The `email` field is the lookup key.

### WhatsApp (Fast2SMS)

`sendWhatsApp(orderID, phone, templateID, variables, state string)` — variables are pipe-separated (`"name|value1|value2"`). Response is stored to `whatsapp/<orderID>_<state>.json`.

### Telegram

`sendTelegram()` fires in a goroutine (fire-and-forget). It is guarded by `telegramEnabled` and only used for:
- New orders (`processing` status) → `TELEGRAM_CHAT_ID_ORDERS`
- Abandoned carts (`is_abandoned=true`) → `TELEGRAM_CHAT_ID_ABC`

### ABC (GoKwik abandoned cart) handler

- Listmonk upsert runs for **every cart** (including non-abandoned)
- Telegram + WhatsApp ABC1 fire only when `is_abandoned == true`
- Raw cart stored as `gokwik/<email>_<HHMMSS>.json`

## Key env vars

| Variable | Purpose |
|---|---|
| `DATA_DIR` | Root for all storage (`/data` in container) |
| `LISTMONK_LIST_ID_ABC` / `LISTMONK_LIST_ID_ORDERS` | Integer list IDs |
| `MESSAGE_ID_ORDER_RECEIVED` / `_SHIPPED` / `_SHIPPED_WITH_TRACKING` / `_ABC1` | Fast2SMS template IDs |
| `TELEGRAM_ENABLED` | Must be `"true"` string to enable |

All datetime values sent to Listmonk must be `time.RFC3339` — non-ISO dates cause Listmonk 500 errors.

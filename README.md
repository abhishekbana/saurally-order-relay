# Saurally Order Relay

Webhook relay service written in Go. Receives commerce events from WooCommerce and GoKwik, syncs subscribers to Listmonk, sends WhatsApp notifications, and posts Telegram alerts — with duplicate prevention and restart-safe storage.

---

## Endpoints

| Endpoint      | Method | Source     | Purpose                        |
|---------------|--------|------------|--------------------------------|
| /woocommerce  | POST   | WooCommerce | Order lifecycle processing    |
| /medusa-order | POST   | Medusa      | Order lifecycle processing (disabled until `MEDUSA_ORDER_ENABLED=true`) |
| /abc          | POST   | GoKwik      | Abandoned cart ingestion      |
| /abc-src      | POST   | Shiprocket/fastrr | Abandoned cart ingestion (checkout) |
| /health       | GET    | Internal    | Health check                  |
| /(root)       | ANY    | Bots        | Blocked and logged            |

---

## WooCommerce Order Logic

| Status / Condition       | Listmonk Upsert | WhatsApp         | Telegram    | Dedup |
|--------------------------|-----------------|------------------|-------------|-------|
| processing               | ✅              | ✅ Order Received | ✅ New Order | Yes  |
| completed                | ✅              | ❌               | ❌          | Yes   |
| shipped (with tracking)  | ❌              | ✅ Shipped + tracking | ❌      | Yes   |
| shipped (no tracking)    | ❌              | ✅ Shipped        | ❌          | Yes   |
| duplicate webhook        | ❌ Skipped      | ❌ Skipped        | ❌ Skipped  | Yes   |

Tracking ID extracted from `meta_data → _wc_shipment_tracking_items → tracking_number`.

---

## Medusa Order Logic

Disabled by default — while `MEDUSA_ORDER_ENABLED` isn't `"true"`, the endpoint
returns `503` and does nothing else (no storage, no dedup, no downstream calls).

Every request must carry a valid `X-Medusa-Signature: sha256=<hex hmac>`
header — an HMAC-SHA256 over the *exact raw request body bytes*, keyed with
`ORDER_RELAY_SECRET` (shared secret, set on both sides, never committed).
Verified with a constant-time comparison (`hmac.Equal`) before the body is
parsed as JSON. Missing/invalid signature → `401`. If `ORDER_RELAY_SECRET`
isn't configured on this side, every request is rejected with `500` (fails
closed, never open).

Once enabled, mirrors the WooCommerce order flow and shares the same
Listmonk list (`LISTMONK_LIST_ID_ORDERS`), Telegram channel
(`TELEGRAM_CHAT_ID_ORDERS`) and WhatsApp template IDs.

| Event / Condition        | Listmonk Upsert | WhatsApp                 | Telegram    | Dedup |
|---------------------------|-----------------|---------------------------|-------------|-------|
| order.placed               | ✅              | ✅ Order Received         | ✅ New Order | Yes  |
| order.shipped (tracking)   | ✅              | ✅ Shipped + tracking     | ❌          | Yes   |
| order.shipped (no tracking)| ✅              | ✅ Shipped                | ❌          | Yes   |
| order.canceled             | ❌              | ❌                        | ❌          | Yes (stored only) |
| duplicate webhook          | ❌ Skipped      | ❌ Skipped                | ❌ Skipped  | Yes   |

Notes:
- `order.id` (Medusa's internal id) is used for dedup/idempotency keys;
  `order.display_id` (human-facing number) is what's shown in Telegram/WhatsApp messages.
- For `pcod` (partial COD) orders, the WhatsApp "order received" message shows
  the outstanding `cod_balance` instead of the full `order.total`, since the
  advance was already paid online.
- Tracking comes directly from the `tracking` object in the payload — no
  parsing required (unlike WooCommerce's `meta_data` extraction).
- Requests where `customer.email` is in `IGNORED_CUSTOMER_EMAILS` are
  skipped entirely before dedup/storage — used to keep test traffic out of
  real Listmonk/Telegram/WhatsApp.
- fastrr's mobile-only checkout placeholder emails (`<mobile>@fastrr.com`)
  are upserted into `LISTMONK_LIST_ID_MOBILE_ORDERS` instead of
  `LISTMONK_LIST_ID_ORDERS` — everything else about the order is processed
  normally.

---

## GoKwik Abandoned Cart Logic

Only carts with `is_abandoned = true` are processed. Each cart is handled independently.

Per cart:
- Customer data extracted from `cart.customer` (email, phone, firstname required)
- Subscriber upserted into Listmonk (ABC list)
- Telegram alert sent with cart details
- Raw payload stored on disk

---

## Shiprocket/fastrr Abandoned Cart Logic (`/abc-src`)

Same behavior as GoKwik's `/abc`, adapted to the fastrr checkout payload shape:
a single flat cart object (not wrapped in a `carts` array), with customer
fields (`email`, `phone_number`, `first_name`, `last_name`) at the top level
instead of nested under `customer`.

**No `is_abandoned` flag exists in this payload** — every webhook received on
this endpoint is treated as an abandoned-cart event and always triggers the
Listmonk upsert + Telegram alert + WhatsApp ABC1 nudge (this assumes fastrr
only calls this endpoint for incomplete/dropped checkouts; a separate order
webhook is expected to handle completed orders).

Field mapping vs. GoKwik:

| GoKwik (`/abc`)      | fastrr (`/abc-src`)         |
|-----------------------|-------------------------------|
| `cart.customer.email` | `email`                       |
| `cart.customer.phone` | `phone_number`                |
| `cart.customer.firstname`/`lastname` | `first_name`/`last_name` |
| `cart.address.city`/`state` | `billing_address.city`/`state` |
| `cart.abc_url`        | `checkout_url`                |
| `cart.total_price`    | `total_price`                 |
| `cart.drop_stage`     | `latest_stage`                |
| `cart.items[].title`/`quantity` | `items[].title`/`quantity` (same shape) |

Raw payloads stored under `storage/shiprocket/`.

Requests where `email` is in `IGNORED_CUSTOMER_EMAILS` are skipped entirely
(no Listmonk/Telegram/WhatsApp, no storage) — same filter used on
`/medusa-order`, for keeping test traffic out of real channels.

fastrr lets customers check out with no email at all, in which case it
fills in a placeholder of the form `<mobile>@fastrr.com`. Any email matching
that pattern is upserted into `LISTMONK_LIST_ID_MOBILE_ABC` instead of
`LISTMONK_LIST_ID_ABC` — everything else (Telegram, WhatsApp ABC1, storage)
is unaffected.

---

## Listmonk Integration

### ABC Subscriber Fields

`phone`, `cart_url`, `abc_stage`, `drop_stage`, `cart_value`, `cart_items`

### Order Subscriber Fields

`company`, `phone`, `address1`, `address2`, `city`, `pincode`, `state`, `last_order_date`, `last_order_id`, `last_order_products`, `last_order_value`, `source`, `abc_stage`

All datetime fields use ISO 8601 (`time.RFC3339`). Non-ISO dates cause Listmonk 500 errors.

---

## Idempotency Keys

| Event           | Marker File Pattern                        |
|-----------------|--------------------------------------------|
| Order received  | `storage/events/order_<ID>_processing`     |
| Order shipped   | `storage/events/order_<ID>_shipped`        |
| Abandoned cart  | `storage/events/abc_<CART_ID>`             |

If a marker exists, the event is skipped entirely.

---

## Environment Variables

```
# Telegram
TELEGRAM_ENABLED=true
TELEGRAM_BOT_TOKEN=xxxxxxxx
TELEGRAM_CHAT_ID_ABC=-123456789
TELEGRAM_CHAT_ID_ORDERS=-987654321

# Listmonk
LISTMONK_ENABLED=true
LISTMONK_URL=https://listmonk.example.com
LISTMONK_USER=admin
LISTMONK_PASS=secret
LISTMONK_LIST_ID_ABC=1
LISTMONK_LIST_ID_ORDERS=2
LISTMONK_LIST_ID_MOBILE_ABC=11
LISTMONK_LIST_ID_MOBILE_ORDERS=12

# WhatsApp (Fast2SMS)
FAST2SMS_WHATSAPP_URL=https://www.fast2sms.com/dev/whatsapp
MESSAGE_ID_ABC1=xxxxx

# Medusa (order webhook)
MEDUSA_ORDER_ENABLED=false
ORDER_RELAY_SECRET=xxxxxxxx

# Test email exclusion (/medusa-order and /abc-src)
IGNORED_CUSTOMER_EMAILS=

# Timezone
TZ=Asia/Kolkata
```

---

## Storage Layout

```
storage/
├── gokwik/       Raw GoKwik payloads
├── shiprocket/   Raw Shiprocket/fastrr checkout payloads
├── woocommerce/  Raw WooCommerce payloads
├── medusa/       Raw Medusa order payloads
├── whatsapp/     WhatsApp API responses
├── events/       Idempotency markers
├── flags/        Internal flags
├── errors/       Reserved for failures
└── logs/app.log  Application log
```

No in-memory state — restarting is always safe.

---

## Deployment

Source is mounted from host; compiled on every container start:

```sh
git pull
docker compose restart
```

---

## Security

- Root path blocked; unknown paths rejected
- JSON-only payloads
- Graceful shutdown on SIGTERM/SIGINT
- HTTP timeouts configured

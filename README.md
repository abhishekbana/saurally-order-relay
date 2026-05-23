# Saurally Order Relay

Webhook relay service written in Go. Receives commerce events from WooCommerce and GoKwik, syncs subscribers to Listmonk, sends WhatsApp notifications, and posts Telegram alerts — with duplicate prevention and restart-safe storage.

---

## Endpoints

| Endpoint      | Method | Source     | Purpose                        |
|---------------|--------|------------|--------------------------------|
| /woocommerce  | POST   | WooCommerce | Order lifecycle processing    |
| /abc          | POST   | GoKwik      | Abandoned cart ingestion      |
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

## GoKwik Abandoned Cart Logic

Only carts with `is_abandoned = true` are processed. Each cart is handled independently.

Per cart:
- Customer data extracted from `cart.customer` (email, phone, firstname required)
- Subscriber upserted into Listmonk (ABC list)
- Telegram alert sent with cart details
- Raw payload stored on disk

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

# WhatsApp (Fast2SMS)
FAST2SMS_WHATSAPP_URL=https://www.fast2sms.com/dev/whatsapp

# Timezone
TZ=Asia/Kolkata
```

---

## Storage Layout

```
storage/
├── gokwik/       Raw GoKwik payloads
├── woocommerce/  Raw WooCommerce payloads
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

# Saurally Order Relay
saurally-order-relay is a production-grade webhook relay service written in Go. It receives commerce events from WooCommerce and GoKwik and reliably forwards them to Mautic CRM, WhatsApp, and Telegram, while preventing duplicate processing.

The service is designed to be restart-safe, idempotent, and suitable for long-running production use on TrueNAS / Docker.

---

## What this service does

This service listens to webhooks and performs the following actions:

• Processes WooCommerce order lifecycle events  
• Processes GoKwik Abandoned Cart (ABC) events  
• Syncs customer and order/cart data to Mautic  
• Sends WhatsApp notifications using templates  
• Sends internal Telegram alerts  
• Prevents duplicate notifications and CRM updates  
• Stores raw payloads and event markers on disk  

---

## Order State Logic (WooCommerce)

| Order Status / Condition | Mautic Upsert | WhatsApp Message | Telegram Alert | Duplicate Protection |
|--------------------------|---------------|------------------|----------------|----------------------|
| processing               | ✅ Yes        | ✅ Order Received | ✅ New Order   | Yes (event marker)   |
| completed                | ✅ Yes        | ❌ No            | ❌ No          | Yes                  |
| shipped (tracking found) | ❌ No         | ✅ Order Shipped + Tracking ID | ❌ No | Yes |
| shipped (no tracking)    | ❌ No         | ✅ Order Shipped | ❌ No          | Yes                  |
| duplicate webhook        | ❌ Skipped    | ❌ Skipped       | ❌ Skipped     | Yes                  |

Notes:  
• Tracking ID is extracted from `meta_data → _wc_shipment_tracking_items → tracking_number`  
• WhatsApp is sent only once per order state  
• Mautic dates are always sent in ISO 8601 format  

---

## Endpoint Summary

| Endpoint       | Method | Source System | Purpose |
|---------------|--------|---------------|---------|
| /abc          | POST   | GoKwik        | Abandoned cart ingestion |
| /woocommerce  | POST   | WooCommerce   | Order lifecycle processing |
| /health       | GET    | Internal      | Health check |
| /(root)       | ANY    | External bots | Blocked and logged |

---

## GoKwik Abandoned Cart Logic (/abc)

| Condition              | Action |
|------------------------|--------|
| is_abandoned = true    | Process cart |
| is_abandoned = false   | Ignore |
| Missing email / phone / firstname | Logged (still processed) |
| Multiple carts in payload | Each cart processed independently |

For each abandoned cart:

• Customer details are extracted strictly from `cart.customer`
• Email, phone, and firstname are mandatory
• Cart value, drop stage, and cart URL are captured
• Items (title + quantity) are extracted
• Data is upserted into Mautic
• A Telegram alert is sent with cart details
• Raw cart payload is stored on disk

Telegram alerts include:

• Customer name  
• Email  
• Phone  
• Cart value  
• Drop stage  
• Items with quantity  
• Cart URL  

All Telegram messages use Markdown/HTML formatting and are non-blocking.

---

## Idempotency Event Keys

| Event Type | Marker File Pattern |
|------------|---------------------|
| Order received | storage/events/order_<ORDER_ID>_processing |
| Order shipped  | storage/events/order_<ORDER_ID>_shipped |
| Abandoned cart | storage/events/abc_<CART_ID> |

If a marker exists, the event is skipped entirely.

---

## WooCommerce Order Handling (/woocommerce)

WooCommerce order webhooks are processed based on order status.

Supported statuses: **processing, completed shipped (via metadata)**

For each order event:

• Order ID, customer, billing, and items are extracted
• Duplicate events are detected and skipped
• Order data is upserted into Mautic
• WhatsApp notification is sent exactly once per state
• Telegram alert is sent for new orders
• Shipment tracking ID is extracted if present

Shipment tracking is extracted from:

```python
meta_data → _wc_shipment_tracking_items → tracking_number
```
When available, the tracking number is appended to the WhatsApp message.

---

## Mautic Integration

Mautic is updated via REST API upserts.

### Abandoned Cart Fields

• email  
• firstname  
• lastname  
• phone / mobile  
• cart_url  
• cart_value  
• drop_stage  
• last_abandoned_cart_date (ISO 8601)  
• tags  

### Order Fields

• email  
• firstname  
• lastname  
• phone  
• address (safely truncated)  
• last_order_id  
• last_order_value  
• last_order_date (ISO 8601)  
• product names  

IMPORTANT  
All datetime fields sent to Mautic MUST be ISO 8601.

The service uses:

```python
time.Now().UTC().Format(time.RFC3339)
```

Any non-ISO date (for example DD/MM/YYYY) will cause Mautic 500 errors.

---

## Telegram Notifications

Telegram is used for internal operational alerts.

Events that trigger Telegram messages:

• Abandoned carts (GoKwik)  
• New orders (WooCommerce processing state)  

Telegram configuration is controlled by environment variables:

```json
TELEGRAM_ENABLED=true  
TELEGRAM_BOT_TOKEN=xxxxxxxx  
TELEGRAM_CHAT_ID=-123456789  
```

Messages are sent asynchronously and never block webhook handling.

---

## WhatsApp Messaging

WhatsApp notifications are sent using predefined template IDs.

Supported scenarios:

• Order received  
• Order shipped  
• Order shipped with tracking ID  

Tracking ID is included only when available.

WhatsApp messages are protected by idempotency flags to avoid duplicates.

---

## Persistent Storage Layout

All persistent data is stored under the mounted storage directory:

```python
storage/
├── gokwik/        Raw GoKwik cart payloads  
├── woocommerce/  Raw WooCommerce order payloads  
├── whatsapp/     WhatsApp API responses  
├── events/       Idempotency marker files  
├── flags/        Internal flags  
└── errors/       Reserved for failures  
```

There is no in-memory state.  
Restarting the container is always safe.

---

## Logging

Logs are written to stdout and to file.

Log file location:

```python
storage/logs/app.log
```

Logs include:

• Raw payloads  
• Processing decisions  
• Duplicate skips  
• External API errors  
• Telegram and WhatsApp send attempts  

Timezone is controlled using:

```python
TZ=Asia/Kolkata
```

---

## Docker Runtime Model

The service runs using the official Go image.

Source code is mounted from the host.

On every container start:

• Latest main.go is compiled
• The binary is executed
• No Docker image rebuild is required

Deployment workflow:

```python
git pull  
docker compose restart  
```
This guarantees the container always runs the latest code.

---

## Health Check

```python
GET /health
```

Response:
```python
ok
```

Used by Docker and reverse proxies.

---

## Security Hardening

• Root path blocked
• Unknown paths logged and rejected
• JSON-only payload acceptance
• Graceful shutdown on SIGTERM / SIGINT
• HTTP timeouts configured

---

## Summary

saurally-order-relay is a hardened webhook relay that:

• Eliminates duplicate customer notifications  
• Keeps Mautic CRM data consistent  
• Provides real-time Telegram visibility  
• Survives restarts and webhook retries safely  

This README reflects the current live production behavior.

# saurally-order-relay

A production-grade webhook relay service for processing commerce events and dispatching downstream actions such as:
- CRM synchronization (Mautic)
- WhatsApp notifications (Fast2SMS)
- Persistent audit logging
- Idempotent message delivery

This service is designed to be **stateless at runtime** and **stateful via filesystem persistence**, making it reliable across restarts and crashes.

---

## Key Features

- Receives WooCommerce and GoKwik webhooks
- Normalizes order lifecycle states
- Guarantees **exactly-once WhatsApp delivery**
- Prevents duplicate notifications
- Persists all payloads for audit/debugging
- Graceful shutdown (Docker-safe)
- Minimal dependencies (Go stdlib)

---

## Order Status Logic

| WooCommerce Status | Internal State | WhatsApp Sent |
|------------------|---------------|---------------|
| processing       | processing    | Yes (once)   |
| completed        | fulfilled     | Yes (once)   |
| shipped          | fulfilled     | Yes (once)   |
| duplicate events | —             | No           |

`completed` and `shipped` are treated as the **same fulfillment state**.

---

## Endpoints

| Endpoint        | Method | Purpose |
|---------------|--------|--------|
| `/`           | POST   | GoKwik webhook |
| `/woocommerce`| POST   | WooCommerce webhook |
| `/health`     | GET    | Health check (Docker / monitoring) |

---

## Directory Layout

```text
saurally-order-relay/
├── main.go
├── Dockerfile
├── docker-compose.yml
├── Makefile
├── .env.example
└── data/
    ├── logs/
    └── storage/
        ├── gokwik/
        ├── woocommerce/
        ├── errors/
        └── flags/
```

## Environment Variables

See .env.example

Required:
 - MAUTIC_URL
 - MAUTIC_USER
 - MAUTIC_PASS
 - FAST2SMS_API_KEY


## Build & Run (Local)
```
make build
make run
```

## Docker Production

```
make docker-build
make docker-up
```

## Health Check

```
curl http://localhost:8080/health
```
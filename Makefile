
---

# 🛠️ `Makefile`

```makefile
# ----------------------------------------------------
# saurally-order-relay Makefile
# ----------------------------------------------------

APP_NAME := saurally-order-relay
IMAGE := saurally/$(APP_NAME)
VERSION := 1.0.0

# ----------------------------------------------------
# Go
# ----------------------------------------------------

.PHONY: build run test clean

build:
	go build -o $(APP_NAME)

run:
	go run main.go

test:
	go test ./...

clean:
	rm -f $(APP_NAME)

# ----------------------------------------------------
# Docker
# ----------------------------------------------------

.PHONY: docker-build docker-run docker-up docker-down docker-logs

docker-build:
	docker build -t $(IMAGE):$(VERSION) .

docker-run:
	docker run --rm -p 8080:8080 \
		--env-file .env \
		-v $(PWD)/data:/data \
		$(IMAGE):$(VERSION)

docker-up:
	docker compose up -d

docker-down:
	docker compose down

docker-logs:
	docker logs -f $(APP_NAME)

# ----------------------------------------------------
# Utility
# ----------------------------------------------------

.PHONY: fmt vet

fmt:
	go fmt ./...

vet:
	go vet ./...

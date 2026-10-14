-include .env
OCIS_DEV_PORT ?= 9200

.PHONY: backend-test backend-vet frontend-build frontend-test license-check dev-up dev-down spike

# needs the REUSE tool: pipx install reuse   (or: uvx reuse lint)
license-check:
	reuse lint

backend-test:
	go test -C backend ./...

backend-vet:
	go vet -C backend ./...
	test -z "$$(gofmt -l backend)"

frontend-build:
	cd frontend && pnpm install --frozen-lockfile && pnpm build

frontend-test:
	cd frontend && pnpm install --frozen-lockfile && pnpm test:unit

.env:
	cp .env.example .env

dev/certs/localhost.crt:
	sh dev/gen-certs.sh

# one command from the repo root: extension build, .env, certs, then the stack
dev-up: .env dev/certs/localhost.crt frontend-build
	docker compose up -d

dev-down:
	docker compose down

spike:
	go run -C backend ./cmd/spike -base https://localhost:$(OCIS_DEV_PORT) $(ARGS)

-include .env
OCIS_DEV_PORT ?= 9200

.PHONY: backend-test backend-vet frontend-build frontend-test license-check dev-up dev-down spike sidecar

# needs the REUSE tool: uv tool install "reuse[charset-normalizer]"
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
	cd frontend && pnpm install --frozen-lockfile && pnpm test:unit && pnpm check:types

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

# throwaway dev master key per run; the sidecar listens on :9300 (nginx routes /agent-api/ to it)
sidecar:
	AGENT_OCIS_URL=https://localhost:$(OCIS_DEV_PORT) AGENT_INSECURE=true AGENT_MASTER_KEY=$$(openssl rand -base64 32) go run -C backend ./cmd/ocis-agent

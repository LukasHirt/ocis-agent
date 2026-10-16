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

# the sidecar listens on :9300 (nginx routes /agent-api/ to it). Local state and a stable dev master key
# live in backend/bin/data (git-ignored); delete that directory to reset. Never use this key elsewhere.
# AGENT_ROLES defaults to the api role only here: the default (api,worker) needs a model provider, which the
# worker role refuses to start without. To run the workers and the scheduler too, export AGENT_ROLES=api,worker
# and AGENT_LLM_BASE_URL, AGENT_LLM_MODEL and AGENT_LLM_API_KEY (or AGENT_LLM_API_KEY_FILE; optional
# AGENT_LLM_MODELS, AGENT_LLM_ALLOWED_HOSTS); they pass through from your shell. The kill switch for a running
# sidecar: `go run -C backend ./cmd/ocis-agent killswitch on|off|status` with the same AGENT_DATA_DIR, plus a
# well-formed AGENT_MASTER_KEY and AGENT_OCIS_URL (the configuration loads first); it applies pending migrations.
sidecar:
	@mkdir -p $(CURDIR)/backend/bin/data
	@test -f $(CURDIR)/backend/bin/data/dev-master.key || (umask 077 && openssl rand -base64 32 > $(CURDIR)/backend/bin/data/dev-master.key)
	AGENT_ROLES=$${AGENT_ROLES:-api} AGENT_DATA_DIR=$(CURDIR)/backend/bin/data AGENT_OCIS_URL=https://localhost:$(OCIS_DEV_PORT) AGENT_INSECURE=true AGENT_MASTER_KEY=$$(cat $(CURDIR)/backend/bin/data/dev-master.key) go run -C backend ./cmd/ocis-agent

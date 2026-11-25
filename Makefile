.DEFAULT_GOAL := help
.PHONY: help install dev dev-core dev-gateway dev-core-go lint lint-core lint-gateway lint-core-go \
	lint-kit golden format format-core format-gateway format-core-go typecheck test test-core test-gateway \
	test-core-go test-kit build build-core build-gateway build-core-go check clean \
	up down db logs ps pair pair-dev media send data-dirs migrate rollback migration test-db contracts

GOLANGCI_LINT_VERSION := v2.15.0
GOLANGCI_LINT := go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMPOSE := VERSION=$(VERSION) docker compose
DATA_DIR ?= $(or $(shell sed -n 's/^DATA_DIR=//p' .env 2>/dev/null),./data)

help: ## list targets
	@grep -E '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*## "}; {printf "  %-15s %s\n", $$1, $$2}'

install: ## install dependencies for every service
	npm ci
	cd gateway && go mod download
	cd core-go && go mod download

dev: dev-core ## run core from source

dev-core: ## run core from source
	npm run dev

dev-gateway: ## run the gateway from source
	cd gateway && go run ./cmd/gateway

dev-core-go: ## run the Go core from source
	cd core-go && go run ./cmd/core

lint: lint-core lint-gateway lint-core-go lint-kit ## lint and format-check everything

lint-core: ## lint and format-check core
	npm run lint

lint-gateway: ## lint and format-check the gateway
	cd gateway && $(GOLANGCI_LINT) run ./...

lint-core-go: ## lint and format-check the Go core
	cd core-go && $(GOLANGCI_LINT) run ./...

lint-kit: ## lint and format-check the shared Go modules
	cd kit && $(GOLANGCI_LINT) run ./...
	cd contracts && $(GOLANGCI_LINT) run ./...

format: format-core format-gateway format-core-go ## fix lint and formatting where possible

format-core: ## fix core formatting
	npm run format

format-gateway: ## fix gateway formatting
	cd gateway && $(GOLANGCI_LINT) fmt ./...

format-core-go: ## fix Go core and shared module formatting
	cd core-go && $(GOLANGCI_LINT) fmt ./...
	cd kit && $(GOLANGCI_LINT) fmt ./...
	cd contracts && $(GOLANGCI_LINT) fmt ./...

typecheck: ## type-check core (the gateway is checked by its build and lint)
	npm run typecheck

test: test-core test-gateway test-core-go test-kit ## run every test suite

test-core: ## run core tests
	npm test

test-gateway: ## run gateway tests with the race detector
	cd gateway && go test -race -count=1 ./...

test-core-go: ## run Go core tests with the race detector
	cd core-go && go test -race -count=1 ./...

test-kit: ## run the shared Go modules' tests with the race detector
	cd kit && go test -race -count=1 ./...
	cd contracts && go test -race -count=1 ./...

build: build-core build-gateway build-core-go ## build every service

build-core: ## build core into core/dist
	npm run build

build-gateway: ## build the gateway into gateway/bin
	cd gateway && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" \
		-o bin/gateway ./cmd/gateway

build-core-go: ## build the Go core into core-go/bin
	cd core-go && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" \
		-o bin/core ./cmd/core

golden: ## write the Go core's golden test files from the TypeScript core
	npx tsx core/scripts/golden.ts

contracts: ## regenerate core's TypeScript types after changing contracts/schemas
	npm run contracts:types -w @kaya/core
	npm run format

check: lint typecheck test build ## everything CI runs

clean: ## remove build output
	rm -rf core/dist gateway/bin core-go/bin

data-dirs:
	@mkdir -p "$(DATA_DIR)/postgres" "$(DATA_DIR)/core" "$(DATA_DIR)/gateway"

up: data-dirs ## build and start the whole stack in Docker, wait until every service is healthy
	$(COMPOSE) up --build --detach --wait --wait-timeout 180

down: ## stop the stack (data stays in DATA_DIR)
	$(COMPOSE) down

db: data-dirs ## start only Postgres and apply migrations, for running services from source
	$(COMPOSE) up --detach --wait db
	$(COMPOSE) run --rm db-roles

logs: ## follow logs from every service
	$(COMPOSE) logs --follow

ps: ## show the status of every service
	$(COMPOSE) ps

PAIR_ARGS = $(if $(PHONE),--phone '$(PHONE)')

pair: data-dirs ## link Kaya to WhatsApp: shows a QR code, or a pairing code with PHONE=<international number>
	@$(COMPOSE) build gateway
	@running=$$($(COMPOSE) ps --status running --quiet gateway); \
	if [ -n "$$running" ]; then $(COMPOSE) stop gateway; fi; \
	$(COMPOSE) run --rm gateway pair $(PAIR_ARGS); status=$$?; \
	if [ -n "$$running" ]; then $(COMPOSE) up --detach --wait gateway; \
	else echo "The gateway was not running; start it with make up or make dev-gateway."; fi; \
	exit $$status

pair-dev: ## pair from source against the db from make db (PHONE= as for pair)
	cd gateway && go run ./cmd/gateway pair $(PAIR_ARGS)

media: ## download the file of one message: make media id=<messages.id> (the gateway must be running and connected)
	@test -n "$(id)" || { echo "usage: make media id=<messages.id>"; exit 1; }
	$(COMPOSE) exec gateway /gateway media fetch '$(id)'

send: export KAYA_TEXT := $(text)
send: ## send a message to your own chat through Kaya: make send text="hello" (the stack must be running and paired)
	@test -n "$(text)" || { echo 'usage: make send text="hello"'; exit 1; }
	$(COMPOSE) exec -T -e KAYA_TEXT core /nodejs/bin/node dist/send.js

migrate: ## apply pending migrations
	$(COMPOSE) run --rm migrate

rollback: ## roll back the latest migration on the live db (asks first)
	@printf 'Roll back the latest migration on the live database? It can drop stored chats. [y/N] '; \
		read answer; [ "$$answer" = y ] || { echo "not rolled back"; exit 1; }
	$(COMPOSE) run --rm migrate --wait rollback

migration: ## create an empty migration: make migration name=add_tasks
	@test -n "$(name)" || { echo "usage: make migration name=add_tasks"; exit 1; }
	@f=db/migrations/$$(date -u +%Y%m%d%H%M%S)_$(name).sql; \
		printf -- '-- migrate:up\n\n-- migrate:down\n' > $$f && echo "created $$f"

TEST_DB := kaya_schema_test
PSQL := $(COMPOSE) exec -T db sh -c
MIGRATE_TEST := $(COMPOSE) run --rm --no-deps migrate --env TEST_DATABASE_URL --wait

test-db: ## on a throwaway db: migrate, rerun (must be a no-op), roll back, reapply, schema tests
	@set -e; \
	$(PSQL) 'dropdb --if-exists -U "$$POSTGRES_USER" $(TEST_DB) && createdb -U "$$POSTGRES_USER" $(TEST_DB)'; \
	trap '$(PSQL) '"'"'dropdb --if-exists -U "$$POSTGRES_USER" $(TEST_DB)'"'"'' EXIT; \
	$(MIGRATE_TEST) up; \
	rerun=$$($(MIGRATE_TEST) up); \
	if echo "$$rerun" | grep -q Applying; then echo "rerunning migrations applied something:"; echo "$$rerun"; exit 1; fi; \
	$(MIGRATE_TEST) rollback; \
	$(MIGRATE_TEST) up; \
	$(PSQL) 'psql -q -U "$$POSTGRES_USER" -d $(TEST_DB)' < db/tests/schema.sql

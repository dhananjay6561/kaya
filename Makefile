.DEFAULT_GOAL := help
.PHONY: help install dev dev-core dev-gateway lint lint-core lint-gateway format format-core \
	format-gateway typecheck test test-core test-gateway build build-core build-gateway check clean \
	up down db logs ps migrate rollback migration test-db

GOLANGCI_LINT_VERSION := v2.15.0
GOLANGCI_LINT := go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMPOSE := VERSION=$(VERSION) docker compose

help: ## list targets
	@grep -E '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*## "}; {printf "  %-15s %s\n", $$1, $$2}'

install: ## install dependencies for every service
	npm ci
	cd gateway && go mod download

dev: dev-core ## run core from source

dev-core: ## run core from source
	npm run dev

dev-gateway: ## run the gateway from source
	cd gateway && go run ./cmd/gateway

lint: lint-core lint-gateway ## lint and format-check everything

lint-core: ## lint and format-check core
	npm run lint

lint-gateway: ## lint and format-check the gateway
	cd gateway && $(GOLANGCI_LINT) run ./...

format: format-core format-gateway ## fix lint and formatting where possible

format-core: ## fix core formatting
	npm run format

format-gateway: ## fix gateway formatting
	cd gateway && $(GOLANGCI_LINT) fmt ./...

typecheck: ## type-check core (the gateway is checked by its build and lint)
	npm run typecheck

test: test-core test-gateway ## run every test suite

test-core: ## run core tests
	npm test

test-gateway: ## run gateway tests with the race detector
	cd gateway && go test -race -count=1 ./...

build: build-core build-gateway ## build every service

build-core: ## build core into core/dist
	npm run build

build-gateway: ## build the gateway into gateway/bin
	cd gateway && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" \
		-o bin/gateway ./cmd/gateway

check: lint typecheck test build ## everything CI runs

clean: ## remove build output
	rm -rf core/dist gateway/bin

up: ## build and start the whole stack in Docker, wait until every service is healthy
	$(COMPOSE) up --build --detach --wait --wait-timeout 180

down: ## stop the stack (data stays in DATA_DIR)
	$(COMPOSE) down

db: ## start only Postgres and apply migrations, for running services from source
	$(COMPOSE) up --detach --wait db
	$(COMPOSE) run --rm migrate

logs: ## follow logs from every service
	$(COMPOSE) logs --follow

ps: ## show the status of every service
	$(COMPOSE) ps

migrate: ## apply pending migrations
	$(COMPOSE) run --rm migrate

rollback: ## roll back the latest migration
	$(COMPOSE) run --rm migrate --wait rollback

migration: ## create an empty migration: make migration name=add_tasks
	@test -n "$(name)" || { echo "usage: make migration name=add_tasks"; exit 1; }
	@f=db/migrations/$$(date -u +%Y%m%d%H%M%S)_$(name).sql; \
		printf -- '-- migrate:up\n\n-- migrate:down\n' > $$f && echo "created $$f"

test-db: ## run the schema tests against the running db
	$(COMPOSE) exec -T db sh -c 'psql -q -U "$$POSTGRES_USER" -d "$$POSTGRES_DB"' < db/tests/schema.sql

.DEFAULT_GOAL := help
.PHONY: help install dev lint format typecheck test build check clean

help: ## list targets
	@grep -E '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*## "}; {printf "  %-10s %s\n", $$1, $$2}'

install: ## install dependencies for every service
	npm ci

dev: ## run core from source
	npm run dev

lint: ## lint and format-check everything
	npm run lint

format: ## fix lint and formatting where possible
	npm run format

typecheck: ## type-check every service
	npm run typecheck

test: ## run every test suite
	npm test

build: ## build every service
	npm run build

check: lint typecheck test build ## everything CI runs

clean: ## remove build output
	rm -rf core/dist

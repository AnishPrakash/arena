SHELL := /bin/bash
GH_USER ?= AnishPrakash
GIT_SHA := $(shell git rev-parse --short HEAD 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(GIT_SHA)
GOFLAGS := -trimpath

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | \
	  awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2}'

.PHONY: sync-langs
sync-langs: ## Copy languages/*.yaml into the embedded manifest dir
	@mkdir -p internal/langs/builtin
	@cp languages/*.yaml internal/langs/builtin/ 2>/dev/null || true

.PHONY: build
build: sync-langs bin/api bin/runner bin/boxrun bin/seed ## Build all binaries

bin/%: FORCE
	@mkdir -p bin
	CGO_ENABLED=0 go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $@ ./cmd/$*
FORCE:

.PHONY: boxrun-static
boxrun-static: ## Build the fully static in-sandbox supervisor
	@mkdir -p bin
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build $(GOFLAGS) \
	  -ldflags "-s -w -extldflags '-static'" -o bin/boxrun ./cmd/boxrun

.PHONY: test
test: ## Run unit tests with the race detector
	go test ./... -race -count=1

.PHONY: test-short
test-short: ## Pure-domain tests only (no docker, no db)
	go test ./internal/core/... ./internal/checker/... ./internal/langs/... -count=1

.PHONY: lint
lint: ## vet + staticcheck + architecture rules
	go vet ./...
	staticcheck ./...
	@./scripts/check-arch.sh

.PHONY: fmt
fmt: ## Format
	gofmt -s -w .

.PHONY: up
up: ## Start the full stack
	docker compose -f deploy/docker-compose.yml up -d --build

.PHONY: down
down: ## Stop and remove the stack
	docker compose -f deploy/docker-compose.yml down -v

.PHONY: logs
logs: ## Tail stack logs
	docker compose -f deploy/docker-compose.yml logs -f --tail=100

.PHONY: migrate
migrate: bin/api ## Apply DB migrations
	./bin/api migrate

.PHONY: seed
seed: bin/seed ## Seed the demo contest
	./bin/seed

.PHONY: images
images: ## Build language sandbox images
	docker build -t arena/cpp20:local     -f images/cpp20/Dockerfile     .
	docker build -t arena/python312:local -f images/python312/Dockerfile .

.PHONY: golden
golden: ## Run the judge-the-judge suite
	go test ./testdata/golden/... -count=1 -v -tags=golden

.PHONY: smoke
smoke: ## End-to-end verdict matrix
	@./scripts/smoke.sh

.PHONY: load
load: ## k6 burst test
	k6 run scripts/load.js

.PHONY: clean
clean:
	rm -rf bin dist

.PHONY: stack
stack: images up ## Build language images, bring the stack up, and smoke test
	@echo "waiting for services..."
	@sleep 15
	@$(MAKE) smoke

.DEFAULT_GOAL := help

GO         ?= go
COMPOSE    ?= docker compose -f deploy/docker-compose.yml
REDIS_NAME ?= rlgw-redis

.PHONY: help
help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: ## Compile the gateway binary
	$(GO) build -trimpath -o bin/gateway ./cmd/gateway

.PHONY: fmt
fmt: ## Format the source
	$(GO) fmt ./...

.PHONY: vet
vet: ## Run go vet
	$(GO) vet ./...

.PHONY: redis
redis: ## Start a local Redis for the integration tests
	@docker rm -f $(REDIS_NAME) >/dev/null 2>&1 || true
	docker run -d --name $(REDIS_NAME) -p 6379:6379 redis:7-alpine >/dev/null
	@echo "redis listening on localhost:6379"

.PHONY: redis-stop
redis-stop: ## Stop the local Redis
	@docker rm -f $(REDIS_NAME) >/dev/null 2>&1 || true

.PHONY: test
test: ## Run the test suite with the race detector
	$(GO) test -race -count=1 ./...

.PHONY: cover
cover: ## Run tests and report coverage
	$(GO) test -race -count=1 -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out | tail -1

.PHONY: bench
bench: ## Benchmark the limiter algorithms against a live Redis
	$(GO) test -bench=. -benchtime=5000x -run='^$$' ./internal/limiter/

.PHONY: up
up: ## Start the full stack (gateway, redis, upstream, prometheus)
	$(COMPOSE) up --build -d
	@echo "gateway    http://localhost:8080"
	@echo "dashboard  http://localhost:9090"
	@echo "metrics    http://localhost:9090/metrics"
	@echo "prometheus http://localhost:9091"

.PHONY: down
down: ## Tear down the stack
	$(COMPOSE) down -v

.PHONY: logs
logs: ## Follow gateway logs
	$(COMPOSE) logs -f gateway

.PHONY: smoke
smoke: ## Fire enough traffic to trip the anonymous quota
	@bash scripts/smoke.sh

.PHONY: loadtest
loadtest: ## Run the k6 load test against the running gateway
	@bash scripts/loadtest.sh

.PHONY: clean
clean: ## Remove build artefacts
	rm -rf bin coverage.out results

COMPOSE_TEST = docker compose -f test/integration/docker-compose.yml

.PHONY: build test test-integration lint docker up down logs

build: ## Build the binary into ./db-proxy-ro
	CGO_ENABLED=0 go build -trimpath -o db-proxy-ro ./cmd/db-proxy-ro

test: ## Unit tests
	go test -race ./...

test-integration: ## Tests against real PostgreSQL, MySQL and MariaDB in Docker
	$(COMPOSE_TEST) up -d --wait
	go test -tags integration -count=1 ./test/integration/; rc=$$?; $(COMPOSE_TEST) down -v; exit $$rc

lint: ## Formatting and static checks
	@test -z "$$(gofmt -l .)" || { gofmt -l .; echo "run gofmt on the files above"; exit 1; }
	go vet -tags integration ./...

docker: ## Build the Docker image
	docker compose build

up: ## Start the proxy
	docker compose up -d --build

down: ## Stop the proxy
	docker compose down

logs: ## Follow both logs
	tail -F logs/error.log logs/query.log

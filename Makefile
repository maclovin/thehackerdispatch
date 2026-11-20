VERSION           ?= $(or $(shell git describe --tags --always --dirty 2>/dev/null | sed 's/^v//'),dev)
COMMIT            ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
IMAGE             ?= ghcr.io/maclovin/thehackerdispatch
CONTAINER_TOOL    ?= docker
TEST_DATABASE_URL ?= postgres://thehacker:dispatch@localhost:5432/thehackerdispatch?sslmode=disable

LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)

.PHONY: help build test lint db run up down image push k8s-render

help: ## list targets
	@grep -E '^[a-z0-9-]+:.*## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*## "}; {printf "  %-12s %s\n", $$1, $$2}'

build: ## build bin/thehackerdispatch
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/thehackerdispatch ./cmd/thehackerdispatch

test: ## run tests (DB tests need `make db`)
	TEST_DATABASE_URL="$(TEST_DATABASE_URL)" go test ./...

lint: ## gofmt, go vet, golangci-lint
	@test -z "$$(gofmt -l .)" || { gofmt -l .; exit 1; }
	go vet ./...
	@if command -v golangci-lint >/dev/null; then golangci-lint run; else echo "golangci-lint not installed, skipped"; fi

db: ## start PostgreSQL only
	$(CONTAINER_TOOL) compose up -d --wait db

run: ## run the app with Go
	DATABASE_URL='postgres://thehacker:dispatch@localhost:5432/thehackerdispatch?sslmode=disable' \
	BASE_URL=http://localhost:8080 \
	SECRETS_KEY=ZGV2LW9ubHkta2V5LWRvLW5vdC11c2UtaW4tcHJvZCE= \
	LOG_LEVEL=debug \
	go run ./cmd/thehackerdispatch

up: ## start PostgreSQL and the app
	$(CONTAINER_TOOL) compose up --build

down: ## remove the local stack
	$(CONTAINER_TOOL) compose down

image: ## build the image
	$(CONTAINER_TOOL) build --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) -t $(IMAGE):$(VERSION) .

push: image ## push the image
	$(CONTAINER_TOOL) push $(IMAGE):$(VERSION)

k8s-render: ## render the example overlay
	kubectl kustomize deploy/overlays/example

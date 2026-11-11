# Differently build and verification targets. `make verify` runs what CI runs.

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.1.0-dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
LDFLAGS := -s -w -X github.com/different-ai/differently/internal/version.Version=$(VERSION) -X github.com/different-ai/differently/internal/version.Commit=$(COMMIT)
BINS    := differently-server differently
# Go build tags for `make build`, `build-linux` and `images`. The default (none)
# is the published build: opensandbox, daytona, freestyle, remote and devhost.
# GO_TAGS=experimental_adapters adds the other adapter kinds (docs/providers).
GO_TAGS ?=
EXPERIMENTAL_TAG := experimental_adapters
PG_PORT ?= 55432

.PHONY: build build-linux test test-postgres fmt-check vet experimental sdk-test helm-lint verify dev images clean

build:
	@mkdir -p bin
	CGO_ENABLED=0 go build -trimpath -tags "$(GO_TAGS)" -ldflags "$(LDFLAGS)" -o bin/ $(addprefix ./cmd/,$(BINS))

build-linux:
	@for arch in amd64 arm64; do \
		mkdir -p bin/linux-$$arch; \
		CGO_ENABLED=0 GOOS=linux GOARCH=$$arch go build -trimpath -tags "$(GO_TAGS)" -ldflags "$(LDFLAGS)" -o bin/linux-$$arch/ $(addprefix ./cmd/,$(BINS)) || exit 1; \
	done

fmt-check:
	@out=$$(gofmt -l cmd internal pkg api); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

# The second pass compiles and vets the tests that need a real stack or a live
# backend (test/e2e, the adapters' live tests). Nothing runs them here, so
# without it they can stop compiling unnoticed.
vet:
	go vet ./...
	go vet -tags "e2e live" ./...

test:
	go test -race -count=1 ./...

# The adapter kinds outside the default build must keep compiling and passing
# their fake-server tests: build, vet and test everything with the tag.
experimental:
	go build -tags $(EXPERIMENTAL_TAG) ./...
	go vet -tags "$(EXPERIMENTAL_TAG) e2e live" ./...
	go test -race -count=1 -tags $(EXPERIMENTAL_TAG) ./...

# Runs the store conformance suite against a disposable Postgres container.
test-postgres:
	@docker rm -f differently-pg-test >/dev/null 2>&1 || true
	docker run -d --rm --name differently-pg-test -p $(PG_PORT):5432 -e POSTGRES_PASSWORD=test postgres:17-alpine >/dev/null
	@for i in $$(seq 1 30); do docker exec differently-pg-test pg_isready -U postgres >/dev/null 2>&1 && break; sleep 1; done
	DIFFERENTLY_TEST_POSTGRES_URL=postgres://postgres:test@localhost:$(PG_PORT)/postgres?sslmode=disable go test -count=1 ./internal/store/postgres/; \
		status=$$?; docker rm -f differently-pg-test >/dev/null; exit $$status

sdk-test:
	cd sdk/typescript && npm ci --no-audit --no-fund && npm test
	cd sdk/python && python3 -m unittest discover -s tests

helm-lint:
	helm lint deploy/helm/differently
	@for f in deploy/helm/differently/ci/*.yaml; do \
		echo "helm template with $$f"; \
		helm template differently deploy/helm/differently -f $$f >/dev/null || exit 1; \
	done

verify: fmt-check vet test experimental sdk-test helm-lint

dev: build
	./bin/differently-server --dev

images:
	docker build -f build/server.Dockerfile --build-arg GO_TAGS=$(GO_TAGS) -t ghcr.io/different-ai/differently-server:$(VERSION) .
	docker build -f build/cli.Dockerfile -t ghcr.io/different-ai/differently-cli:$(VERSION) .

clean:
	rm -rf bin dist

# --- End-to-end tests: the real stack on any Docker host --------------------
#   make e2e-prepare e2e-up e2e e2e-down
# Set DOCKER_CONTEXT to choose the Docker host.
E2E_COMPOSE := test/e2e/compose.yaml

.PHONY: e2e-prepare e2e-up e2e e2e-down

e2e-prepare:
	docker build -q -f build/server.Dockerfile -t differently-server:e2e . >/dev/null
	docker pull -q opensandbox/execd:release-1.2.0 >/dev/null
	docker pull -q debian:bookworm-slim >/dev/null

e2e-up:
	docker compose -f $(E2E_COMPOSE) up -d --force-recreate --wait

e2e:
	DIFFERENTLY_E2E_COMPOSE=$(CURDIR)/$(E2E_COMPOSE) go test -tags e2e -count=1 -v -timeout 20m ./test/e2e/

e2e-down:
	docker compose -f $(E2E_COMPOSE) down -v

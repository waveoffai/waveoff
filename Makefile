# Waveoff — build, test and deploy.
#
# The only prerequisite is Go. controller-gen and setup-envtest are Go tool
# dependencies, so a fresh checkout needs no global installs.

SHELL := /usr/bin/env bash -o pipefail
ENVTEST_K8S_VERSION ?= 1.34.x
LOCALBIN := $(CURDIR)/bin
IMG ?= ghcr.io/waveoffai/waveoff-manager:latest
RECORDER_IMG ?= ghcr.io/waveoffai/waveoff-recorder:latest

.PHONY: all
all: generate fmt vet lint-boundary check-pins check-namespace test build

##@ Build

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X github.com/waveoffai/waveoff/internal/cli.Version=$(VERSION)

.PHONY: build
build: ## Build the CLI, the manager and the recorder sidecar.
	go build -ldflags "$(LDFLAGS)" -o $(LOCALBIN)/waveoff ./cmd/waveoff
	go build -o $(LOCALBIN)/manager ./cmd/manager
	go build -o $(LOCALBIN)/waveoff-recorder ./cmd/waveoff-recorder

.PHONY: generate
generate: ## Regenerate deepcopy, CRDs, webhook config and docs.
	go tool controller-gen object paths=./api/...
	go tool controller-gen "crd:allowDangerousTypes=true" webhook paths=./... \
		output:crd:artifacts:config=config/crd/bases \
		output:webhook:artifacts:config=config/webhook
	go run ./hack/gendocs

.PHONY: fmt
fmt:
	go fmt ./...

.PHONY: vet
vet:
	go vet ./...

##@ Test

.PHONY: test
test: test-unit test-envtest ## Unit and API-server tests. See test-integration and test-e2e for the rest.

.PHONY: test-unit
test-unit: ## Run the unit and property tests.
	go test ./api/... ./internal/... -coverprofile=cover.out

.PHONY: envtest-bins
envtest-bins: ## Download the API server binaries envtest needs.
	go tool setup-envtest use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path

.PHONY: test-envtest
test-envtest: envtest-bins ## Run the tests that need a real API server.
	KUBEBUILDER_ASSETS="$$(go tool setup-envtest use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)" \
		go test ./test/envtest/... -count=1

.PHONY: langgraph-env
langgraph-env: ## Create the Python environment the LangGraph integration test needs.
	python3 -m venv .venv
	./.venv/bin/pip install -q --upgrade pip
	./.venv/bin/pip install -q langgraph langchain-anthropic langchain-mcp-adapters \
		opentelemetry-sdk opentelemetry-instrumentation-httpx

.PHONY: test-integration
test-integration: ## Drive the recorder and replayer with a real LangGraph agent
                   ## against the real MCP reference server. Needs make langgraph-env and npx.
	go test ./test/integration/... -count=1 -timeout 15m

.PHONY: test-e2e
test-e2e: ## Run the end-to-end suite against a throwaway kind cluster. Needs Docker.
	./hack/e2e.sh

.PHONY: test-golden
test-golden: ## Rewrite the diff renderer's golden files.
	go test ./internal/diff/ -update

##@ Checks

.PHONY: lint-boundary
lint-boundary: ## Enforce the licensing boundary and the no-telemetry rule.
	./hack/check-boundary.sh

.PHONY: check-namespace
check-namespace: ## Prove the deployment namespace is really configurable.
	./hack/check-namespace.sh

.PHONY: check-pins
check-pins: ## Check workflow pins, interpolation, triggers and credentials.
	./hack/check-pins.sh

.PHONY: update-pins
update-pins: ## Re-resolve pinned Actions to their tags' current commits. Needs gh.
	./hack/update-pins.sh

.PHONY: lint-go
lint-go: ## Run golangci-lint. Downloads it on first use.
	@command -v golangci-lint >/dev/null || { 		echo "installing golangci-lint into $(LOCALBIN)"; 		GOBIN=$(LOCALBIN) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.4.0; 	}
	@PATH="$(LOCALBIN):$$PATH" golangci-lint run ./...

.PHONY: lint-spelling
lint-spelling: ## Spell-check the prose. Needs npx.
	npx --yes cspell@9.8.0 --no-progress --show-suggestions "**/*.md"

.PHONY: verify-generated
verify-generated: generate ## Fail if generated files are out of date.
	@if ! git diff --quiet -- api config docs; then \
		echo "generated files are out of date; run: make generate"; \
		git diff --stat -- api config docs; \
		exit 1; \
	fi

.PHONY: verify-samples
verify-samples: build ## Fail if the sample manifests' digests are stale.
	$(LOCALBIN)/waveoff verify config/samples/*.yaml

##@ Deploy

.PHONY: install
install: ## Install the CRD into the current cluster.
	kubectl apply -k config/crd

.PHONY: uninstall
uninstall:
	kubectl delete -k config/crd --ignore-not-found

.PHONY: deploy
deploy: ## Deploy the webhook. Requires cert-manager.
	cd config/manager && kubectl kustomize . >/dev/null
	kubectl apply -k config/default

.PHONY: undeploy
undeploy:
	kubectl delete -k config/default --ignore-not-found

.PHONY: docker-build
docker-build: ## Build both cluster images.
	docker build --build-arg TARGET=manager -t $(IMG) .
	docker build --build-arg TARGET=waveoff-recorder -t $(RECORDER_IMG) .

.PHONY: quickstart
quickstart: ## Bring up a kind cluster with Waveoff installed and a manifest applied.
	./hack/quickstart.sh

.PHONY: bench
bench: ## Run the recorder latency benchmarks.
	go test ./internal/recorder/ -run XXX -bench . -benchtime 300x

.PHONY: help
help: ## Show this help.
	@awk 'BEGIN {FS = ":.*##"} /^[a-zA-Z_-]+:.*?##/ { printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)

# Weft
IMG ?= ghcr.io/alethic/weft:latest
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT ?= $(shell git rev-parse HEAD 2>/dev/null)
DATE ?= $(shell git log -1 --format=%cI 2>/dev/null)
PKG := github.com/alethic/weft/internal/version
LDFLAGS := -X $(PKG).Version=$(VERSION) -X $(PKG).Commit=$(COMMIT) -X $(PKG).Date=$(DATE)
CHART ?= charts/weft
NAMESPACE ?= weft-system
CONTROLLER_GEN ?= go tool controller-gen
SETUP_ENVTEST ?= go tool setup-envtest
# Pinned so a local run and CI disagree about nothing. Bumping it here is the
# only place it changes.
GOLANGCI_LINT_VERSION ?= v2.13.2
GOLANGCI_LINT ?= $(shell command -v golangci-lint 2>/dev/null || echo $(shell go env GOPATH)/bin/golangci-lint)
ENVTEST_K8S_VERSION ?= 1.34.x
ENVTEST_DIR := $(CURDIR)/bin/envtest

.PHONY: all
all: generate fmt vet lint test build

.PHONY: generate
generate: ## Regenerate deepcopy, the CRD and the controller ClusterRole.
	$(CONTROLLER_GEN) object paths=./api/v1alpha1/...
	$(CONTROLLER_GEN) crd paths=./api/v1alpha1/... output:crd:artifacts:config=config/crd
	$(CONTROLLER_GEN) rbac:roleName=weft-controller paths=./internal/controller/... output:rbac:artifacts:config=config/rbac
	# The chart ships the generated CRD verbatim so the two cannot drift.
	# TestChartCRDMatchesGenerated fails if this copy is skipped.
	cp config/crd/weft.run_weaves.yaml $(CHART)/files/weaves-crd.yaml

.PHONY: fmt
fmt:
	go fmt ./...

.PHONY: vet
vet:
	go vet ./...

.PHONY: lint
lint: ## Run golangci-lint, installing the pinned version if it is missing.
	@if ! $(GOLANGCI_LINT) --version 2>/dev/null | grep -q "$(patsubst v%,%,$(GOLANGCI_LINT_VERSION))"; then 		echo "installing golangci-lint $(GOLANGCI_LINT_VERSION)"; 		curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/HEAD/install.sh 			| sh -s -- -b $(shell go env GOPATH)/bin $(GOLANGCI_LINT_VERSION); 	fi
	$(GOLANGCI_LINT) run ./...

.PHONY: vulncheck
vulncheck: ## Check dependencies against the Go vulnerability database.
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

.PHONY: envtest
envtest: ## Download the control-plane binaries the controller tests run against.
	$(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(ENVTEST_DIR) -p path

.PHONY: test
test: ## All tests. Needs helm for the chart tests and envtest for the controller tests.
	go test ./... -count=1

.PHONY: test-all
test-all: envtest test ## Fetch the control plane first, then run everything.

.PHONY: build
build:
	go build -ldflags "$(LDFLAGS)" -o bin/weft ./cmd/weft

.PHONY: docker-build
docker-build:
	docker build -t $(IMG) 		--build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) --build-arg DATE=$(DATE) .

.PHONY: lint-chart
lint-chart: ## Lint and render the chart.
	helm lint $(CHART)
	helm template weft $(CHART) --namespace $(NAMESPACE) >/dev/null

.PHONY: install
install: ## Install just the CRD.
	kubectl apply -f config/crd/weft.run_weaves.yaml

.PHONY: deploy
deploy: ## Install the controller with Helm.
	helm upgrade --install weft $(CHART) --namespace $(NAMESPACE) --create-namespace

.PHONY: undeploy
undeploy: ## Remove the controller. Run 'make reap' first if any Weave uses finalize.
	helm uninstall weft --namespace $(NAMESPACE)

.PHONY: reap
reap: ## Release every finalizer Weft has placed. Run before undeploy.
	go run ./cmd/weft reap

.PHONY: run
run: ## Run the controller locally against the current kubecontext.
	go run ./cmd/weft

.PHONY: help
help:
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-14s\033[0m %s\n", $$1, $$2}'

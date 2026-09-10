# Weft
IMG ?= ghcr.io/alethic/weft:latest
CONTROLLER_GEN ?= go run sigs.k8s.io/controller-tools/cmd/controller-gen

.PHONY: all
all: generate fmt vet test build

.PHONY: generate
generate: ## Regenerate deepcopy functions, the CRD and the controller ClusterRole.
	$(CONTROLLER_GEN) object paths=./api/v1alpha1/...
	$(CONTROLLER_GEN) crd paths=./api/v1alpha1/... output:crd:artifacts:config=config/crd
	$(CONTROLLER_GEN) rbac:roleName=weft-controller paths=./internal/controller/... output:rbac:artifacts:config=config/rbac

.PHONY: fmt
fmt:
	go fmt ./...

.PHONY: vet
vet:
	go vet ./...

.PHONY: test
test:
	go test ./... -count=1

.PHONY: build
build:
	go build -o bin/weft ./cmd/weft

.PHONY: docker-build
docker-build:
	docker build -t $(IMG) .

.PHONY: install
install: ## Install just the CRD.
	kubectl apply -f config/crd/weft.run_weaves.yaml

.PHONY: deploy
deploy: ## Install the CRD, RBAC and the controller.
	kubectl kustomize config | kubectl apply -f -

.PHONY: undeploy
undeploy: ## Remove the controller. Run 'make reap' first if any Weave uses finalize.
	kubectl kustomize config | kubectl delete --ignore-not-found -f -

.PHONY: reap
reap: ## Release every finalizer Weft has placed. Run before undeploy.
	go run ./cmd/weft reap

.PHONY: run
run: ## Run the controller locally against the current kubecontext.
	go run ./cmd/weft

.PHONY: help
help:
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-16s\033[0m %s\n", $$1, $$2}'

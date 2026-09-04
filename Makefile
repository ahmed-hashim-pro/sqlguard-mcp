.PHONY: help build test lint fmt check clean image kind-up kind-down deploy undeploy k8s-verify chart-lint

help:            ## Show this help
	@grep -E '^[a-z-]+:.*##' $(MAKEFILE_LIST) | sed 's/:.*##/\t/'

build:           ## Build the sqlguard binary into ./bin
	go build -o bin/sqlguard ./cmd/sqlguard

test:            ## Run all tests with the race detector
	go test -race ./...

lint:            ## Vet and check formatting
	go vet ./...
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then \
		echo "not gofmt'd:"; echo "$$unformatted"; exit 1; \
	fi

fmt:             ## Format all Go source
	gofmt -w .

check: lint test ## Everything CI runs

clean:           ## Remove build output
	rm -rf bin

# --- container and Kubernetes -------------------------------------------------

IMAGE ?= sqlguard:dev
CLUSTER ?= sqlguard

image:           ## Build the container image
	docker build -f deploy/Dockerfile -t $(IMAGE) --build-arg VERSION=$(shell git describe --tags --always --dirty 2>/dev/null || echo dev) .

kind-up:         ## Create a local kind cluster
	kind get clusters 2>/dev/null | grep -qx $(CLUSTER) || kind create cluster --name $(CLUSTER)
	kubectl config use-context kind-$(CLUSTER)

kind-down:       ## Delete the local kind cluster
	kind delete cluster --name $(CLUSTER)

chart-lint:      ## Lint the Helm chart and render it
	helm lint chart/
	helm template sqlguard chart/ > /dev/null

# kind load, rather than a registry: the image never leaves this machine.
deploy: image kind-up ## Build, load and install the chart into kind
	kind load docker-image $(IMAGE) --name $(CLUSTER)
	helm upgrade --install sqlguard ./chart --wait --timeout 5m
	@echo
	@echo "kubectl port-forward svc/sqlguard 8080:8080"

undeploy:        ## Remove the release (the PVC is deleted too)
	helm uninstall sqlguard || true
	kubectl delete pvc sqlguard-data --ignore-not-found

k8s-verify:      ## Prove the deployment works: probes, a read, and a refused write
	./deploy/verify.sh

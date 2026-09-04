.PHONY: help build test lint fmt check clean

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

.PHONY: help lint lint-fix test build clean install

# default target when just running 'make'
help:
	@echo "Available targets:"
	@echo "  make lint         - run linters on the codebase"
	@echo "  make lint-fix     - run linters and auto-fix issues where possible"
	@echo "  make test         - run all tests"
	@echo "  make build        - build the ev-metrics binary"
	@echo "  make install      - install ev-metrics to GOPATH/bin"
	@echo "  make clean        - remove build artifacts"

# run golangci-lint on the codebase
lint:
	@echo "Running golangci-lint..."
	golangci-lint run ./...

# run golangci-lint with auto-fix
lint-fix:
	@echo "Running golangci-lint with auto-fix..."
	golangci-lint run --fix ./...

# run all tests
test:
	@echo "Running tests..."
	go test -race -v ./...

# build the binary
build:
	@echo "Building ev-metrics..."
	go build -o ev-metrics

# install to GOPATH/bin
install:
	@echo "Installing ev-metrics..."
	go install

# clean build artifacts
clean:
	@echo "Cleaning build artifacts..."
	rm -f ev-metrics

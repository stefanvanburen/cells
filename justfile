# Run lint and test.
default: check

# Run all tests.
test:
    go test -race ./...

# Run all tests with verbose output.
test-v:
    go test -race -v ./...

# Run go vet, staticcheck, and go fix.
lint:
    go vet ./...
    staticcheck ./...
    go fix ./...

# Format all Go files.
fmt:
    gofmt -w .

# Run lint and test.
check: lint test

# Install the cells binary.
install:
    go install ./cmd/cells/...

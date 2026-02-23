# Run lint and test.
default: check

# Run all tests.
test:
    go test -race ./...

# Run go vet and staticcheck.
lint:
    go vet ./...
    go tool staticcheck ./...

# Format all Go files and run automatic fixes.
fmt:
    gofmt -w .
    go fix ./...

# Run lint and test.
check: lint test

# Install the cells binary.
install:
    go install ./cmd/cells/...

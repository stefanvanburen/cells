# Run lint and test.
default: check

# Run all tests.
test:
    go test -race ./...


# Format all Go files and run automatic fixes.
fmt:
    gofmt -w .
    go fix ./...

# Run the prek hooks: file checks, gofmt, go fix, go vet, staticcheck and
# tests.
check:
    prek run --all-files

# Install the cells binary.
install:
    go install ./cmd/cells/...

# Run lint and test.
default: check

# Run all tests.
test:
    go test -race ./...

# Run go vet and staticcheck.
lint:
    # -printf.funcs names ok.Sprintf so vet checks the format strings inside
    # assertion options; printf's default function list does not include it,
    # and go test's built-in vet cannot be passed analyzer flags.
    go vet -printf.funcs=Sprintf ./...
    go tool staticcheck ./...
    go fix -diff ./...
    test -z "$(gofmt -l .)" || (echo "gofmt needed on:"; gofmt -l .; exit 1)

# Format all Go files and run automatic fixes.
fmt:
    gofmt -w .
    go fix ./...

# Run lint and test.
check: lint test

# Install the cells binary.
install:
    go install ./cmd/cells/...

.PHONY: test lint fmt deps check

# Run all tests
test:
	go test -v ./...

# Run tests with coverage
test-coverage:
	go test -v -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html

# Run linter
lint:
	golangci-lint run ./...

# Format code
fmt:
	gofmt -s -w .
	gofumpt -w .

# Download dependencies
deps:
	go mod download
	go mod tidy

# Run all checks
check: fmt lint test

# Clean build artifacts
clean:
	rm -f coverage.out coverage.html

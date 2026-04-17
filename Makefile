.PHONY: build test lint run clean docker-build docker-up docker-down

# Build the server binary
build:
	go build -o server ./cmd/server

# Run all tests
test:
	go test ./...

# Run tests with verbose output
test-v:
	go test -v ./...

# Run tests with race detector
test-race:
	go test -race ./...

# Run golangci-lint (install: https://golangci-lint.run/usage/install/)
lint:
	golangci-lint run ./...

# Run the server locally (requires Redis)
run: build
	./server

# Remove build artifacts
clean:
	rm -f server

# Build Docker image
docker-build:
	docker compose build

# Start all services
docker-up:
	docker compose up -d --build

# Stop all services
docker-down:
	docker compose down

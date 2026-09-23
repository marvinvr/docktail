.PHONY: build run clean docker-build docker-push test fmt vet docs-generate

# Variables
BINARY_NAME=docktail
DOCKER_IMAGE=ghcr.io/marvinvr/docktail
VERSION?=latest

# Build the binary
build:
	go build -o $(BINARY_NAME) .

# Run the application locally
run: build
	./$(BINARY_NAME)

# Clean build artifacts
clean:
	go clean
	rm -f $(BINARY_NAME)

# Build Docker image
docker-build:
	docker build --build-arg VERSION=$(VERSION) -t $(DOCKER_IMAGE):$(VERSION) .

# Render docs/*.md into ./.docs-preview for a local look at the output.
# The published site is built in the docktail-website repo, which pins this
# repo as a submodule and runs the same generator.
docs-generate:
	go run ./tools/docsgen

# Push Docker image
docker-push: docker-build
	docker push $(DOCKER_IMAGE):$(VERSION)

# Run tests
test:
	go test -v ./...

# Format code
fmt:
	go fmt ./...

# Run go vet
vet:
	go vet ./...

# Install dependencies
deps:
	go mod download
	go mod tidy

# Run all checks
check: fmt vet test

# Build for multiple platforms
build-all:
	GOOS=linux GOARCH=amd64 go build -o $(BINARY_NAME)-linux-amd64 .
	GOOS=linux GOARCH=arm64 go build -o $(BINARY_NAME)-linux-arm64 .
	GOOS=darwin GOARCH=amd64 go build -o $(BINARY_NAME)-darwin-amd64 .
	GOOS=darwin GOARCH=arm64 go build -o $(BINARY_NAME)-darwin-arm64 .

# Start the development stack (builds from source; see docker-compose.dev.yaml)
up:
	docker compose -f docker-compose.dev.yaml up -d

# Stop the development stack
down:
	docker compose -f docker-compose.dev.yaml down

# View the development stack's DockTail logs
logs:
	docker logs -f dev-docktail

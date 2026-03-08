.PHONY: build build-linux build-docker push run clean

# Default target registry - override with REGISTRY=your-registry
REGISTRY ?= localhost
IMAGE_NAME ?= jeembot
TAG ?= latest

# Build for local machine
build:
	go build -o bin/jeembot .

# Build for Linux 64-bit (cross-compilation for RPI/server)
build-linux:
	GOOS=linux GOARCH=amd64 go build -o bin/jeembot-linux .

# Build Docker image
build-docker:
	docker build -t $(REGISTRY)/$(IMAGE_NAME):$(TAG) .

push:
	docker push $(REGISTRY)/$(IMAGE_NAME):$(TAG)

run:
	docker-compose up --build

clean:
	docker-compose down -v

# Development helpers
dev: build
	docker run --rm -it \
		--env-file .env \
		-p 8080:8080 \
		$(REGISTRY)/$(IMAGE_NAME):$(TAG)

# Test webhook locally
test-webhook:
	@echo "Testing webhook endpoint..."
	@echo '{"text":"@jeembot /to cti Test task from webhook"}' | \
	curl -X POST http://localhost:8080/teams/webhook \
		-H "Content-Type: application/json" \
		-H "Authorization: HMAC $$(echo -n '{\"text\":\"@jeembot /to cti Test task from webhook\"}' | openssl dgst -sha256 -hmac '$(TEAMS_HMAC_SECRET)' -r | cut -d' ' -f1)" \
		-d @-

.PHONY: build push run clean

# Default target registry - override with REGISTRY=your-registry
REGISTRY ?= jeembot
IMAGE_NAME ?= teams-webhook
TAG ?= latest

build:
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

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build test vet lint validate docker
build:
	CGO_ENABLED=0 go build -ldflags '$(LDFLAGS)' -o bin/release-digest ./cmd/release-digest

test:
	go test ./...

vet:
	go vet ./...

# Lints the local-dev config and the one baked into the image.
lint: build
	./bin/release-digest config lint
	RELEASE_DIGEST_PUBLIC_BASE_URL=https://rss.example.com ./bin/release-digest config lint --config deploy/release-digest.yaml

# Posts the rendered feed to the W3C Feed Validation Service. Needs a store
# populated by `release-digest build`. Run it locally or in CI, not on every commit.
validate: build
	./bin/release-digest validate --w3c

# The container image. Run it with `docker compose up -d`.
docker:
	docker build --build-arg VERSION=$(VERSION) -t release-digest:latest .

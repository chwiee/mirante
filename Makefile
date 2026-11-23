.PHONY: build test demo docker docs-ia

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

build:
	go build -ldflags "-X main.version=$(VERSION)" -o bin/mirante ./cmd/mirante

test:
	go vet ./... && go test ./...

demo:
	go run ./cmd/mirante --demo

docker:
	docker build --build-arg VERSION=$(VERSION) -t mirante:dev .

# regenera docs/ia a partir do template (web/ia); o teste falha se esquecer
docs-ia:
	go test ./internal/iadoc -update

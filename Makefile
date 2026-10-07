.PHONY: build test demo docker

build:
	go build -o bin/mirante ./cmd/mirante

test:
	go vet ./... && go test ./...

demo:
	go run ./cmd/mirante --demo

docker:
	docker build -t mirante:dev .

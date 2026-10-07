.PHONY: build test demo docker docs-ia

build:
	go build -o bin/mirante ./cmd/mirante

test:
	go vet ./... && go test ./...

demo:
	go run ./cmd/mirante --demo

docker:
	docker build -t mirante:dev .

# regenera docs/ia a partir do template (web/ia); o teste falha se esquecer
docs-ia:
	go test ./internal/iadoc -update

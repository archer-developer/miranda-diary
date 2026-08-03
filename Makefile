.PHONY: build run fmt lint test check tools deploy

build:
	CGO_ENABLED=0 go build -o miranda-diary ./cmd/miranda-diary

run: build
	./miranda-diary

fmt:
	gofmt -w .
	goimports -w .

lint:
	golangci-lint run ./...

test:
	go test ./... -race

check: fmt lint test

tools:
	go install golang.org/x/tools/cmd/goimports@latest
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest

deploy:
	./scripts/deploy.sh

BINARY := bin/alert-system
VERSION ?= dev

.PHONY: build run test tidy

build:
	go build -ldflags "-X main.version=$(VERSION)" -o $(BINARY) ./cmd/server

run:
	go run -ldflags "-X main.version=$(VERSION)" ./cmd/server

test:
	go test ./...

tidy:
	go mod tidy

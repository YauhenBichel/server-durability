.PHONY: build test lint dist

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null | sed 's/^v//')
LDFLAGS := -s -w -X main.version=$(VERSION)

build:
	go build -ldflags="$(LDFLAGS)" -o server-durability ./cmd/server-durability

test:
	go test -race -count=1 ./...

lint:
	go vet ./...
	test -z "$$(gofmt -l .)"

dist:
	mkdir -p dist
	CGO_ENABLED=0 GOOS=linux  GOARCH=amd64 go build -trimpath -ldflags="$(LDFLAGS)" -o dist/server-durability-linux-amd64 ./cmd/server-durability
	CGO_ENABLED=0 GOOS=linux  GOARCH=arm64 go build -trimpath -ldflags="$(LDFLAGS)" -o dist/server-durability-linux-arm64 ./cmd/server-durability
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags="$(LDFLAGS)" -o dist/server-durability-darwin-arm64 ./cmd/server-durability

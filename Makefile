BINARY := lion
PKG := ./cmd/lion
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X github.com/jodok/lion/internal/cli.version=$(VERSION)

.PHONY: build test vet lint fmt install clean

build:
	go build -ldflags "$(LDFLAGS)" -o bin/$(BINARY) $(PKG)

test:
	go test ./...

vet:
	go vet ./...

lint:
	golangci-lint run ./...

fmt:
	gofmt -s -w .

install:
	go install -ldflags "$(LDFLAGS)" $(PKG)

clean:
	rm -rf bin

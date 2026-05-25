BINARY  := juice
PKG     := github.com/daios-ai/juice/cmd/juice
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
LDFLAGS := -ldflags "-X main.version=$(VERSION) -X main.commit=$(COMMIT)"

.PHONY: build test clean install

build:
	go build $(LDFLAGS) -o $(BINARY) $(PKG)

test:
	go test ./... -count=1

clean:
	rm -f $(BINARY)

install:
	go install $(LDFLAGS) $(PKG)

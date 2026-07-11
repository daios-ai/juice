BINARY  := juice
PKG     := github.com/daios-ai/juice/cmd/juice
PREFIX  ?= $(HOME)/.local
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
LDFLAGS := -ldflags "-X main.version=$(VERSION) -X main.commit=$(COMMIT)"

.PHONY: build test clean install uninstall

build:
	go build $(LDFLAGS) -o $(BINARY) $(PKG)

test:
	go test ./... -count=1

clean:
	rm -f $(BINARY)

install: build
	mkdir -p $(PREFIX)/bin
	install -m 755 $(BINARY) $(PREFIX)/bin/$(BINARY)

# Removes the installed binary only; $JUICE_HOME/kernel/ data (signing key, config) is left intact.
uninstall:
	rm -f $(PREFIX)/bin/$(BINARY)

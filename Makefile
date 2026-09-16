BINARY  := juice
PKG     := github.com/daios-ai/juice/cmd/juice
# The installation root, as install.sh and the kernel use it: everything juice owns
# lives under one directory, the binary included.
JUICE_HOME ?= $(HOME)/.juice
PREFIX  ?= $(JUICE_HOME)
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

# Removes the installed binary only; the kernels beside it (signing keys, ledgers, configs)
# are left intact.
uninstall:
	rm -f $(PREFIX)/bin/$(BINARY)

# The network suite: a five-kernel economy driven end to end over a real network. Not the commit
# gate — run it after anything that touches federation or money. Writes its log, checkpoints and
# metrics to netsim-runs/<rail>-<timestamp>/. RAIL=anvil needs Foundry; RAIL=sepolia needs an RPC
# and a funded key. See docs/network-simulation.md.
.PHONY: netsim
netsim:
	go run ./netsim -rail $(or $(RAIL),play) $(if $(ROUNDS),-rounds $(ROUNDS),)

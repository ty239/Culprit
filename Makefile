BINARY     := kubectl-culprit
CMD_PATH   := ./cmd/kubectl-culprit
GOBIN      ?= $(shell go env GOBIN)
ifeq ($(GOBIN),)
GOBIN      := $(shell go env GOPATH)/bin
endif
KREW_STORE := $(HOME)/.krew/store/culprit

.PHONY: build test lint install clean

build:
	go build -o $(BINARY) $(CMD_PATH)

test:
	go test ./...

lint:
	golangci-lint run

install: build
	@if [ -d "$(HOME)/.krew/store" ]; then \
		mkdir -p "$(KREW_STORE)/$(shell git describe --tags --always 2>/dev/null || echo dev)"; \
		cp $(BINARY) "$(KREW_STORE)/$(shell git describe --tags --always 2>/dev/null || echo dev)/$(BINARY)"; \
		echo "Installed to krew store: $(KREW_STORE)"; \
	else \
		cp $(BINARY) "$(GOBIN)/$(BINARY)"; \
		echo "Installed to GOBIN: $(GOBIN)/$(BINARY)"; \
	fi

clean:
	rm -f $(BINARY)

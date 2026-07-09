GO ?= go

.PHONY: check fmt vet test build run-echo zip

check: fmt vet test

fmt:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

vet:
	$(GO) vet ./...

test:
	$(GO) test ./...

build:
	$(GO) build ./...

# Smoke-test the engine end to end with the fake adapter.
run-echo:
	@tmp=$$(mktemp -d) && $(GO) run ./cmd/agentchat-cli -client echo -dir $$tmp "hello from make" && rm -rf $$tmp

zip:
	cd .. && zip -qr agentchat.zip agentchat -x 'agentchat/frontend/node_modules/*' && echo ../agentchat.zip

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

# Wails desktop app (nested module in app/; needs network + wails CLI).
# Wails v2 links against webkit2gtk-4.0 by default; newer distros (e.g.
# Ubuntu 24.04+) ship only webkit2gtk-4.1, which wails supports behind the
# webkit2_41 build tag. Autodetect: prefer 4.0 if present, else use 4.1.
WAILS_TAGS ?= $(shell pkg-config --exists webkit2gtk-4.0 2>/dev/null || ! pkg-config --exists webkit2gtk-4.1 2>/dev/null || echo webkit2_41)

app-tidy:
	cd app && $(GO) mod tidy

app-build-check: app-tidy
	cd app && $(GO) build -tags "desktop,production,$(WAILS_TAGS)" ./... && $(GO) vet -tags "desktop,production,$(WAILS_TAGS)" ./...

app-dev: app-tidy
	cd app && wails dev $(if $(WAILS_TAGS),-tags $(WAILS_TAGS))

app-build: app-tidy
	cd app && wails build $(if $(WAILS_TAGS),-tags $(WAILS_TAGS))

zip:
	cd .. && zip -qr agentchat.zip agentchat -x 'agentchat/app/build/*' 'agentchat/app/frontend/node_modules/*' && echo ../agentchat.zip

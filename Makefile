BIN     := bin/dwt
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
GOFILES := cmd internal
LABEL   := com.local.dontwastetime
PLIST   := $(HOME)/Library/LaunchAgents/$(LABEL).plist
STATE   ?= $(HOME)/Library/Application Support/dontwastetime

.PHONY: build check test install install-agent uninstall-agent render-plist assistant-check

build:
	go build -ldflags "-X main.version=$(VERSION)" -o $(BIN) ./cmd/dwt

check:
	@out="$$(gofmt -l $(GOFILES))"; if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi
	go vet ./...
	go test -race ./...

test:
	go test -race ./...

assistant-check:
	cd assistant && uv run pytest -q

# install = build + launchd agent. Run it from the checkout that holds
# config.json, .env and taxonomy.json: that directory is the agent's
# working directory.
install: install-agent

render-plist:
	@sed -e "s|__DWT_BIN__|$(CURDIR)/$(BIN)|g" -e "s|__DWT_DIR__|$(CURDIR)|g" -e "s|__DWT_LOGS__|$(STATE)/logs|g" \
		deploy/$(LABEL).plist | awk -v dry_run="$(DRY_RUN)" \
		'{ print; if (dry_run == "1" && /<string>serve<\/string>/) print "    <string>--dry-run</string>" }'

install-agent: build
	@test -f config.json || { echo "config.json missing: cp config.example.json config.json and edit it"; exit 1; }
	@test -f .env || { echo ".env missing: cp .env.example .env and fill it"; exit 1; }
	./$(BIN) doctor --config config.json
	mkdir -p "$(STATE)/logs" "$(HOME)/Library/LaunchAgents"
	$(MAKE) -s render-plist > "$(PLIST)"
	plutil -lint "$(PLIST)"
	-launchctl bootout gui/$$(id -u)/$(LABEL) 2>/dev/null
	launchctl bootstrap gui/$$(id -u) "$(PLIST)"
	@echo "installed $(PLIST); logs in $(STATE)/logs"

uninstall-agent:
	-launchctl bootout gui/$$(id -u)/$(LABEL)
	rm -f "$(PLIST)"

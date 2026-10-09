.PHONY: build install uninstall icons test unit check clean run release

BIN := dist/dsh-desktop

# webview's bundled C library asks pkg-config for webkit2gtk-4.0, which modern
# distributions no longer ship. pkgconfig/webkit2gtk-4.0.pc forwards to 4.1.
export PKG_CONFIG_PATH := $(CURDIR)/pkgconfig$(if $(PKG_CONFIG_PATH),:$(PKG_CONFIG_PATH))

VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.1.0)

build:
	@mkdir -p dist
	go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o $(BIN) .
	@echo "built $(BIN) ($$(du -h $(BIN) | cut -f1), version $(VERSION))"

run: build
	./$(BIN)

icons:
	python3 scripts/render-icons.py

install:
	./scripts/install.sh

uninstall:
	./scripts/install.sh --uninstall

# Unit tests cover port selection, the URL pattern, and the default port range.
# They need no display and no dsh install.
unit:
	go test ./...

# Boots a real host, loads the UI in a real WebKit window, inspects the rendered
# document and prints a verdict. This is the only check that proves the client
# actually executed, which is what "the window is blank" failures need.
test: build unit
	./scripts/smoke.sh

check: vet unit

vet:
	gofmt -l . | (! grep .) || (echo "gofmt needed"; exit 1)
	go vet ./...

# A portable tarball for the GitHub release: the binary plus what a user needs to
# run it and to rebuild it on their own machine.
release: build
	@mkdir -p dist
	@rm -rf dist/dsh-desktop-$(VERSION)-linux-x86_64
	@mkdir -p dist/dsh-desktop-$(VERSION)-linux-x86_64
	@cp $(BIN) README.md LICENSE THIRD_PARTY_NOTICES.md dist/dsh-desktop-$(VERSION)-linux-x86_64/
	@mkdir -p dist/dsh-desktop-$(VERSION)-linux-x86_64/scripts
	@cp scripts/install.sh dist/dsh-desktop-$(VERSION)-linux-x86_64/scripts/
	@mkdir -p dist/dsh-desktop-$(VERSION)-linux-x86_64/assets
	@cp -r assets/icons dist/dsh-desktop-$(VERSION)-linux-x86_64/assets/
	@tar -C dist -czf dist/dsh-desktop-$(VERSION)-linux-x86_64.tar.gz dsh-desktop-$(VERSION)-linux-x86_64
	@rm -rf dist/dsh-desktop-$(VERSION)-linux-x86_64
	@echo "packaged dist/dsh-desktop-$(VERSION)-linux-x86_64.tar.gz"

clean:
	rm -rf dist

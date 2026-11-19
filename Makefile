# tree-sitter needs CGO, so cross-compiling from one host is not practical;
# release binaries are built natively per-OS by .github/workflows/release.yml.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  = -s -w -X main.version=$(VERSION)
GOOS    ?= $(shell go env GOOS)
GOARCH  ?= $(shell go env GOARCH)
EXT      = $(if $(filter windows,$(GOOS)),.exe,)
DIST     = dist

.PHONY: ui build dist checksums clean

ui:
	cd web && npm install && npm run build
	rm -rf internal/webui/dist
	mkdir -p internal/webui/dist
	cp -R web/dist/. internal/webui/dist/
	printf 'Built by make ui\n' > internal/webui/dist/README

build: ui
	go build -ldflags '$(LDFLAGS)' -o gatt$(EXT) ./cmd/gatt

dist: ui
	CGO_ENABLED=1 go build -trimpath -ldflags '$(LDFLAGS)' \
		-o $(DIST)/gatt-$(GOOS)-$(GOARCH)$(EXT) ./cmd/gatt

checksums:
	cd $(DIST) && sha256sum gatt-* > checksums.txt

clean:
	rm -rf $(DIST) gatt gatt.exe web/dist internal/webui/dist/*
	printf 'Placeholder so go:embed all:dist succeeds before make ui.\n' > internal/webui/dist/README

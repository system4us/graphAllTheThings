# tree-sitter needs CGO, so cross-compiling from one host is not practical;
# release binaries are built natively per-OS by .github/workflows/release.yml.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  = -s -w -X main.version=$(VERSION)
GOOS    ?= $(shell go env GOOS)
GOARCH  ?= $(shell go env GOARCH)
EXT      = $(if $(filter windows,$(GOOS)),.exe,)
DIST     = dist

.PHONY: build dist checksums clean

build:
	go build -ldflags '$(LDFLAGS)' -o gatt$(EXT) ./cmd/gatt

dist:
	CGO_ENABLED=1 go build -trimpath -ldflags '$(LDFLAGS)' \
		-o $(DIST)/gatt-$(GOOS)-$(GOARCH)$(EXT) ./cmd/gatt

checksums:
	cd $(DIST) && sha256sum gatt-* > checksums.txt

clean:
	rm -rf $(DIST) gatt gatt.exe

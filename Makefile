all: build

SHELL := /bin/bash
.DEFAULT_GOAL := all

S3_PLUGIN := gpbackup_s3_plugin

# Where the built binary lands. This is a contract with the RPM packaging, which
# runs `make build` and then collects binaries out of $GOPATH/bin, so it must
# keep pointing there. GOPATH may be a list; only the first element is used,
# matching the go command. Unset falls back to the go command's own default,
# which is why this needs no GOPATH guard.
BIN_DIR := $(shell echo $${GOPATH:-~/go} | awk -F':' '{ print $$1 "/bin"}')

GIT_VERSION := $(shell { git describe --tags --always 2>/dev/null || echo "v0.0.0-NoTag"; } | perl -pe 's/(.*)-([0-9]*)-(g[0-9a-f]*)/\1+dev.\2.\3/')

# The -X argument names a variable by its full package path, so it has to track
# the module path in go.mod. Changing one without the other leaves the version
# unstamped and reports nothing: the linker does not treat an unmatched symbol
# as an error.
PLUGIN_VERSION_STR := "-X github.com/greenplum-db/gpbackup-s3-plugin/s3plugin.Version=$(GIT_VERSION)"

DEBUG := -gcflags=all="-N -l"

# gofmt and goimports run as golangci-lint v2 formatters, configured in
# .golangci.yml, so they need no separate installation of their own.
GOLANGCI_LINT_VERSION := v2.11.4

.PHONY: all depend build build_linux build_mac debug install lint format vet \
	unit test tools clean

depend:
	go mod download

build: depend
	go build -o $(BIN_DIR)/$(S3_PLUGIN) -ldflags $(PLUGIN_VERSION_STR)

build_linux: depend
	env GOOS=linux GOARCH=amd64 go build -o $(S3_PLUGIN) -ldflags $(PLUGIN_VERSION_STR)

build_mac: depend
	env GOOS=darwin GOARCH=amd64 go build -o $(BIN_DIR)/$(S3_PLUGIN) -ldflags $(PLUGIN_VERSION_STR)

debug: depend
	go build $(DEBUG) -o $(BIN_DIR)/$(S3_PLUGIN) -ldflags $(PLUGIN_VERSION_STR)

lint:
	golangci-lint run

# Rewrites files in place, rather than just reporting, for local use.
format:
	golangci-lint fmt

vet:
	go vet ./...

# ginkgo is pinned by the `tool` directive in go.mod, so `go tool` runs the
# version this module was tested against instead of whatever @latest resolves to.
unit:
	go tool ginkgo -r --keep-going --randomize-suites --randomize-all --no-color s3plugin 2>&1

test: lint vet unit

# golangci-lint is intentionally not a `tool` directive: it does not support
# being built as a library dependency.
tools:
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

# gpsync is the current name, gpscp the older one. Recursively expanded so this
# is resolved only when install runs, rather than on every make invocation.
COPYUTIL = $(shell command -v gpsync >/dev/null 2>&1 && echo gpsync || echo gpscp)

install: build
	@psql -t -d template1 -c 'select distinct hostname from gp_segment_configuration' > /tmp/seg_hosts 2>/dev/null; \
	if [ $$? -eq 0 ]; then \
		$(COPYUTIL) -f /tmp/seg_hosts $(BIN_DIR)/$(S3_PLUGIN) =:$(GPHOME)/bin/$(S3_PLUGIN); \
		if [ $$? -eq 0 ]; then \
			echo 'Successfully copied $(S3_PLUGIN) to $(GPHOME) on all segments'; \
		else \
			echo 'Failed to copy $(S3_PLUGIN) to $(GPHOME)'; \
		fi; \
	else \
		echo 'Database is not running, please start the database and run this make target again'; \
	fi; \
	rm /tmp/seg_hosts

clean:
	# Build artifacts
	rm -f $(BIN_DIR)/$(S3_PLUGIN)
	# Test artifacts
	rm -rf /tmp/go-build*
	rm -rf /tmp/gexec_artifacts*
	rm -rf /tmp/ginkgo*

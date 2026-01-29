.PHONY: all help proto proto.clean certs certs.clean server user clean \
        chroot chroot.clean chroot.nuke \
        deps tidy fmt vet test build build.bin install \
        jobctl jobworker-server run.server run.server.sudo

.DEFAULT_GOAL := all

CHROOT_SCRIPT := ./scripts/chroot.sh

LOG_PATH ?= $(abspath ./jobworker-server.log)

# ---- Go build settings ----
GO       ?= go
BIN_DIR  ?= ./bin
JOBCTL_BIN := $(BIN_DIR)/jobctl
SERVER_BIN := $(BIN_DIR)/jobworker-server

# Rebuild binaries whenever any Go source changes
GO_FILES := $(shell find cmd internal proto -name '*.go' -type f)


# Packages (explicit so you don't accidentally build ./... into many mains)
JOBCTL_PKG := ./cmd/jobctl
SERVER_PKG := ./cmd/jobworker-server

# ---- Default: build everything you need to run locally ----

## NOTE: add chroot to enable building of chroot jail when needed
all: proto certs build
	@echo ">>> all complete"

# ---- Help ----
help:
	@echo "Default: make                        -> proto + certs + chroot + build binaries"
	@echo "Targets:"
	@echo "  help                               - show this help"
	@echo "  all                                - proto + certs + chroot + build"
	@echo ""
	@echo "Go build/test:"
	@echo "  deps                               - download deps"
	@echo "  tidy                               - go mod tidy"
	@echo "  fmt                                - gofmt ./..."
	@echo "  vet                                - go vet ./..."
	@echo "  test                               - go test ./..."
	@echo "  build                              - build both binaries into $(BIN_DIR)/"
	@echo "  jobctl                             - build jobctl only"
	@echo "  jobworker-server                   - build server only"
	@echo "  install                            - go install both binaries"
	@echo ""
	@echo "Proto:"
	@echo "  proto                              - generate protobufs"
	@echo "  proto.clean                        - clean generated protobufs"
	@echo ""
	@echo "Certs:"
	@echo "  certs                              - run 'make all' in certs/"
	@echo "  make certs server                  - run 'make server' in certs/"
	@echo "  make certs user                    - run 'make user' in certs/"
	@echo "  certs.clean                        - run 'make clean' in certs/"
	@echo ""
	@echo "Chroot:"
	@echo "  chroot                             - build minimal chroot (calls '$(CHROOT_SCRIPT) build')"
	@echo "  chroot.clean                       - unmount & remove chroot (calls '$(CHROOT_SCRIPT) clean')"
	@echo "  chroot.nuke                        - force cleanup (calls '$(CHROOT_SCRIPT) nuke')"
	@echo ""
	@echo "Run:"
	@echo "  run.server                         - run server (as root)"

# ---- Proto ----
proto:
	@./scripts/generate_proto.sh

proto.clean:
	@./scripts/generate_proto.sh clean

# ---- Certs namespace ----
# Allows: make certs          (defaults to certs/all)
#         make certs server   (runs certs/server)
#         make certs user     (runs certs/user)
certs:
	@$(MAKE) -C certs $(if $(filter-out $@,$(MAKECMDGOALS)),$(filter-out $@,$(MAKECMDGOALS)),all)

server user:
	@:

certs.clean:
	@$(MAKE) -C certs clean

# ---- Chroot lifecycle (no coupling to global clean) ----
chroot:
	@$(CHROOT_SCRIPT) build

chroot.clean:
	@$(CHROOT_SCRIPT) clean

chroot.nuke:
	@$(CHROOT_SCRIPT) nuke

# ---- Go deps & hygiene ----
deps:
	@$(GO) mod download

tidy:
	@$(GO) mod tidy

fmt:
	@$(GO) fmt ./...

vet:
	@$(GO) vet ./...

test:
	@$(GO) test ./...

# ---- Build binaries ----
build: build.bin

build.bin: $(JOBCTL_BIN) $(SERVER_BIN)
	@echo ">>> built: $(JOBCTL_BIN) $(SERVER_BIN)"

$(BIN_DIR):
	@mkdir -p $(BIN_DIR)

jobctl: $(JOBCTL_BIN)
jobworker-server: $(SERVER_BIN)

$(JOBCTL_BIN): $(GO_FILES) | $(BIN_DIR)
	@echo ">>> building jobctl -> $(JOBCTL_BIN)"
	@$(GO) build -o $(JOBCTL_BIN) $(JOBCTL_PKG)

$(SERVER_BIN): $(GO_FILES) | $(BIN_DIR)
	@echo ">>> building jobworker-server -> $(SERVER_BIN)"
	@$(GO) build -o $(SERVER_BIN) $(SERVER_PKG)

# Optional: install into GOPATH/bin
install:
	@echo ">>> go install jobctl and jobworker-server"
	@$(GO) install $(JOBCTL_PKG)
	@$(GO) install $(SERVER_PKG)

# ---- Run server ----
run.server: build
	@sudo fuser -k 50051/tcp >/dev/null 2>&1 || true
	@sudo $(SERVER_BIN) -listen :50051 -certs ./certs -log $(LOG_PATH)

# ---- Global clean (does NOT touch chroot) ----
clean: proto.clean certs.clean
	@rm -rf $(BIN_DIR)
	@echo ">>> clean complete (kept chroot)"

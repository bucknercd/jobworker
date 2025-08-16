.PHONY: all help proto proto.clean certs certs.clean server user clean \
        chroot chroot.clean chroot.nuke
.DEFAULT_GOAL := all

CHROOT_SCRIPT := ./scripts/chroot.sh

# ---- Default (build everything except chroot cleanup) ----
all: proto certs chroot

# ---- Help ----
help:
	@echo "Default: make                 -> proto + certs + chroot (build)"
	@echo "Targets:"
	@echo "  help                        - show this help"
	@echo "  all                         - same as 'make' (proto + certs + chroot build)"
	@echo "  proto                       - generate protobufs"
	@echo "  proto.clean                 - clean generated protobufs"
	@echo "  certs                       - run 'make all' in certs/"
	@echo "  certs server                - run 'make server' in certs/"
	@echo "  certs user                  - run 'make user' in certs/"
	@echo "  certs clean                 - run 'make clean' in certs/"
	@echo "  chroot                      - build minimal chroot (calls '$(CHROOT_SCRIPT) build')"
	@echo "  chroot.clean                - unmount & remove chroot (calls '$(CHROOT_SCRIPT) clean')"
	@echo "  chroot.nuke                 - force cleanup (calls '$(CHROOT_SCRIPT) nuke')"
	@echo "  clean                       - clean proto + certs (does NOT touch chroot)"

# ---- Proto ----
proto:
	@./scripts/generate_proto.sh

proto.clean:
	@./scripts/generate_proto.sh clean

# ---- Certs namespace ----
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

# ---- Global clean (does NOT touch chroot) ----
clean: proto.clean certs.clean
	@echo ">>> clean complete"

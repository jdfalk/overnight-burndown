# Makefile — overnight-burndown
#
# Conventions:
#   * `make` (no args) is `make build`
#   * `make ci` runs everything CI runs, in CI order. Pre-PR sanity check.
#   * `make install-launchd` / `make uninstall-launchd` manage the LaunchAgent.
#   * `make pause` / `make resume` toggle the kill switch without touching launchd.

BIN          := bin/burndown
PKG          := ./...
LAUNCHD_NAME := com.jdfalk.burndown
LAUNCHD_PATH := $(HOME)/Library/LaunchAgents/$(LAUNCHD_NAME).plist
PLIST_SRC    := launchd/$(LAUNCHD_NAME).plist
BURNDOWN_DIR := $(HOME)/.burndown
PAUSE_FILE   := $(BURNDOWN_DIR)/PAUSE

GO           := go
GOFLAGS      ?=

.PHONY: all build test vet staticcheck ci clean \
        install-launchd uninstall-launchd \
        pause resume status \
        run dry-run \
        help

all: build

build:
	@mkdir -p bin
	$(GO) build $(GOFLAGS) -o $(BIN) ./cmd/burndown
	@echo "built $(BIN)"

test:
	$(GO) test $(GOFLAGS) -race -count=1 $(PKG)

vet:
	$(GO) vet $(PKG)

staticcheck:
	@command -v staticcheck >/dev/null || { \
	  echo "staticcheck not installed; run: go install honnef.co/go/tools/cmd/staticcheck@latest"; \
	  exit 1; }
	staticcheck $(PKG)

ci: vet staticcheck test build

clean:
	rm -rf bin

# ---- launchd lifecycle ----

install-launchd: build
	@mkdir -p $(HOME)/Library/LaunchAgents
	cp $(PLIST_SRC) $(LAUNCHD_PATH)
	@launchctl unload $(LAUNCHD_PATH) 2>/dev/null || true
	launchctl load $(LAUNCHD_PATH)
	@echo "loaded $(LAUNCHD_NAME) — runs nightly at 23:00"

uninstall-launchd:
	@launchctl unload $(LAUNCHD_PATH) 2>/dev/null || true
	@rm -f $(LAUNCHD_PATH)
	@echo "uninstalled $(LAUNCHD_NAME)"

# ---- runtime kill switch ----

pause:
	@mkdir -p $(BURNDOWN_DIR)
	@touch $(PAUSE_FILE)
	@echo "paused — burndown will skip the next scheduled run"
	@echo "(remove $(PAUSE_FILE) or 'make resume' to clear)"

resume:
	@rm -f $(PAUSE_FILE)
	@echo "resumed"

status:
	@printf "binary:        "; ls -l $(BIN) 2>/dev/null || echo "not built"
	@printf "launchd plist: "; ls -l $(LAUNCHD_PATH) 2>/dev/null || echo "not installed"
	@printf "pause flag:    "; ls -l $(PAUSE_FILE) 2>/dev/null || echo "(none — running)"
	@printf "burndown dir:  "; ls -ld $(BURNDOWN_DIR) 2>/dev/null || echo "(none — first run will create)"

# ---- run targets (one nightly cycle) ----

# `make run` invokes the same entrypoint launchd does. Useful for
# kicking off an off-schedule run by hand.
run: build
	$(BIN) run

# `make dry-run` overrides every repo's mode in the loaded config to
# dry-run, then runs the cycle. Use this on night 1 of staged rollout.
dry-run: build
	$(BIN) run --dry-run

help:
	@echo "Targets:"
	@echo "  build             Build $(BIN)"
	@echo "  test              go test -race -count=1"
	@echo "  vet               go vet"
	@echo "  staticcheck       staticcheck (must be installed)"
	@echo "  ci                vet + staticcheck + test + build"
	@echo "  clean             remove bin/"
	@echo "  install-launchd   copy plist + launchctl load (~/Library/LaunchAgents)"
	@echo "  uninstall-launchd unload + remove plist"
	@echo "  pause             touch ~/.burndown/PAUSE — skip next run"
	@echo "  resume            remove the PAUSE file"
	@echo "  status            show binary, plist, pause flag state"

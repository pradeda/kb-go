BUILD_DIR = build
BINARY   = $(BUILD_DIR)/kb
INSTALL  = /usr/local/bin/kb
RUNTIME  = runtime
CONFIG   = config
BUILD    = go build -tags fts5 -o $(BINARY) .
TEST     = go test -tags sqlite_fts5 ./...
PYTEST   = PYTHONPATH=$(RUNTIME) /usr/bin/python3 -m unittest discover -s tests -p 'test_*.py' -v

# Deploy manifest — the SINGLE source for `install`, `verify-installed` and
# `check-manifest`. Format: <repo source>:<deployed destination>:<mode>.
# Never add an artifact to the install recipe without adding it here: that is
# exactly how runtime/watcher.sh stayed tracked, referenced by both systemd
# units and the README, and never actually installed (2026-09-21).
DEPLOY_MANIFEST = \
	runtime/compile.py:/opt/kb/compile.py:755 \
	runtime/supersede_index.py:/opt/kb/supersede_index.py:644 \
	config/secret_patterns.json:/opt/kb/secret_patterns.json:644 \
	runtime/embed_daemon.py:/opt/kb/embed_daemon.py:644 \
	runtime/refresh_volatile.sh:/opt/kb/refresh_volatile.sh:755 \
	runtime/watcher.sh:/opt/kb/watcher.sh:755 \
	runtime/kb_atlas.py:/opt/kb/kb_atlas.py:755 \
	runtime/atlas_template.html:/opt/kb/atlas_template.html:644

# Runtime files that are deliberately NOT deployed because they run from the
# repo (see the README provisioning section). Everything else under runtime/
# must appear in DEPLOY_MANIFEST or check-manifest fails.
DEPLOY_EXEMPT = runtime/provision_storage.py

build:
	mkdir -p $(BUILD_DIR)
	$(BUILD)

test:
	$(TEST)
	$(PYTEST)
	./tests/test_refresh_volatile.sh
	$(MAKE) check-manifest

# Hermetic: no /opt/kb, no host state. Fails when a runtime file would silently
# never be deployed, which is the drift a plain `make test` cannot see.
check-manifest:
	@fail=0; total=0; \
	for entry in $(DEPLOY_MANIFEST); do total=$$((total + 1)); done; \
	for f in $$(find $(RUNTIME) -maxdepth 1 -type f | sort); do \
		case " $(DEPLOY_EXEMPT) " in *" $$f "*) continue;; esac; \
		case " $(DEPLOY_MANIFEST) " in *" $$f:"*) ;; \
		*) echo "NOT DEPLOYED $$f — add it to DEPLOY_MANIFEST or DEPLOY_EXEMPT"; fail=1;; esac; \
	done; \
	[ $$fail -eq 0 ] || exit 1; \
	echo "check-manifest: $$total artifacts declared, every runtime/ file deployed or exempt"

install: build
	sudo install -m 0755 $(BINARY) $(INSTALL)
	@for entry in $(DEPLOY_MANIFEST); do \
		src=$${entry%%:*}; rest=$${entry#*:}; dst=$${rest%%:*}; mode=$${rest##*:}; \
		sudo install -m $$mode "$$src" "$$dst" || exit 1; \
		echo "installed $$src -> $$dst ($$mode)"; \
	done
	$(MAKE) verify-installed

# Host-specific: compares every manifest artifact with what is actually
# installed on this machine. Kept out of `make test` on purpose — a unit test
# must not depend on /opt/kb existing (developers, CI).
verify-installed:
	@fail=0; total=0; \
	for entry in $(DEPLOY_MANIFEST); do \
		total=$$((total + 1)); \
		src=$${entry%%:*}; rest=$${entry#*:}; dst=$${rest%%:*}; mode=$${rest##*:}; \
		if [ ! -f "$$dst" ]; then echo "MISSING $$dst (from $$src) — run make install"; fail=1; continue; fi; \
		if [ ! -r "$$dst" ]; then echo "UNREADABLE $$dst (cannot hash as $$(id -un)) — fix ownership/mode"; fail=1; continue; fi; \
		a=$$(sha256sum "$$src" | cut -d' ' -f1); b=$$(sha256sum "$$dst" | cut -d' ' -f1); \
		if [ "$$a" != "$$b" ]; then \
			echo "DRIFT $$dst: repo $$a != installed $$b — run make install, or mirror your /opt/kb edit into $$src"; fail=1; fi; \
		actual=$$(stat -c %a "$$dst"); \
		if [ "$$actual" != "$$mode" ]; then \
			echo "MODE $$dst is $$actual, manifest declares $$mode — run make install"; fail=1; fi; \
	done; \
	[ $$fail -eq 0 ] || exit 1; \
	echo "verify-installed: $$total/$$total artifacts match (sha256 + mode)"

.PHONY: build test check-manifest install verify-installed

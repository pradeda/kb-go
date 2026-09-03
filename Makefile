BUILD_DIR = build
BINARY   = $(BUILD_DIR)/kb
INSTALL  = /usr/local/bin/kb
COMPILE  = /opt/kb/compile.py
PATTERNS = /opt/kb/secret_patterns.json
DAEMON   = /opt/kb/embed_daemon.py
REFRESH  = /opt/kb/refresh_volatile.sh
RUNTIME  = runtime
CONFIG   = config
BUILD    = go build -tags fts5 -o $(BINARY) .
TEST     = go test -tags sqlite_fts5 ./...
PYTEST   = PYTHONPATH=$(RUNTIME) /usr/bin/python3 -m unittest discover -s tests -p 'test_*.py' -v

build:
	mkdir -p $(BUILD_DIR)
	$(BUILD)

test:
	$(TEST)
	$(PYTEST)
	./tests/test_refresh_volatile.sh

install: build
	sudo cp $(BINARY) $(INSTALL)
	sudo cp $(RUNTIME)/compile.py $(COMPILE)
	sudo cp $(CONFIG)/secret_patterns.json $(PATTERNS)
	sudo install -m 0644 $(RUNTIME)/embed_daemon.py $(DAEMON)
	sudo install -m 0755 $(RUNTIME)/refresh_volatile.sh $(REFRESH)

.PHONY: build test install

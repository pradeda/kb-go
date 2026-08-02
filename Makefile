BINARY   = kb
INSTALL  = /usr/local/bin/kb
COMPILE  = /opt/kb/compile.py
PATTERNS = /opt/kb/secret_patterns.json
DAEMON   = /opt/kb/embed_daemon.py
BUILD    = go build -tags fts5 -o $(BINARY) .
TEST     = go test -tags sqlite_fts5 ./...
PYTEST   = /usr/bin/python3 -m unittest -v test_compile.py test_provision_storage.py test_embed_daemon.py

build:
	$(BUILD)

test:
	$(TEST)
	$(PYTEST)

install: build
	sudo cp $(BINARY) $(INSTALL)
	sudo cp compile.py $(COMPILE)
	sudo cp secret_patterns.json $(PATTERNS)
	sudo install -m 0644 embed_daemon.py $(DAEMON)

.PHONY: build test install

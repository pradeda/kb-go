BINARY   = kb
INSTALL  = /usr/local/bin/kb
COMPILE  = /opt/kb/compile.py
PATTERNS = /opt/kb/secret_patterns.json
BUILD    = go build -tags fts5 -o $(BINARY) .
TEST     = go test -tags sqlite_fts5 ./...
PYTEST   = /usr/bin/python3 -m unittest -v test_compile.py test_provision_storage.py

build:
	$(BUILD)

test:
	$(TEST)
	$(PYTEST)

install: build
	sudo cp $(BINARY) $(INSTALL)
	sudo cp compile.py $(COMPILE)
	sudo cp secret_patterns.json $(PATTERNS)

.PHONY: build test install

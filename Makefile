BINARY   = kb
INSTALL  = /usr/local/bin/kb
COMPILE  = /opt/kb/compile.py
PATTERNS = /opt/kb/secret_patterns.json
BUILD    = go build -tags fts5 -o $(BINARY) .
TEST     = go test -tags sqlite_fts5 ./...

build:
	$(BUILD)

test:
	$(TEST)

install: build
	sudo cp $(BINARY) $(INSTALL)
	sudo cp compile.py $(COMPILE)
	sudo cp secret_patterns.json $(PATTERNS)

.PHONY: build test install

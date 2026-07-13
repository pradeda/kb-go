BINARY   = kb
INSTALL  = /usr/local/bin/kb
COMPILE  = /opt/kb/compile.py
PATTERNS = /opt/kb/secret_patterns.json
BUILD    = go build -tags fts5 -o $(BINARY) .

build:
	$(BUILD)

install: build
	sudo cp $(BINARY) $(INSTALL)
	sudo cp compile.py $(COMPILE)
	sudo cp secret_patterns.json $(PATTERNS)

.PHONY: build install

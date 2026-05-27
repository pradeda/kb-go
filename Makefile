BINARY   = kb
INSTALL  = /usr/local/bin/kb
BUILD    = go build -tags fts5 -o $(BINARY) .

build:
	$(BUILD)

install: build
	sudo cp $(BINARY) $(INSTALL)

.PHONY: build install

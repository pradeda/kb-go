BINARY   = kb-go
INSTALL  = /usr/local/bin/kb-ask
BUILD    = go build -tags fts5 -o $(BINARY) .

build:
	$(BUILD)

install: build
	sudo cp $(BINARY) $(INSTALL)

.PHONY: build install

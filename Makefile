BINARY   = kb
INSTALL  = /usr/local/bin/kb
COMPILE  = /opt/kb/compile.py
BUILD    = go build -tags fts5 -o $(BINARY) .

build:
	$(BUILD)

install: build
	sudo cp $(BINARY) $(INSTALL)
	sudo cp compile.py $(COMPILE)

.PHONY: build install

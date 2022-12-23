
TARGET=bin/logwatcher
MAIN_PKG=main.go

.PHONY: build
build:
	go build $(LDFLAGS) -o $(TARGET) $(MAIN_PKG)

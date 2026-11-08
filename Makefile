PREFIX ?= $(HOME)/.local

.PHONY: build install linux

build:
	go build -o rex-activity .

install:
	go build -o $(PREFIX)/bin/rex-activity .

# For running it on a Linux Rex host.
linux:
	GOOS=linux GOARCH=amd64 go build -o rex-activity-linux-amd64 .
	GOOS=linux GOARCH=arm64 go build -o rex-activity-linux-arm64 .

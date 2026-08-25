GO ?= go
BINARY_NAME ?= icloud-cli
BUILD_DIR ?= bin
COMMAND_PATH ?= ./cmd

GOBIN ?= $(shell $(GO) env GOBIN)
ifeq ($(strip $(GOBIN)),)
GOBIN := $(shell $(GO) env GOPATH)/bin
endif

.DEFAULT_GOAL := build

.PHONY: build run deps install

build:
	@mkdir -p "$(BUILD_DIR)"
	$(GO) build -o "$(BUILD_DIR)/$(BINARY_NAME)" $(COMMAND_PATH)

run:
	$(GO) run $(COMMAND_PATH) $(ARGS)

deps:
	$(GO) mod download

install:
	@mkdir -p "$(GOBIN)"
	$(GO) build -o "$(GOBIN)/$(BINARY_NAME)" $(COMMAND_PATH)

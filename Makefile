# Make does not offer a recursive wildcard function, so here's one:
rwildcard=$(wildcard $1$2) $(foreach d,$(wildcard $1*),$(call rwildcard,$d/,$2))

SHELL := /bin/bash
PROJECT := github.com/jenkins-x/lighthouse-jx-controller
JXCONTROLLER_EXECUTABLE := lighthouse-jx-controller
DOCKER_REGISTRY := jenkinsxio
JXCONTROLLER_MAIN_SRC_FILE=cmd/jxcontroller/main.go
GO := GO111MODULE=on go
GO_NOMOD := GO111MODULE=off go
VERSION ?= $(shell echo "$$(git describe --abbrev=0 --tags 2>/dev/null)-dev+$(REV)" | sed 's/^v//')
GO_LDFLAGS :=  -X $(PROJECT)/pkg/version.Version='$(VERSION)'
GO_DEPENDENCIES := $(call rwildcard,pkg/,*.go) $(call rwildcard,cmd/,*.go)

GOTEST := $(GO) test

CLIENTSET_GENERATOR_VERSION := kubernetes-1.15.12

all: check test build

.PHONY: test
test: 
	CGO_ENABLED=$(CGO_ENABLED) $(GOTEST) -short ./pkg/... ./cmd/...

.PHONY: check
check: fmt lint sec

get-fmt-deps: ## Install test dependencies
	$(GO_NOMOD) get golang.org/x/tools/cmd/goimports

.PHONY: importfmt
importfmt: get-fmt-deps
	@echo "Formatting the imports..."
	goimports -w $(GO_DEPENDENCIES)

.PHONY: fmt
fmt: importfmt
	@echo "FORMATTING"
	@FORMATTED=`$(GO) fmt ./...`
	@([[ ! -z "$(FORMATTED)" ]] && printf "Fixed unformatted files:\n$(FORMATTED)") || true

GOLINT := $(GOPATH)/bin/golint
$(GOLINT):
	$(GO_NOMOD) get -u golang.org/x/lint/golint

.PHONY: lint
lint: $(GOLINT)
	@echo "VETTING"
	$(GO) vet ./...
	@echo "LINTING"
	$(GOLINT) -set_exit_status ./...

GOSEC := $(GOPATH)/bin/gosec
$(GOSEC):
	$(GO_NOMOD) get -u github.com/securego/gosec/cmd/gosec

.PHONY: sec
sec: $(GOSEC)
	@echo "SECURITY SCANNING"
	$(GOSEC) -fmt=csv ./...

.PHONY: clean
clean:
	rm -rf bin build release

.PHONY: build
build: jx-controller

.PHONY: jx-controller
jx-controller:
	$(GO) build -i -ldflags "$(GO_LDFLAGS)" -o bin/$(JXCONTROLLER_EXECUTABLE) $(JXCONTROLLER_MAIN_SRC_FILE)

.PHONY: mod
mod: build
	echo "tidying the go module"
	$(GO) mod tidy

.PHONY: build-linux
build-linux: build-jx-controller-linux

.PHONY: build-jx-controller-linux
build-jx-controller-linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -ldflags "$(GO_LDFLAGS)" -o bin/$(JXCONTROLLER_EXECUTABLE) $(JXCONTROLLER_MAIN_SRC_FILE)

.PHONY: container
container: 
	docker-compose build $(DOCKER_IMAGE_NAME)

.PHONY: production-container
production-container:
	docker build --rm -t $(DOCKER_REGISTRY)/$(DOCKER_IMAGE_NAME) .

.PHONY: push-container
push-container: production-container
	docker push $(DOCKER_REGISTRY)/$(DOCKER_IMAGE_NAME)


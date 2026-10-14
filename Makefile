# claude-rollet: proxy que sorteia o provider de cada sessão do Claude Code, às cegas
#
# `make` ou `make help` lista os alvos. Este Makefile é uma fachada: cada alvo
# delega pro go. Regra nova entra com `## descrição` na mesma linha pra
# aparecer no help.

SHELL := bash
.SHELLFLAGS := -eu -o pipefail -c
MAKEFLAGS += --warn-undefined-variables --no-builtin-rules --no-print-directory
.DEFAULT_GOAL := help
.DELETE_ON_ERROR:

# ---------------------------------------------------------------------------
# Variáveis (?= permite sobrescrever: `make test PKG=./internal/proxy/...`)
# ---------------------------------------------------------------------------

APP     ?= rollet
BIN_DIR ?= bin
PKG     ?= ./...
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

# Cores só quando stdout é um terminal (pipe e CI ficam limpos)
BOLD  :=
CYAN  :=
GREEN :=
RESET :=
ifneq ($(shell [ -t 1 ] && echo tty),)
  BOLD  := $(shell tput bold 2>/dev/null)
  CYAN  := $(shell tput setaf 6 2>/dev/null)
  GREEN := $(shell tput setaf 2 2>/dev/null)
  RESET := $(shell tput sgr0 2>/dev/null)
endif

##@ Geral

.PHONY: help
help: ## Lista os alvos disponíveis
	@awk 'BEGIN { FS = ":.*##"; printf "\n$(BOLD)$(APP)$(RESET)\n\nUso: make $(CYAN)<alvo>$(RESET)\n" } \
	  /^##@/ { printf "\n$(BOLD)%s$(RESET)\n", substr($$0, 5) } \
	  /^[a-zA-Z0-9_.\/-]+:.*?##/ { printf "  $(CYAN)%-18s$(RESET) %s\n", $$1, $$2 } \
	  END { printf "\n" }' $(MAKEFILE_LIST)

.PHONY: setup
setup: ## Baixa as dependências Go
	@echo "$(GREEN)▸ setup$(RESET)"
	go mod download

##@ Desenvolvimento

.PHONY: run
run: build ## Sobe o proxy em foreground com o binário construído
	@echo "$(GREEN)▸ run$(RESET)"
	./$(BIN_DIR)/$(APP) serve

##@ Qualidade

.PHONY: fmt
fmt: ## Formata o código (modifica arquivos)
	@echo "$(GREEN)▸ fmt$(RESET)"
	gofmt -w .

.PHONY: lint
lint: ## gofmt (sem modificar) e go vet
	@echo "$(GREEN)▸ lint$(RESET)"
	test -z "$$(gofmt -l .)" || { gofmt -l .; echo "rode: make fmt"; exit 1; }
	go vet $(PKG)

.PHONY: test
test: ## Roda os testes com race detector
	@echo "$(GREEN)▸ test$(RESET)"
	go test -race $(PKG)

.PHONY: check
check: lint test build ## Tudo que o CI roda: lint, test, build
	@echo "$(GREEN)✓ check ok$(RESET)"

##@ Build

.PHONY: build
build: ## Compila para BIN_DIR (padrão bin/)
	@echo "$(GREEN)▸ build$(RESET)"
	@mkdir -p $(BIN_DIR)
	go build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(APP) ./cmd/rollet

.PHONY: install
install: ## Instala o rollet no GOBIN (go install)
	@echo "$(GREEN)▸ install$(RESET)"
	go install -ldflags "$(LDFLAGS)" ./cmd/rollet

.PHONY: clean
clean: ## Remove artefatos gerados
	@echo "$(GREEN)▸ clean$(RESET)"
	rm -rf $(BIN_DIR)

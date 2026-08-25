.DEFAULT_GOAL := help

GO ?= go
BIN_DIR := bin
BINARY := $(BIN_DIR)/unraid-vsock-sensors
PLUGIN_BINARY := $(BIN_DIR)/unraid-vsock-sensors-cc
VERSION ?= $(shell ./version.sh)
LDFLAGS = -s -w -X main.version=$(VERSION)

.PHONY: help

help: ## Affiche les commandes disponibles
	@awk 'BEGIN { FS = ":.*## "; print "Commandes disponibles :" } \
		/^[a-zA-Z0-9_-]+:.*## / { printf "  make %-16s %s\n", $$1, $$2 }' \
		$(MAKEFILE_LIST)

.PHONY: fmt tidy vet test check all

fmt: ## Formate tous les fichiers Go
	$(GO) fmt ./...

tidy: ## Synchronise les dépendances Go
	$(GO) mod tidy

vet: ## Recherche les erreurs Go courantes
	$(GO) vet ./...

test: ## Exécute tous les tests, y compris ceux du plugin
	$(GO) test ./...

check: vet test ## Vérifie le projet sans créer d'artefacts

all: check build plugin-package ## Vérifie, compile et crée le package du plugin

.PHONY: build

build: ## Compile un binaire Linux statique
	CGO_ENABLED=0 GOOS=linux $(GO) build -trimpath -ldflags="$(LDFLAGS)" -o $(BINARY) .

.PHONY: plugin-build plugin-test plugin-generate plugin-package unraid-package

plugin-build: ## Compile le plugin CoolerControl pour Linux
	CGO_ENABLED=0 GOOS=linux $(GO) build -trimpath -ldflags="$(LDFLAGS)" -o $(PLUGIN_BINARY) ./coolercontrol-plugin

plugin-test: ## Teste le plugin CoolerControl
	$(GO) test ./coolercontrol-plugin/...

plugin-generate: ## Régénère les fichiers Go depuis le protocole CoolerControl
	cd coolercontrol-plugin && ./generate.sh

plugin-package: ## Crée une archive du plugin installable sans Go ni Git
	GO="$(GO)" VERSION="$(VERSION)" ./coolercontrol-plugin/package.sh

unraid-package: ## Crée le plugin serveur installable dans Unraid
	GO="$(GO)" VERSION="$(VERSION)" ./unraid-plugin/package.sh

build plugin-build: | $(BIN_DIR)

$(BIN_DIR):
	mkdir -p $@

.PHONY: clean

clean: ## Supprime tous les fichiers générés
	$(RM) -r $(BIN_DIR) dist

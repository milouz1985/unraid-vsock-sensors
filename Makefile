.DEFAULT_GOAL := help

GO ?= go
BIN_DIR := bin
BINARY := $(BIN_DIR)/unraid-vsock-sensors
VERSION ?= $(shell ./version.sh)
# Freeze the inferred version before packaging modifies generated tracked files.
VERSION := $(VERSION)
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

test: ## Exécute tous les tests Go
	$(GO) test ./...

check: vet test ## Vérifie le projet sans créer d'artefacts

all: check build unraid-package hwmon-package ## Vérifie, compile et crée tous les paquets

.PHONY: build

build: ## Compile un binaire Linux statique
	CGO_ENABLED=0 GOOS=linux $(GO) build -trimpath -ldflags="$(LDFLAGS)" -o $(BINARY) .

.PHONY: unraid-package hwmon-package

unraid-package: ## Crée le plugin serveur installable dans Unraid
	GO="$(GO)" VERSION="$(VERSION)" ./unraid-plugin/package.sh

hwmon-package: ## Crée le paquet hwmon installable sur Proxmox sans Go
	GO="$(GO)" VERSION="$(VERSION)" ./virt-temp/package.sh

build: | $(BIN_DIR)

$(BIN_DIR):
	mkdir -p $@

.PHONY: clean

clean: ## Supprime tous les fichiers générés
	$(RM) -r $(BIN_DIR) dist

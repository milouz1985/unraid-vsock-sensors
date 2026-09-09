.DEFAULT_GOAL := help

GO ?= go
BIN_DIR := bin
BINARY := $(BIN_DIR)/unraid-vsock-sensors
VERSION ?= $(shell ./version.sh)
DEBIAN_REVISION ?= 1

# Freeze the inferred version before packaging modifies generated tracked files.
VERSION := $(VERSION)

LDFLAGS = -s -w -X main.version=$(VERSION)

BASH_SCRIPTS := version.sh \
	unraid-plugin/package.sh \
	unraid-plugin/rc.unraid-vsock-sensors \
	unraid-plugin/rc_test.sh \
	unraid-plugin/service.sh \
	virt-temp/package.sh \
	virt-temp/prepare-dkms.sh \
	tests/vm/build-template.sh \
	tests/vm/common.sh \
	tests/vm/run.sh \
	tests/vm/guest-tests.sh \
	tests/vm/package-tests.sh \
	tests/vm/sync-builder.sh

POSIX_SCRIPTS := virt-temp/debian/postinst.in \
	virt-temp/debian/prerm.in \
	virt-temp/debian/postrm.in

.PHONY: help

help: ## Affiche les commandes disponibles
	@awk 'BEGIN { FS = ":.*## "; print "Commandes disponibles :" } \
		/^[a-zA-Z0-9_-]+:.*## / { printf "  make %-20s %s\n", $$1, $$2 }' \
		$(MAKEFILE_LIST)


.PHONY: fmt tidy vet test check-scripts check all

fmt: ## Formate tous les fichiers Go
	$(GO) fmt ./...

tidy: ## Synchronise les dépendances Go
	$(GO) mod tidy

vet: ## Recherche les erreurs Go courantes
	$(GO) vet ./...

test: ## Exécute tous les tests Go
	$(GO) test ./...


.PHONY: vm-template-sync vm-template test-vm test-vm-core test-vm-package test-vm-all lint-shell

vm-template-sync: ## Synchronise le builder du template vers Proxmox
	bash tests/vm/sync-builder.sh

vm-template: ## Synchronise puis reconstruit le template Proxmox
	bash tests/vm/sync-builder.sh --rebuild

test-vm: ## Exécute tous les tests dans une VM Proxmox distante
	VM_TEST_SUITE=all bash tests/vm/run.sh

test-vm-core: ## Teste le module, hwmon et SMART sous noyau PVE
	VM_TEST_SUITE=core bash tests/vm/run.sh

test-vm-package: ## Teste le cycle complet du paquet Debian et de DKMS
	VM_TEST_SUITE=package bash tests/vm/run.sh

test-vm-all: ## Exécute tous les tests VM, y compris le paquet Debian
	VM_TEST_SUITE=all bash tests/vm/run.sh

lint-shell: ## Analyse les scripts shell avec ShellCheck
	shellcheck $(BASH_SCRIPTS)


check-scripts: ## Vérifie la syntaxe des scripts et de l'interface
	for script in $(BASH_SCRIPTS); do \
		bash -n "$$script" || exit $$?; \
	done
	for script in $(POSIX_SCRIPTS); do \
		sh -n "$$script" || exit $$?; \
	done
	php -l unraid-plugin/UnraidVsockSensors.page >/dev/null
	bash unraid-plugin/rc_test.sh

check: vet test check-scripts ## Vérifie le projet sans créer d'artefacts

all: check build unraid-package hwmon-package ## Vérifie, compile et crée tous les paquets


.PHONY: build

build: | $(BIN_DIR) ## Compile un binaire Linux statique
	CGO_ENABLED=0 GOOS=linux $(GO) build -trimpath -ldflags="$(LDFLAGS)" -o $(BINARY) .


.PHONY: unraid-package hwmon-package

unraid-package: ## Crée le plugin serveur installable dans Unraid
	GO="$(GO)" VERSION="$(VERSION)" ./unraid-plugin/package.sh

hwmon-package: ## Crée le paquet Debian hwmon installable sur Proxmox
	GO="$(GO)" VERSION="$(VERSION)" DEBIAN_REVISION="$(DEBIAN_REVISION)" ./virt-temp/package.sh


$(BIN_DIR):
	mkdir -p $@


.PHONY: clean

clean: ## Supprime tous les fichiers générés
	$(RM) -r $(BIN_DIR) dist
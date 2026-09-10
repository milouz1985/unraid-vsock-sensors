.DEFAULT_GOAL := help

GO ?= go
BIN_DIR := bin
BINARY := $(BIN_DIR)/unraid-vsock-sensors
VERSION ?=
DEBIAN_REVISION ?= 1

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

MODULE_BUILD_ARTIFACTS := virt-temp/module/*.o \
	virt-temp/module/*.ko \
	virt-temp/module/*.mod \
	virt-temp/module/*.mod.c \
	virt-temp/module/.*.cmd \
	virt-temp/module/Module.symvers \
	virt-temp/module/modules.order


# Resolve VERSION only for targets that actually produce versioned artifacts.
#
# If VERSION is provided explicitly, version.sh validates it.
# Otherwise version.sh derives it from Git.
define resolve-version
	version="$(VERSION)"; \
	if [ -n "$$version" ]; then \
		version="$$(VERSION="$$version" ./version.sh)" || exit $$?; \
	else \
		version="$$(./version.sh)" || exit $$?; \
	fi; \
	[ -n "$$version" ] || { \
		echo "Unable to determine version" >&2; \
		exit 1; \
	};
endef


.PHONY: help

help: ## Affiche les commandes disponibles
	@awk 'BEGIN { FS = ":.*## "; print "Commandes disponibles :" } \
		/^[a-zA-Z0-9_-]+:.*## / { printf "  make %-20s %s\n", $$1, $$2 }' \
		$(MAKEFILE_LIST)


.PHONY: fmt tidy vet test test-race check-scripts lint-shell check all

fmt: ## Formate tous les fichiers Go
	$(GO) fmt ./...

tidy: ## Synchronise les dépendances Go
	$(GO) mod tidy

vet: ## Recherche les erreurs Go courantes
	$(GO) vet ./...

test: ## Exécute tous les tests Go
	$(GO) test ./...

test-race: ## Exécute tous les tests Go avec le détecteur de courses
	$(GO) test -race ./...

lint-shell: ## Analyse les scripts shell avec ShellCheck
	shellcheck -x -P SCRIPTDIR $(BASH_SCRIPTS) $(POSIX_SCRIPTS)

check-scripts: ## Vérifie la syntaxe des scripts et de l'interface
	for script in $(BASH_SCRIPTS); do \
		bash -n "$$script" || exit $$?; \
	done
	for script in $(POSIX_SCRIPTS); do \
		sh -n "$$script" || exit $$?; \
	done
	php -l unraid-plugin/UnraidVsockSensors.page >/dev/null
	bash unraid-plugin/rc_test.sh

check: vet test check-scripts lint-shell ## Vérifie le projet sans créer d'artefacts

all: check test-race build unraid-package hwmon-package ## Vérifie, compile et crée tous les paquets


.PHONY: vm-template-sync vm-template-rebuild test-vm test-vm-core test-vm-package

vm-template-sync: ## Synchronise le builder du template vers Proxmox
	bash tests/vm/sync-builder.sh

vm-template-rebuild: ## Synchronise puis reconstruit le template Proxmox
	bash tests/vm/sync-builder.sh --rebuild

test-vm: ## Exécute tous les tests dans une VM Proxmox distante
	VM_TEST_SUITE=all bash tests/vm/run.sh

test-vm-core: ## Teste le module, hwmon et SMART sous noyau PVE
	VM_TEST_SUITE=core bash tests/vm/run.sh

test-vm-package: ## Teste le cycle complet du paquet Debian et de DKMS
	VM_TEST_SUITE=package bash tests/vm/run.sh


.PHONY: build

build: | $(BIN_DIR) ## Compile un binaire Linux statique
	@$(resolve-version) \
	CGO_ENABLED=0 GOOS=linux $(GO) build -trimpath \
		-ldflags="-s -w -X main.version=$$version" \
		-o $(BINARY) .


.PHONY: unraid-package hwmon-package

unraid-package: ## Crée le plugin serveur installable dans Unraid
	@$(resolve-version) \
	GO="$(GO)" VERSION="$$version" ./unraid-plugin/package.sh

hwmon-package: ## Crée le paquet Debian hwmon installable sur Proxmox
	@$(resolve-version) \
	GO="$(GO)" VERSION="$$version" DEBIAN_REVISION="$(DEBIAN_REVISION)" \
		./virt-temp/package.sh


$(BIN_DIR):
	mkdir -p $@


.PHONY: clean

clean: ## Supprime tous les fichiers générés
	$(RM) -r $(BIN_DIR) dist
	$(RM) $(MODULE_BUILD_ARTIFACTS)
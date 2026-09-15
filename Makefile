.DEFAULT_GOAL := help

GO ?= go
GOFMT ?= gofmt
BIN_DIR := bin
BINARY := $(BIN_DIR)/unraid-vsock-sensors
VERSION ?=
DEBIAN_REVISION ?= 1
FUZZTIME ?= 30s

MPT3_FUZZ_TARGETS := FuzzParseMPT3PCIAddress \
	FuzzParseMPT3Model \
	FuzzParseMPT3SASAddress \
	FuzzParseMPT3Temperature \
	FuzzValidateMPT3ConfigReply

BASH_SCRIPTS := version.sh \
	version_test.sh \
	unraid-plugin/package.sh \
	unraid-plugin/package_test.sh \
	unraid-plugin/update-plg.sh \
	unraid-plugin/update_plg_test.sh \
	unraid-plugin/poll_attributes \
	unraid-plugin/rc.unraid-vsock-sensors \
	unraid-plugin/rc_test.sh \
	unraid-plugin/service.sh \
	unraid-plugin/service_test.sh \
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


.PHONY: fmt fmt-check tidy tidy-check vet test test-race fuzz-mpt3 check-scripts lint-shell check all

fmt: ## Formate tous les fichiers Go
	$(GOFMT) -w .

fmt-check: ## Vérifie le formatage Go sans modifier les fichiers
	@unformatted="$$($(GOFMT) -l .)" || exit $$?; \
	if [ -n "$$unformatted" ]; then \
		echo "Fichiers Go non formatés :" >&2; \
		printf '%s\n' "$$unformatted" >&2; \
		exit 1; \
	fi

tidy: ## Synchronise les dépendances Go
	$(GO) mod tidy

tidy-check: ## Vérifie go.mod et go.sum sans les modifier
	$(GO) mod tidy -diff

vet: ## Recherche les erreurs Go courantes
	$(GO) vet ./...

test: ## Exécute tous les tests Go
	$(GO) test ./...

test-race: ## Exécute tous les tests Go avec le détecteur de courses
	$(GO) test -race ./...

fuzz-mpt3: ## Lance successivement les campagnes de fuzzing MPT3
	@for target in $(MPT3_FUZZ_TARGETS); do \
		echo "Fuzzing $$target for $(FUZZTIME)"; \
		$(GO) test -run='^$$' -fuzz="^$${target}$$" -fuzztime="$(FUZZTIME)" . || exit $$?; \
	done

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
	php -l unraid-plugin/UnraidVsockSensorsDiagnostics.page >/dev/null
	bash version_test.sh
	bash unraid-plugin/package_test.sh
	bash unraid-plugin/update_plg_test.sh
	bash unraid-plugin/rc_test.sh
	bash unraid-plugin/service_test.sh

check: fmt-check tidy-check vet test check-scripts lint-shell ## Vérifie le projet sans créer d'artefacts

all: check test-race artifacts ## Vérifie, compile et crée tous les paquets


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
	CGO_ENABLED=0 GOOS=linux  GOARCH=amd64 $(GO) build -buildvcs=false -trimpath \
		-ldflags="-s -w -X main.version=$$version" \
		-o $(BINARY) .


.PHONY: unraid-package hwmon-package artifacts update-plg release

unraid-package: ## Crée le paquet txz installable dans Unraid
	@$(resolve-version) \
	GO="$(GO)" VERSION="$$version" ./unraid-plugin/package.sh

hwmon-package: ## Crée le paquet Debian hwmon installable sur Proxmox
	@$(resolve-version) \
	GO="$(GO)" VERSION="$$version" DEBIAN_REVISION="$(DEBIAN_REVISION)" \
		./virt-temp/package.sh

artifacts: build unraid-package hwmon-package ## Produit tous les artefacts versionnés

update-plg: ## Génère le descripteur .plg public depuis le paquet .txz
	@VERSION="$(VERSION)" ./unraid-plugin/update-plg.sh

release: ## Valide et prépare tous les artefacts d'une release
	@if [ -z "$(VERSION)" ]; then \
		echo "VERSION is required (example: make release VERSION=2.0.0)" >&2; \
		exit 1; \
	fi
	@VERSION="$(VERSION)" ./version.sh --release >/dev/null
	@status="$$(git status --porcelain --untracked-files=normal)"; \
	if [ -n "$$status" ]; then \
		echo "A release requires a clean Git worktree:" >&2; \
		printf '%s\n' "$$status" >&2; \
		exit 1; \
	fi
	+$(MAKE) --no-print-directory check
	+$(MAKE) --no-print-directory test-race
	+$(MAKE) --no-print-directory test-vm
	+$(MAKE) --no-print-directory artifacts VERSION="$(VERSION)"
	+$(MAKE) --no-print-directory update-plg VERSION="$(VERSION)"


$(BIN_DIR):
	mkdir -p $@


.PHONY: clean

clean: ## Supprime tous les fichiers générés
	$(RM) -r $(BIN_DIR) dist
	$(RM) $(MODULE_BUILD_ARTIFACTS)

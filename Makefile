.DEFAULT_GOAL := help

GO := go
BINARY := bin/unraid-vsock-sensors
PLUGIN_BINARY := bin/unraid-vsock-sensors-cc

.PHONY: help build test vet fmt tidy check plugin-build plugin-test plugin-generate plugin-package clean

help: ## Affiche les commandes disponibles
	@echo "Commandes disponibles :"
	@echo "  make build  - Compile le binaire statique"
	@echo "  make test   - Exécute les tests"
	@echo "  make vet    - Recherche les erreurs Go courantes"
	@echo "  make fmt    - Formate les fichiers Go"
	@echo "  make tidy   - Met à jour go.mod et go.sum"
	@echo "  make check  - Vérifie et compile le serveur et le plugin"
	@echo "  make plugin-build - Compile le plugin CoolerControl"
	@echo "  make plugin-test  - Teste le plugin CoolerControl"
	@echo "  make plugin-generate - Régénère le protocole Go du plugin"
	@echo "  make plugin-package - Crée l'archive autonome du plugin"
	@echo "  make clean  - Supprime le binaire compilé"

build: ## Compile un binaire Linux statique
	mkdir -p bin
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags="-s -w" -o $(BINARY) .

test: ## Exécute les tests
	$(GO) test ./...

vet: ## Recherche les erreurs Go courantes
	$(GO) vet ./...

fmt: ## Formate le code source
	gofmt -w *.go

tidy: ## Synchronise les dépendances Go
	$(GO) mod tidy

check: vet test build plugin-build ## Vérifie et compile le serveur et le plugin

plugin-build: ## Compile le plugin CoolerControl
	mkdir -p bin
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags="-s -w" -o $(PLUGIN_BINARY) ./coolercontrol-plugin

plugin-test: ## Teste le plugin CoolerControl
	$(GO) test ./coolercontrol-plugin/...

plugin-generate: ## Régénère les fichiers Go depuis le protocole CoolerControl
	cd coolercontrol-plugin && ./generate.sh

plugin-package: ## Crée une archive du plugin installable sans Go ni Git
	./coolercontrol-plugin/package.sh

clean: ## Supprime les fichiers générés
	$(RM) $(BINARY) $(PLUGIN_BINARY)
	$(RM) -r dist

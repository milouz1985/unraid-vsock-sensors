.DEFAULT_GOAL := help

GO := go
BINARY := bin/unraid-fan-control

.PHONY: help build test vet fmt tidy check clean

help: ## Affiche les commandes disponibles
	@echo "Commandes disponibles :"
	@echo "  make build  - Compile le binaire statique"
	@echo "  make test   - Exécute les tests"
	@echo "  make vet    - Recherche les erreurs Go courantes"
	@echo "  make fmt    - Formate les fichiers Go"
	@echo "  make tidy   - Met à jour go.mod et go.sum"
	@echo "  make check  - Exécute vet, les tests et la compilation"
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

check: vet test build ## Vérifie et compile le projet

clean: ## Supprime les fichiers générés
	$(RM) $(BINARY)

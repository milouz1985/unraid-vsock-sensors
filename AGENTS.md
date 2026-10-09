# AGENTS.md

## Rôle de ce document

Ce fichier contient les règles de travail propres aux agents de développement.
Il n'est pas la source de vérité exhaustive du produit ni de ses invariants.

Hiérarchie documentaire :

- `README.md` décrit l'installation, la configuration, l'exploitation, les
  diagnostics, la récupération et la sécurité visibles par l'opérateur ;
- `CONTRIBUTING.md` porte l'architecture, les invariants, les décisions de
  conception et les lifecycles techniques ;
- `virt-temp/README.md` détaille uniquement l'interface, le lifetime et les
  contraintes du module noyau ;
- `tests/vm/README.md` décrit la portée et les procédures des tests
  d'intégration réels ;
- ce fichier indique quoi lire, quelles validations exécuter et comment
  intervenir.

Une information absente d'`AGENTS.md` peut donc rester normative. Avant une
modification non triviale, lire les sections pertinentes des documents
propriétaires.

## Lectures obligatoires par zone

- Disques, emhttpd, SMART, inventaire, politiques ou état thermique :
  `CONTRIBUTING.md`, « Côté Unraid » et « Politique des températures », puis la
  partie collecte de `README.md` si le comportement utilisateur change.
- HBA, MPT3 ou StorCLI : `CONTRIBUTING.md`, « Collecte HBA et commandes
  externes » et « ABI MPT3 et IO Unit Page 7 ».
- Module noyau, configfs, miscdevice ou hwmon kernel : `virt-temp/README.md`
  ainsi que `CONTRIBUTING.md`, « Côté Proxmox » et « Failsafe ».
- Receiver VSOCK, cache ou diagnostics : `CONTRIBUTING.md`, « Côté Proxmox »,
  « Diagnostics » et « Failsafe », puis `README.md` si le comportement
  opérateur change.
- Systemd, Debian ou DKMS : les sections d'exploitation de `README.md`, les
  sections techniques et de packaging de `CONTRIBUTING.md`, puis
  `tests/vm/README.md` pour l'intégration réelle.
- Release ou artefacts : les sections correspondantes de `CONTRIBUTING.md`.

## Portée et simplicité

- Implémenter le plus petit changement qui satisfait le besoin et préserve les
  invariants documentés. Ne pas anticiper de CLI, endpoint, fallback, état,
  persistance, goroutine ou voie parallèle sans scénario concret.
- Préférer des responsabilités et transitions d'état explicites aux
  abstractions génériques. N'introduire une interface que pour de vrais
  backends interchangeables ou une frontière externe utile aux tests.
- Factoriser la duplication conceptuelle, pas une simple ressemblance
  syntaxique. Préserver la direction `composants métier -> internal/sensors`.
- Une fonction à un seul appelant, un type métier, un snapshot ou une copie
  sous mutex peuvent matérialiser une frontière utile ; le nombre de lignes ne
  suffit pas à les déclarer morts.
- Une simplification doit supprimer un état, un chemin, une responsabilité, une
  dépendance ou une duplication conceptuelle, sans modifier le comportement
  d'un refactoring annoncé comme neutre.
- Ne pas découper `hba_mpt3ctl.go` ou `virt-temp/module/virt-temp.c` si cela
  disperse leur contexte d'ABI ou de cycle de vie.
- Quand un mécanisme remplace l'ancien, retirer le chemin devenu inutile dès
  que la compatibilité le permet. Avant de conclure, rechercher les helpers,
  états et abstractions devenus redondants.
- Protéger le travail existant : ne pas nettoyer ni écraser les modifications
  de l'utilisateur et ne toucher qu'aux fichiers nécessaires.

## Conventions

- La version Go de référence est dans `go.mod` ; celle du template VM dans
  `tests/vm/go-version`.
- Formater le Go avec `gofmt` ou `make fmt`. Garder code, commentaires et
  erreurs en anglais, et la documentation utilisateur en français sauf
  convention locale contraire.
- Ajouter `// SPDX-License-Identifier: GPL-3.0-or-later` aux nouveaux fichiers
  Go. Utiliser LF et préserver le dialecte des scripts POSIX shell.
- Donner aux erreurs assez de contexte et respecter les mécanismes existants
  anti-spam et sticky-error.

## Fichiers générés et release

- `bin/`, `dist/` et `unraid-plugin/unraid-vsock-sensors.plg` sont générés. Pour
  le `.plg`, modifier sa source puis utiliser le workflow prévu.
- Ne pas lancer `make release`, créer de tag ou pousser sans demande explicite.
- Suivre `CONTRIBUTING.md` pour l'ordre de release et les remotes.

## Validation

Pour un changement Go ou plugin habituel, exécuter au minimum :

```sh
make check
make test-race
```

Utiliser les tests VM concernés pour les vraies frontières noyau,
configfs/miscdevice, hwmon/sysfs, cache de topologie, DKMS/Debian, systemd ou
block devices :

```sh
make test-vm
make test-vm-core
make test-vm-package
```

Préférer les tests unitaires pour parsers, machines d'état, fixtures sysfs,
StorCLI et erreurs ou timeouts simulés. Pour le parser MPT3, envisager aussi
`make fuzz-mpt3`.

Toujours indiquer les validations réellement exécutées et celles impossibles ;
ne jamais annoncer un succès non observé.

## Git et restitution

- Ne jamais exécuter `git add`, `git commit`, `git tag` ou `git push`. Une
  demande de découpage en commits n'autorise pas leur création.
- Laisser worktree et index en l'état pour la review utilisateur. Présenter les
  fichiers modifiés, le changement, les compromis, les validations et un
  message de commit proposé.
- Un changement de comportement exige des tests ciblés et, si son contrat
  public change, la documentation associée. Préserver la compatibilité lorsque
  l'état matériel est incertain.

## Indépendance des tests

Pour toute modification non triviale du comportement, d'un protocole, d'une
machine d'état, de la concurrence ou d'une frontière système, séparer autant
que possible l'implémentation de la conception des tests.

- Déléguer la conception ou la revue des tests à un sous-agent indépendant
  lorsque les capacités multi-agent sont disponibles.
- Le sous-agent de test doit partir du contrat attendu, des spécifications,
  des interfaces externes et du comportement observable, pas du raisonnement
  ayant conduit à l'implémentation.
- Ne pas demander au sous-agent de simplement confirmer les tests déjà écrits
  par l'agent d'implémentation.
- Rechercher en priorité des oracles indépendants : valeurs littérales issues
  d'un contrat externe, fixtures indépendantes, vrais composants aux frontières,
  propriétés observables et scénarios susceptibles de réfuter l'implémentation.
- Un test doit pouvoir expliquer quel défaut plausible il détecte.
- Distinguer les tests de contrat (comportement attendu) des tests de
  caractérisation (comportement actuel). Le nom et les commentaires d'un test
  de caractérisation doivent rendre cette portée explicite : un comportement
  historique potentiellement défectueux ne devient pas une garantie métier ou
  de sécurité parce qu'un test le décrit.
- Un mock ou un faux système de fichiers ne prouve que les comportements
  effectivement exercés. Les garanties qui nécessitent le vrai noyau,
  configfs, hwmon ou AF_VSOCK relèvent des tests VM appropriés ; la logique
  purement applicative peut rester testée en Go.
- L'absence d'une dépendance ou d'un environnement obligatoire pour un test
  doit provoquer un échec explicite dans la validation habituelle, sans mode
  strict séparé. Les autres skips doivent rester visibles et justifiés.
- Éviter les tests qui reconstruisent l'attendu avec les mêmes helpers,
  constantes, parsers ou algorithmes que le code testé.
- Éviter de modifier le code de production uniquement pour faciliter un test,
  sauf si le changement améliore également l'architecture de production.
- Les tests déjà couverts à une frontière plus forte ne doivent pas être
  dupliqués sans invariant supplémentaire clairement identifié.
- Le sous-agent de test doit chercher activement à falsifier l'implémentation,
  pas à confirmer qu'elle fonctionne ; lorsque c'est possible, lui faire
  proposer les scénarios et les oracles avant de lui présenter les tests
  existants.

Pour les changements triviaux ou purement mécaniques, ne pas créer de
délégation multi-agent sans bénéfice concret.

# AGENTS.md

## Rôle de ce document

Ce fichier contient les règles de travail propres aux agents de développement.
Il n'est pas la source de vérité exhaustive du produit ni de ses invariants.

Hiérarchie documentaire :

- `README.md` décrit le produit, son installation et son comportement visible ;
- `CONTRIBUTING.md` porte les invariants techniques et choix de conception ;
- `virt-temp/README.md` détaille le module noyau et le receiver Proxmox ;
- `tests/vm/README.md` décrit l'intégration VM, noyau et packaging ;
- ce fichier indique quoi lire et comment intervenir.

Une information absente d'`AGENTS.md` peut donc rester normative. Avant une
modification non triviale, lire les sections pertinentes des documents
propriétaires.

## Lectures obligatoires par zone

- Disques, emhttpd, SMART, inventaire, politiques ou état thermique :
  `CONTRIBUTING.md`, « Côté Unraid » et « Politique des températures », puis la
  partie collecte de `README.md` si le comportement utilisateur change.
- HBA, MPT3 ou StorCLI : `CONTRIBUTING.md`, « Collecte HBA et commandes
  externes » et « ABI MPT3 et IO Unit Page 7 ».
- VSOCK, hwmon, configfs, cache ou diagnostics : `CONTRIBUTING.md`, « Côté
  Proxmox », « Diagnostics » et « Failsafe », ainsi que `virt-temp/README.md`.
- Module noyau, DKMS, Debian ou systemd : `virt-temp/README.md` et
  `tests/vm/README.md`.
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

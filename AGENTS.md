# AGENTS.md

## Objectif

`unraid-vsock-sensors` exporte les températures des disques et HBA d'une VM
Unraid vers un hôte Proxmox via `AF_VSOCK`, puis les expose comme capteurs
Linux `hwmon` au moyen du module noyau `virt-temp`.

Garder l'implémentation directe et conservatrice : préférer des responsabilités
et transitions d'état explicites aux abstractions génériques.

Avant une modification non triviale, lire les parties pertinentes de :

- `README.md` pour le comportement et les usages supportés ;
- `CONTRIBUTING.md` pour l'architecture, les tests et la release ;
- `tests/vm/README.md` pour l'intégration VM, noyau ou packaging ;
- `virt-temp/README.md` pour le module noyau et le côté Proxmox.

## Cartographie du dépôt

Package Go principal :

- `main.go` : points d'entrée `serve`, `hwmon` et `version` ;
- `disk*.go` : inventaire Unraid, identité, politiques, sources emhttpd/SMART,
  état thermique et orchestration disque ;
- `control_server.go` : API HTTP locale sur socket Unix pour la WebUI ;
- `hba*.go` : orchestration HBA et backends MPT3/StorCLI ;
- `publisher.go` : publisher VSOCK persistant ;
- `hwmon.go` : receiver Proxmox, orchestration des familles, cache et
  notification des changements de topologie ;
- `hwmon_topology.go` : modèle et projection de la topologie hwmon ;
- `hwmon_device.go` : validation, réconciliation configfs et écritures dans les
  miscdevices par sonde ;
- `hwmon_cache.go` : persistance de la dernière topologie valide, sans
  températures ;
- `diagnostics.go` : snapshot JSON runtime en lecture seule ;
- `internal/sensors` : modèle/protocole partagé et framing VSOCK ;
- `internal/vsockaddr` : validation partagée du CID et du port.

Autres composants :

- `unraid-plugin/` : WebUI, client du control socket, scripts rc et packaging ;
- `virt-temp/module/virt-temp.c` : configfs pour la topologie, un miscdevice par
  sonde pour les températures, et exposition hwmon ;
- `virt-temp/` : packaging Debian/DKMS, unité systemd et documentation ;
- `tests/vm/` : tests réels Proxmox/noyau/systemd/package.

## Invariants architecturaux

### Collecte et état des disques

- En fonctionnement normal, utiliser l'état/cache maintenu par `emhttpd`.
  Le SMART direct est seulement un fallback lorsque le heartbeat
  `poll_attributes` devient stale ; il reste borné en temps et en concurrence.
- Ne jamais réveiller un disque ATA rotationnel pour lire sa température :
  vérifier son standby et conserver `smartctl -n standby,3` comme barrière
  anti-race.
- Le JSON SMART ne fournit une température qu'au travers de champs sémantiques
  structurés. Ne pas parser les attributs ATA 190/194, `raw.value` ou
  `raw.string`. Toute valeur sémantique finie est valide ; cette couche
  n'impose pas de plage physique arbitraire.
- `Temp=0` peut être une vraie mesure ou la sentinelle `standby`/`waking` : seul
  l'état les distingue. Ne jamais republier la température physique précédant
  le standby pendant le réveil.
- La grâce de réveil exige une transition standby -> active observée sans
  interruption. Une erreur d'inventaire casse cette continuité.
- Après l'échec d'un SMART direct, publier le disque indisponible plutôt que
  réutiliser une ancienne mesure.

### Inventaire et identité

- L'identité disque est l'ID stable Unraid, jamais `/dev/sdX`.
- Les entrées assigned gagnent sur les entrées unassigned correspondantes. Des
  IDs actifs dupliqués dans une même source invalident tout l'inventaire.
- Valider strictement `rotational`/`spundown` seulement pour les entrées incluses
  dans la collecte ; une entrée exclue ou purement affichée ne doit pas invalider
  l'ensemble pour ces champs.
- En mode `Auto`, inclure par prudence un bus physique inconnu, exclure les
  disques USB confirmés et toujours exclure `flash`.

### Propriété de l'état runtime

Conserver ces responsabilités distinctes :

- `smartSourceState` : heartbeat emhttpd, fallback et cadence SMART ;
- `diskStateTracker` : continuité de l'état thermique par disque ;
- `lastSuccessfulSnapshot` : dernière vue disque cohérente et réussie ;
- collecte HBA : indépendante de la collecte disque.

Ne pas les fusionner dans un état générique pour réduire le nombre de lignes.

### Control plane Unraid

- Le daemon est l'unique writer des politiques disque persistées. La WebUI
  consulte et modifie celles-ci via l'API locale sur socket Unix.
- Ne pas réintroduire de CLI d'administration, de writer concurrent ni
  d'écriture de `disk-policies.json` depuis PHP ou les scripts.
- `service.sh` reste un trampoline WebGUI pour le cycle de vie du service.
- Une panne du control plane ne doit arrêter ni la collecte ni VSOCK.

### VSOCK, configfs et hwmon

- La collecte reste indépendante de la publication : une opération disque/HBA
  lente ne doit pas bloquer le heartbeat VSOCK.
- Conserver le flux JSON persistant délimité par des retours à la ligne et sa
  validation dans `internal/sensors`. Toute rupture volontaire met à jour
  `ProtocolVersion` et ses tests.
- Configfs (`/sys/kernel/config/virt_temp`) est le control plane de `virt-temp` :
  il porte l'existence des sondes et leurs métadonnées.
- `/dev/virt-temp/*` est le data plane qui reçoit les températures runtime. Ne
  pas déplacer les températures dans configfs pour supprimer le miscdevice ou
  réduire le LOC.
- Quand `unraid-vsock-hwmon` fonctionne, il est l'unique writer supporté de
  `/sys/kernel/config/virt_temp`. Les manipulations manuelles sont réservées au
  développement et au diagnostic avec le receiver arrêté ; ce contrat
  n'implique ni verrou d'ownership ni surveillance périodique du drift.
- La réconciliation configfs est idempotente et non transactionnelle. Un échec
  peut laisser temporairement un état kernel intermédiaire, corrigé lors d'une
  tentative ultérieure.
- `hwmonInventory.sensors` décrit la dernière topologie complètement
  réconciliée, pas nécessairement cet état kernel intermédiaire.
  `needsReconcile` demande une nouvelle tentative.
- `reconfigured` signifie qu'une réconciliation complète a réussi et qu'une
  notification de changement de topologie est pertinente. Un échec partiel
  conserve le dernier inventaire valide, fixe `needsReconcile=true` et ne publie
  pas cet événement. Un succès dans l'autre famille ne doit pas masquer cet
  état : le signal global `reconfigured` n'est émis que lorsque toutes les
  familles sont à nouveau réconciliées.
- À la création, écrire la température avant le premier `label`, car ce dernier
  rend la sonde visible via hwmon.
- Ne pas réintroduire `sample`/`configure`/`commit`, de staging ou d'inventaire
  transactionnel dans le noyau pour rendre la réconciliation atomique.
- Côté Proxmox, persister uniquement la topologie. Préserver le stale/failsafe :
  après `stale_timeout` sans refresh, hwmon retourne la valeur de sécurité de
  `100 °C`.
- Garder la découverte matérielle et la logique métier en userspace ; le module
  noyau reste petit et mécanique.

### Diagnostics

- Les diagnostics observent un snapshot runtime en lecture seule. Ils ne
  déclenchent jamais SMART, `sdspin`, collecte HBA ou refresh.
- Ne pas accéder directement aux mutex des collectors : utiliser leurs statuts
  ou snapshots cohérents.
- Les IDs peuvent contenir des numéros de série ; conserver l'avertissement de
  masquage avant partage.

## Simplicité et portée des changements

- Implémenter d'abord le plus petit changement qui satisfait le comportement
  demandé et préserve les invariants existants. Ne pas anticiper des besoins
  futurs ni ajouter de CLI, endpoint, fallback, état, persistance, goroutine
  ou voie parallèle sans scénario concret.
- N'introduire une interface que pour de vrais backends interchangeables ou une
  frontière externe utile aux tests. Ne pas généraliser avant au moins deux
  usages partageant la même responsabilité et un propriétaire naturel.
- Ne factoriser que la duplication conceptuelle, pas une simple ressemblance
  syntaxique. Préserver la direction `composants métier -> internal/sensors`.
- Une fonction à un seul appelant peut matérialiser une frontière utile. Un
  type métier, un snapshot, une copie sous mutex ou une projection immutable ne
  sont pas du code mort du seul fait qu'ils ajoutent des lignes.
- Une simplification doit réellement supprimer un état, un chemin, une
  responsabilité, une dépendance ou une duplication conceptuelle. Réduire le
  LOC n'est pas un objectif.
- Ne pas découper `hba_mpt3ctl.go` ou `virt-temp.c` si cela disperse leur contexte
  d'ABI et de cycle de vie.
- Lorsqu'un nouveau mécanisme remplace l'ancien, retirer le chemin devenu
  inutile dès que la compatibilité le permet.
- Avant de conclure, rechercher les helpers, états, chemins et abstractions
  devenus redondants, sans modifier le comportement d'un refactoring annoncé
  comme neutre.

## Conventions

- La version Go de référence est déclarée dans `go.mod` ; celle du template VM
  est dans `tests/vm/go-version`.
- Formater le Go avec `gofmt` / `make fmt`. Garder code, commentaires et erreurs
  en anglais ; garder la documentation utilisateur en français sauf convention
  locale contraire.
- Ajouter `// SPDX-License-Identifier: GPL-3.0-or-later` aux nouveaux fichiers
  Go. Utiliser LF et préserver le dialecte des scripts POSIX shell.
- Donner aux erreurs assez de contexte et respecter les mécanismes existants
  anti-spam/sticky-error.
- Protéger le travail de l'utilisateur : ne pas écraser ou nettoyer des
  modifications préexistantes et ne toucher qu'aux fichiers nécessaires.

## Fichiers générés et release

- `bin/`, `dist/` et `unraid-plugin/unraid-vsock-sensors.plg` sont générés. Pour
  le `.plg`, modifier sa source puis le régénérer par le workflow prévu.
- Ne pas lancer `make release`, créer de tag ou pousser sans demande explicite.
- Suivre `CONTRIBUTING.md` pour l'ordre de release et les remotes (`origin`
  Gitea, `github` public).

## Validation

Pour un changement Go/plugin habituel, exécuter au minimum :

```sh
make check
make test-race
```

`make check` couvre formatage, modules, vet, tests Go, syntaxes shell/PHP,
scripts plugin et ShellCheck.

Utiliser les tests VM pour les vraies frontières noyau, configfs/miscdevice,
hwmon/sysfs, cache de topologie, DKMS/Debian, systemd ou block devices :

```sh
make test-vm
make test-vm-core
make test-vm-package
```

Préférer les tests unitaires pour parsers, machines d'état, fixtures sysfs,
StorCLI et erreurs/timeouts simulés. Pour le parser MPT3, envisager aussi
`make fuzz-mpt3`.

Toujours indiquer les validations réellement exécutées et celles impossibles ;
ne jamais annoncer un succès non observé.

## Git et restitution

- Ne jamais exécuter `git add`, `git commit`, `git tag` ou `git push`. Une
  demande de découpage en commits n'autorise pas leur création.
- Laisser worktree et index en l'état pour la review utilisateur. Présenter les
  fichiers modifiés, le changement fonctionnel, les compromis, les validations
  et un message de commit proposé.
- Un changement de comportement requiert des tests ciblés et, si son contrat
  public change, la documentation associée. Préserver la compatibilité et le
  comportement conservateur lorsque l'état matériel est incertain.

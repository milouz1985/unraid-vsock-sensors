# AGENTS.md

## Objectif

`unraid-vsock-sensors` est un petit projet Linux/Go qui exporte les températures
des disques et des HBA depuis une VM Unraid vers un hôte Proxmox via
`AF_VSOCK`, puis les expose comme capteurs Linux `hwmon` natifs au moyen du
module noyau `virt-temp`.

Garder l'implémentation directe et conservatrice. Préférer du code explicite et
des transitions d'état claires à des abstractions génériques.

Avant toute modification non triviale, lire les parties pertinentes de :

- `README.md` pour le comportement à l'exécution et les usages supportés ;
- `CONTRIBUTING.md` pour l'architecture, les tests, le processus de release et
  les conventions ;
- `tests/vm/README.md` pour les changements touchant l'intégration VM, noyau ou
  packaging ;
- `virt-temp/README.md` pour les changements touchant le module noyau ou le côté
  Proxmox.

## Cartographie du dépôt

Package Go principal :

- `main.go` : points d'entrée du binaire (`serve`, côté hwmon/Proxmox et version) et câblage global du processus.
- `disk.go` : orchestration du collector disque et snapshot runtime publié.
- `disk_inventory.go` : inventaire des disques Unraid et gestion de leur identité.
- `disk_source.go` : heartbeat emhttpd et planification du fallback SMART direct.
- `disk_source_config.go` : configuration `poll_attributes` et fraîcheur SMART.
- `disk_state.go` : continuité de l'état thermique (`valid`, `standby`, `waking`,
  `unavailable`).
- `disk_fallback.go` : fallback SMART direct borné.
- `disk_policy.go` : politiques include/exclude par disque et stockage persistant associé.
- `control_server.go` : API HTTP locale sur socket Unix utilisée par la WebUI pour l'inventaire et les mutations de politiques.
- `hba.go` : orchestration du collector HBA.
- `hba_mpt3ctl.go` : backend ioctl MPT3 natif.
- `hba_storcli.go` : backend StorCLI.
- `publisher.go` : publisher VSOCK persistant.
- `hwmon*.go` : receiver côté Proxmox, gestion topologie/cache et `/dev/virt-temp`.
- `diagnostics.go` : génération du snapshot JSON de diagnostic runtime en lecture seule.
- `internal/sensors` : modèle/protocole partagé et framing VSOCK.
- `internal/vsockaddr` : validation partagée du CID et du port VSOCK.

Autres composants :

- `unraid-plugin/` : interface du plugin Unraid, client PHP du control socket,
  scripts rc, packaging et `.plg` généré.
- `virt-temp/module/virt-temp.c` : pont noyau vers hwmon.
- `virt-temp/` : packaging Debian/DKMS et unités systemd.
- `tests/vm/` : tests d'intégration réels Proxmox/noyau/systemd.

## Invariants architecturaux

Préserver ces règles sauf si la tâche demande explicitement un changement de
comportement.

### Collecte des disques

- En fonctionnement normal, les températures disque proviennent de l'état/cache
  déjà maintenu par `emhttpd` d'Unraid.
- Le SMART direct n'est qu'un fallback lorsque le heartbeat `poll_attributes`
  devient stale. Il doit rester borné dans le temps et en concurrence.
- Ne jamais réveiller volontairement un disque ATA rotationnel uniquement pour
  obtenir sa température. Vérifier d'abord son état standby et conserver la
  barrière anti-race `smartctl -n standby,3`.
- Le parsing JSON SMART consomme uniquement des champs de température sémantiques
  et structurés. Ne pas réintroduire le parsing des attributs ATA 190/194, de
  `raw.value` ou de `raw.string`.
- Une température sémantique est valide si elle est finie. Ne pas ajouter de
  bornes arbitraires de plausibilité dans une couche qui n'est pas responsable
  de l'interprétation de la valeur matérielle.

### État thermique

- `Temp=0` avec un disque disponible peut être la sentinelle synthétique utilisée
  pour `standby` ou `waking` ; il peut aussi s'agir d'une vraie mesure à `0 °C`.
- C'est l'état, et non la valeur numérique seule, qui distingue ces cas.
- Ne jamais réutiliser la température physique précédant le standby pendant le
  réveil du disque.
- La grâce de réveil n'est autorisée qu'après une transition standby -> active
  observée sans interruption. Une erreur de visibilité dans l'inventaire casse
  cette continuité.
- Si une tentative SMART directe en fallback échoue, publier le disque comme
  indisponible ; ne pas réutiliser une ancienne température physique.

### Inventaire et identité

- L'identité d'un disque est son identifiant stable fourni par Unraid, jamais
  `/dev/sdX`.
- Les entrées assigned gagnent sur les entrées unassigned correspondantes.
- Des IDs actifs dupliqués dans une même source invalident l'inventaire ; ne pas
  publier un inventaire partiel.
- La validation stricte de `rotational`/`spundown` ne s'applique qu'aux entrées
  réellement incluses dans la collecte. Les entrées exclues ou destinées à
  l'affichage ne doivent pas casser tout l'inventaire si ces champs thermiques
  sont absents ou invalides.
- En mode `Auto`, un bus physique inconnu est inclus de manière conservatrice ;
  les disques USB confirmés sont exclus. Le périphérique `flash` est toujours
  exclu.

### Propriété de l'état runtime

Conserver la séparation actuelle des responsabilités :

- `smartSourceState` : heartbeat emhttpd, transitions de fallback et cadence des
  tentatives SMART directes.
- `diskStateTracker` : continuité/historique thermique par disque.
- `lastSuccessfulSnapshot` : dernière vue runtime/publiée cohérente et réussie
  des disques.
- La collecte HBA reste indépendante de la collecte disque.

Ne pas fusionner ces éléments dans un état générique ou monolithique uniquement
pour réduire le nombre de lignes.

### Control plane Unraid

- Le daemon est l'unique propriétaire en écriture des politiques disque persistées.
- La WebUI communique avec le daemon via l'API HTTP locale sur socket Unix pour
  consulter l'inventaire et modifier les politiques.
- Ne pas réintroduire de CLI d'administration disque, de writer concurrent ou
  d'écriture directe de `disk-policies.json` depuis PHP ou les scripts du plugin.
- `service.sh` est uniquement le trampoline requis par le WebGUI Unraid pour les
  opérations de cycle de vie du service ; ne pas y remettre de logique métier.
- Une défaillance du control plane ne doit pas arrêter la collecte thermique ni
  la publication VSOCK.

### VSOCK et hwmon

- La collecte doit rester indépendante de la publication afin qu'une opération
  disque/HBA lente ne puisse pas bloquer le heartbeat VSOCK.
- Conserver le flux JSON persistant délimité par des retours à la ligne et la
  validation du protocole dans `internal/sensors`.
- Traiter volontairement les changements de protocole incompatibles : mettre à
  jour `ProtocolVersion` et les tests correspondants lorsqu'une rupture de
  compatibilité est intentionnelle.
- Côté Proxmox, ne persister que la topologie, jamais les températures.
- Préserver le comportement stale/failsafe du module noyau. Une absence de
  refresh doit finir par rendre le capteur indisponible à la frontière hwmon.
- Garder autant que possible la logique métier et la découverte matérielle en
  userspace. Le module noyau doit rester petit et mécanique.

### Diagnostics

- Les diagnostics sont des observations en lecture seule de l'état runtime
  courant.
- La page de diagnostics lit uniquement le snapshot JSON runtime et ne doit jamais
  déclencher SMART, `sdspin`, une collecte HBA ou un signal de refresh.
- Éviter que les diagnostics accèdent directement aux mutex internes des
  collectors. Utiliser les statuts exposés par les collectors ou des snapshots
  runtime cohérents.
- Les IDs disque peuvent contenir des numéros de série ; conserver
  l'avertissement/documentation existant sur leur masquage avant partage d'un
  diagnostic.

## Principes de conception

Préférer l'implémentation la plus simple qui préserve les invariants ci-dessus.

Ne pas introduire d'architecture générique sans besoin concret. Éviter notamment
les abstractions de type provider de température générique, couches
repository/service, mega-state, ou framework générique d'écriture atomique
lorsque les composants existants ont des sémantiques réellement différentes.

Les petites interfaces sont pertinentes lorsqu'il existe de vrais backends
interchangeables ou lorsqu'elles isolent une frontière externe pour les tests,
par exemple les readers HBA ou les connexions VSOCK.

Ne pas découper du code bas niveau cohésif uniquement pour raccourcir les
fichiers. `hba_mpt3ctl.go` et `virt-temp/module/virt-temp.c` gagnent à conserver
localement leur contexte d'ABI et de cycle de vie.

Lors d'un refactoring, préférer dans cet ordre :

1. supprimer l'état réellement dupliqué ou les wrappers sans responsabilité propre ;
2. déplacer le code vers le composant qui en est réellement propriétaire ;
3. tester le comportement observable plutôt que les détails d'implémentation
   privés ;
4. garder le flux de données explicite, même lorsqu'un helper générique pourrait
   économiser quelques lignes.

La réduction du LOC n'est pas un objectif en soi.

## Conventions de code

- Version de Go : 1.27.0 ou supérieure, comme déclaré par le dépôt.
- Formater le Go avec `gofmt` / `make fmt`.
- Garder les identifiants Go, commentaires de code et messages d'erreur/diagnostic
  cohérents avec le code existant en anglais.
- Garder la documentation destinée aux utilisateurs en français sauf si le
  fichier environnant utilise une autre langue.
- Les nouveaux fichiers Go doivent conserver l'en-tête SPDX du dépôt :
  `// SPDX-License-Identifier: GPL-3.0-or-later`.
- Les fichiers texte utilisent des fins de ligne LF.
- Respecter le dialecte shell existant : les fichiers déclarés POSIX shell doivent
  rester compatibles POSIX ; ne pas les convertir implicitement en Bash.
- Préférer des erreurs contenant suffisamment de contexte pour diagnostiquer
  l'opération qui a échoué.
- Éviter le spam de logs lorsqu'un mécanisme de transition ou sticky-error existe
  déjà.
- Garder les commits ciblés ; ne pas mélanger refactoring sans rapport,
  changement de comportement et nettoyage documentaire.

## Fichiers générés et release

- `bin/` et `dist/` sont des sorties générées.
- `unraid-plugin/unraid-vsock-sensors.plg` est généré dans le workflow de
  release. Modifier de préférence sa source/template puis le régénérer plutôt
  que d'éditer directement le descripteur généré.
- Ne pas exécuter `make release`, créer de tag, commit ou push sauf si la tâche
  le demande explicitement.

Remotes utilisés par le processus de release documenté :

- `origin` : dépôt de développement Gitea ;
- `github` : dépôt GitHub public utilisé pour distribuer les releases/fichiers du
  plugin.

Suivre `CONTRIBUTING.md` pour l'ordre exact des opérations de release.

## Validation

Pour les changements Go/plugin habituels, exécuter au minimum :

```sh
make check
make test-race
```

`make check` couvre le formatage, la cohérence des modules, `go vet`, les tests
Go, la syntaxe shell/PHP, les tests des scripts du plugin et ShellCheck.

Utiliser les tests VM lorsqu'un changement traverse une vraie frontière système,
notamment :

- comportement du module noyau `virt-temp` ;
- `/dev/virt-temp` et hwmon/sysfs ;
- comportement du cache de topologie ;
- packaging DKMS ou Debian ;
- intégration systemd ;
- comportement réel des block devices Linux.

Commandes utiles :

```sh
make test-vm
make test-vm-core
make test-vm-package
```

Préférer les tests unitaires pour les parsers, machines d'état temporelles,
fixtures USB/sysfs de topologie, StorCLI, erreurs/timeouts HBA simulés et autres
cas qui ne feraient que simuler du matériel à l'intérieur d'une VM.

Pour les changements du parser MPT3, envisager aussi :

```sh
make fuzz-mpt3
```

Avant de déclarer une tâche terminée, indiquer quels contrôles ont réellement
été exécutés et lesquels n'ont pas pu l'être. Ne jamais affirmer qu'un test est
passé si l'environnement a empêché son exécution.

## Discipline de modification

### Commits et revue utilisateur

- Ne jamais exécuter `git add`, `git commit`, `git tag` ou `git push` dans ce
  dépôt. L'utilisateur veut relire les modifications puis les indexer et les
  committer lui-même.
- Une consigne demandant un « commit séparé » ou un changement « dans un commit
  purement organisationnel » décrit le découpage attendu des modifications ;
  elle n'autorise pas Codex à créer ce commit.
- À la fin d'une modification, laisser le worktree et l'index en l'état,
  présenter les changements et les validations effectuées, puis proposer un ou
  plusieurs messages de commit adaptés au découpage demandé.
- Ne créer un commit que si l'utilisateur revient explicitement et demande sans
  ambiguïté à Codex de l'exécuter, malgré cette préférence générale.

Lorsqu'un comportement change volontairement :

- mettre à jour ou ajouter des tests ciblés avant ou en même temps que
  l'implémentation ;
- mettre à jour `README.md` / `CONTRIBUTING.md` lorsque le contrat documenté
  change ;
- préserver la compatibilité ascendante sauf si la tâche autorise explicitement
  une rupture ;
- conserver un comportement de sécurité conservateur lorsque l'état matériel est
  incertain.

Lorsqu'une tâche est uniquement un refactoring, les tests doivent conserver le
même comportement visible de l'extérieur. Si un refactoring oblige à modifier
les attentes comportementales, s'arrêter et réévaluer s'il est réellement
neutre fonctionnellement.

## Discipline de simplicité et périmètre des changements

Pour toute évolution, implémenter d'abord le plus petit changement qui satisfait le besoin demandé et préserve les invariants existants.

Ne pas anticiper des besoins futurs qui ne sont pas explicitement demandés ou déjà présents dans le dépôt.

En particulier :

- ne pas ajouter de CLI, endpoint, API, mode de fonctionnement, fallback ou compatibilité supplémentaire sans besoin concret ;
- ne pas ajouter une abstraction uniquement parce qu'elle pourrait avoir plusieurs implémentations plus tard ;
- ne pas créer d'interface lorsqu'il n'existe qu'une seule implémentation réelle, sauf frontière externe utile aux tests ;
- ne pas créer de package, couche, wrapper ou type intermédiaire uniquement pour isoler quelques lignes ;
- ne pas dupliquer un chemin d'accès existant sous une nouvelle forme "pour être complet" ;
- ne pas conserver deux mécanismes permettant d'effectuer la même opération sauf nécessité de compatibilité explicitement identifiée ;
- ne pas ajouter de mécanisme de fallback sans scénario de panne réel et comportement attendu documenté ;
- ne pas introduire de nouvelle persistance, cache, mutex, lock, goroutine ou état runtime sans expliquer quel problème concret il résout ;
- ne pas généraliser une implémentation locale tant qu'au moins deux usages réels ne le nécessitent. Plusieurs usages similaires ne suffisent pas à eux seuls : ils doivent partager la même responsabilité et avoir un propriétaire commun naturel.

Lorsqu'une fonctionnalité traverse plusieurs composants, conserver le minimum de chemins possibles.

Par exemple, pour une opération WebUI vers daemon :

WebUI -> client local -> socket Unix -> daemon

Ne pas ajouter parallèlement une CLI Go, un second client, un writer direct ou une autre voie d'administration sauf demande explicite.

### Avant d'ajouter une nouvelle construction

Avant de créer un nouveau fichier, type, interface, état, goroutine, endpoint ou mécanisme de fallback, vérifier :

1. Quel besoin actuel impose cette construction ?
2. Existe-t-il déjà un composant qui en est naturellement propriétaire ?
3. Peut-on résoudre le besoin avec le flux existant ?
4. Cette construction supprime-t-elle davantage de complexité qu'elle n'en ajoute ?
5. Existe-t-il au moins un appelant ou scénario réel qui l'utilise immédiatement ?

Si la justification repose principalement sur "au cas où", "plus tard", "pour être extensible" ou "pour être générique", ne pas ajouter la construction.

### Taille et portée d'un patch

Un changement fonctionnel doit rester centré sur son objectif.

Ne pas profiter d'une évolution pour :

- ajouter des fonctionnalités adjacentes non demandées ;
- généraliser les composants touchés ;
- préparer une architecture pour de futurs usages hypothétiques ;
- remplacer des mécanismes existants qui ne posent pas de problème pour homogénéiser le code ;
- créer des chemins alternatifs "au cas où".

Si l'implémentation devient significativement plus large que le besoin initial, réévaluer l'approche avant de continuer.

Une augmentation importante du nombre de fichiers, types, états ou chemins d'exécution doit être considérée comme un signal d'alerte, pas comme une conséquence normale d'une fonctionnalité.

### Préférence pour la suppression

Lors d'une évolution d'architecture, rechercher activement ce que le nouveau mécanisme permet de supprimer.

Si un nouveau chemin remplace un ancien chemin, supprimer l'ancien dès que la compatibilité le permet au lieu de maintenir les deux par défaut.

Une migration n'est pas terminée lorsqu'un nouveau mécanisme fonctionne ; elle est terminée lorsque les mécanismes devenus inutiles ont été retirés.

### Revue de fin de changement

Avant de considérer une évolution terminée, refaire une passe spécifique de simplification :

- quels fichiers ou helpers ajoutés peuvent être supprimés ?
- quels états sont dérivables plutôt que stockés ?
- quels chemins sont maintenant impossibles ou redondants ?
- quelles abstractions n'ont finalement qu'un seul usage ?
- quels mécanismes précédents sont devenus inutiles ?
- des tests valident-ils des détails d'implémentation qui pourraient être remplacés par du comportement observable ?

Ne pas conserver du code uniquement parce qu'il a déjà été écrit.

### Distinguer simplification réelle et réduction syntaxique

Une simplification doit réduire une responsabilité, un état indépendant, un chemin d'exécution ou une dépendance.

Réduire uniquement le nombre de lignes, de fonctions, de types ou de fichiers n'est pas en soi une simplification.

Avant de supprimer, fusionner, factoriser ou déplacer une construction existante, identifier son rôle sémantique. Ne pas modifier une construction uniquement parce qu'elle est petite, n'a qu'un appelant ou ressemble syntaxiquement à une autre.

#### Duplication syntaxique et duplication conceptuelle

Deux morceaux de code similaires ne doivent être factorisés que s'ils représentent la même responsabilité et appartiennent naturellement au même composant.

La ressemblance d'implémentation seule n'est pas suffisante.

En particulier :

- ne pas créer un helper partagé uniquement parce que plusieurs composants utilisent la même expression simple ;
- ne pas déplacer une règle métier ou une validation locale dans un package partagé uniquement pour éliminer quelques lignes ;
- plusieurs usages réels sont une condition utile pour envisager une abstraction, mais ne suffisent pas à la justifier ;
- une abstraction partagée doit avoir un propriétaire sémantique clair.

Préférer deux expressions locales simples à un helper partagé dont la responsabilité serait ambiguë.

#### Wrappers et frontières architecturales

Une fonction avec un seul appelant n'est pas nécessairement un wrapper mort.

Conserver un wrapper lorsqu'il matérialise une frontière utile, notamment :

- adaptation entre configuration de production et logique testable ;
- construction d'une dépendance externe ;
- séparation entre orchestration et implémentation ;
- frontière de propriété ou de package ;
- point d'entrée donnant un nom à une opération de plus haut niveau.

Ne pas inliner une fonction uniquement pour supprimer un niveau d'appel.

Un wrapper peut être supprimé lorsque son retrait élimine réellement une responsabilité ou une indirection sans déplacer cette même logique chez l'appelant.

#### Copies, projections et snapshots

Ne pas considérer automatiquement une copie de données comme un état dupliqué.

Une copie ou une structure distincte peut être volontaire lorsqu'elle :

- capture une vue cohérente sous mutex ;
- empêche l'exposition d'un état mutable interne ;
- matérialise un snapshot temporel ;
- convertit un modèle runtime riche vers un modèle publié plus restreint ;
- évite l'aliasing entre composants ayant des cycles de vie différents.

Avant de supprimer une copie, une projection ou un type de snapshot, vérifier explicitement :

1. qui possède les données avant et après le changement ;
2. si les données peuvent encore muter ;
3. si la cohérence temporelle reste garantie ;
4. si le consommateur obtient davantage d'état interne qu'auparavant ;
5. si le changement crée un alias sur une structure interne.

Ne pas remplacer un snapshot immutable par une référence vers l'état mutable uniquement pour supprimer un type ou quelques champs copiés.

#### Types sémantiques et booléens

Ne pas remplacer systématiquement un type métier, une enum ou un état nommé par un booléen pour réduire le code.

Préférer un type explicite lorsqu'il représente un concept du domaine et rend les appels ou les conditions plus lisibles.

Un booléen est préférable uniquement lorsque la propriété représentée est intrinsèquement binaire et que son sens reste évident à tous les endroits où il est construit et consommé.

La suppression de quelques constantes n'est pas une simplification si elle fait perdre le vocabulaire du domaine.

#### Propriété des packages et direction des dépendances

Ne pas déplacer une fonction vers un package plus générique uniquement parce qu'elle est pure ou utilisée depuis plusieurs endroits.

Avant tout déplacement, vérifier que le concept appartient réellement au package cible.

Les packages bas niveau ou partagés doivent rester indépendants des composants d'orchestration qui les utilisent.

En particulier, `internal/sensors` reste propriétaire du modèle/protocole partagé et du framing VSOCK. Ne pas y déplacer des helpers propres à la collecte disque, HBA, hwmon ou à leur orchestration simplement pour mutualiser du code.

Préserver une direction de dépendance simple :

composants métier -> modèles/protocoles partagés

et non l'inverse.

#### Critère de revue d'un refactoring

Pour chaque simplification proposée, être capable de répondre à la question :

« Qu'est-ce qui disparaît réellement après ce changement ? »

Réponses valables :

- un état indépendant ;
- un chemin d'exécution ;
- une responsabilité ;
- une dépendance ;
- une duplication conceptuelle ;
- un mécanisme devenu inutile.

Réponses insuffisantes à elles seules :

- une fonction ;
- un type ;
- quelques lignes ;
- une boucle ;
- un fichier ;
- un appel intermédiaire.

Si la même responsabilité subsiste simplement à un autre endroit, considérer le changement comme un déplacement de code et non comme une simplification.
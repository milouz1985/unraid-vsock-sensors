# Contribuer à unraid-vsock-sensors

Ce document décrit l'environnement de développement, les vérifications à
effectuer et le processus de publication de `unraid-vsock-sensors`.

Pour l'installation et l'utilisation du projet, voir [`README.md`](README.md).

## Sources de vérité documentaires

Chaque information normative a un propriétaire principal :

- [`README.md`](README.md) décrit l'installation, la configuration,
  l'exploitation, les diagnostics, la récupération et la sécurité visibles par
  l'opérateur ;
- ce document est la source de vérité de l'architecture, des invariants, des
  décisions de conception et des lifecycles techniques nécessaires pour
  modifier le projet ;
- [`virt-temp/README.md`](virt-temp/README.md) détaille uniquement l'interface,
  le lifetime et les contraintes du module noyau : configfs, miscdevices,
  hwmon, IDs, stale timeout et unload ;
- [`tests/vm/README.md`](tests/vm/README.md) décrit la portée et les procédures
  des tests d'intégration réels ;
- [`AGENTS.md`](AGENTS.md) contient uniquement les règles de travail propres aux
  agents, les validations à exécuter et les renvois vers ces sources.

Une information absente d'`AGENTS.md` peut donc rester un invariant du projet.
Les renvois sont préférés à la copie d'une même règle dans plusieurs documents.

## Cartographie du dépôt

Le package Go principal est organisé par responsabilité :

- `main.go` fournit les commandes `serve`, `hwmon` et `version` ;
- `disk*.go` couvre l'inventaire Unraid, les politiques, emhttpd/SMART et l'état
  thermique des disques ;
- `control_server.go` expose l'API locale de la WebUI sur socket Unix ;
- `hba*.go` couvre l'inventaire HBA et les backends MPT3/StorCLI ;
- `publisher.go` maintient la publication VSOCK ;
- `hwmon*.go` valide, persiste et réconcilie la topologie Proxmox ;
- `diagnostics.go` construit le snapshot runtime en lecture seule ;
- `internal/sensors` porte le modèle et le framing VSOCK partagés ;
- `internal/vsockaddr` valide le CID et le port.

Les autres composants principaux sont `unraid-plugin/` pour la WebUI et le
packaging Unraid, `virt-temp/` pour le module et le paquet Debian/DKMS, et
`tests/vm/` pour les tests réels Proxmox, noyau, systemd et packaging.

## Prérequis

Le projet nécessite :

- Go 1.27.0 ou plus récent ;
- GNU Make ;
- Bash ;
- PHP CLI ; les extensions cURL et PCNTL sont nécessaires pour le test
  d'intégration du client de la socket de contrôle ;
- ShellCheck ;
- Git ;
- `dpkg-buildpackage`, debhelper 13 et `rsync` pour construire le paquet Proxmox.

Sous Debian ou Ubuntu :

```sh
sudo apt install make shellcheck php-cli php-curl git debhelper rsync
```

Le module `virt-temp` nécessite également les headers Linux correspondant au
noyau utilisé pour sa compilation.

## Vérifications courantes

Les principales commandes sont :

```sh
make fmt
make tidy
make check
make test-race
make fuzz-mpt3
make build
make artifacts
```

`make check` exécute notamment :

- vérification `gofmt` ;
- vérification de `go.mod` et `go.sum` ;
- `go vet`;
- tests Go ;
- syntaxe Bash et POSIX shell ;
- lint PHP ;
- tests des scripts du plugin ;
- ShellCheck.

Le race detector est exécuté séparément :

```sh
make test-race
```

## Fuzzing MPT3

Le parser binaire de IO Unit Page 7 dispose d'une cible de fuzzing.

Lancer la campagne par défaut :

```sh
make fuzz-mpt3
```

Modifier leur durée :

```sh
make fuzz-mpt3 FUZZTIME=2m
```

Une campagne longue ne fait pas partie de `make check`, mais le seed du fuzzer
est exécuté par `go test ./...`.

## Tests d'intégration Proxmox

Les tests VM utilisent un vrai noyau Proxmox, le vrai module `virt_temp`, DKMS,
systemd et de vrais block devices QEMU.

Commandes principales :

```sh
make vm-template-sync
make vm-template-rebuild
make test-vm
make test-vm-core
make test-vm-package
make test-vm-vsock
```

Le fonctionnement du runner et la préparation du template sont documentés dans
[`tests/vm/README.md`](tests/vm/README.md).

Les tests VM doivent être privilégiés lorsqu'ils permettent de vérifier une
véritable frontière système, par exemple :

- module noyau ;
- configfs `virt_temp` ;
- les nodes `/dev/virt-temp/*` ;
- hwmon/sysfs ;
- DKMS ;
- systemd ;
- suppression/recréation réelle de la topologie kernel ;
- AF_VSOCK guest → host via un guest KVM imbriqué dans la VM disposable.

Les tests unitaires restent préférables pour les cas qui nécessiteraient sinon
de simuler du matériel dans la VM, notamment :

- parsers ;
- topologies sysfs USB particulières ;
- StorCLI ;
- timeouts ioctl ;
- erreurs HBA ;
- logique temporelle ;
- fuzzing MPT3.

La frontière suit la propriété testée, pas le langage du test. Une décision
pure de script, comme l'ordre « tous les builds DKMS avant toute installation »,
reste dans un test classique. Le comportement réel de `dpkg`, DKMS, systemd,
configfs, d'un miscdevice ou d'un module retenu par un FD appartient à la VM.
Ne pas maintenir un faux gestionnaire de paquets, faux noyau ou faux systemd
pour rejouer cette intégration. Pour les unités systemd, utiliser
`systemd-analyze verify` pour la validité statique et réserver la VM aux effets
fonctionnels.

## Principes d'architecture

Le projet cherche à conserver une séparation claire entre les responsabilités.

### Côté Unraid

#### Collecte des disques et état runtime

Quand le heartbeat `poll_attributes` est sain, les champs `temp` et `spundown`
maintenus par `emhttpd` font autorité. UVSS ne connaît pas l'âge physique de la
mesure. Entre deux acquisitions SMART prévues, republier la dernière valeur est
intentionnel : `stale_timeout` protège l'absence de refresh vers hwmon, pas
l'âge de la mesure matérielle. Ne pas ajouter de TTL thermique indépendant sans
nouveau besoin fonctionnel.

Le SMART direct est un fallback temporaire quand ce heartbeat devient stale.
Il reste borné en temps et en concurrence. Pour un disque ATA rotationnel, la
collecte vérifie d'abord le standby puis conserve `smartctl -n standby,3` comme
barrière anti-race afin de ne jamais réveiller le disque. Après un échec SMART,
le disque devient indisponible au lieu de republier une ancienne mesure.

Le JSON SMART ne fournit une température qu'au travers de champs sémantiques
structurés. Les attributs ATA 190/194, `raw.value` et `raw.string` ne sont pas
des sources de température. Toute valeur sémantique finie est acceptée sans
filtre arbitraire de plausibilité physique.

La continuité thermique reste répartie entre des propriétaires distincts :

- `smartSourceState` porte le heartbeat emhttpd, le fallback et la cadence
  SMART ;
- `diskStateTracker` porte les transitions `standby`, `waking` et `active` ;
- `lastSuccessfulSnapshot` porte la dernière vue disque entièrement cohérente ;
- la collecte HBA possède son propre état.

Ces états ne doivent pas être fusionnés dans un cache générique. Une wake grace
exige une transition standby vers actif observée sans erreur d'inventaire
intermédiaire. UVSS ne republie pas sa mesure pré-standby pendant le réveil.

Dans l'inventaire, l'ID stable Unraid est l'identité ; `/dev/sdX` ne l'est
jamais. Une entrée assigned l'emporte sur l'entrée unassigned correspondante et
des IDs actifs dupliqués dans une même source invalident l'inventaire. Les
champs `rotational` et `spundown` ne sont validés strictement que pour les
entrées effectivement collectées. En mode `Auto`, un bus physique inconnu est
inclus par prudence, un disque USB confirmé est exclu et `flash` reste toujours
exclu.

Les températures des disques proviennent de :

```text
/var/local/emhttp/disks.ini
/var/local/emhttp/devs.ini
```

#### Invariant d'identité des disques emhttpd

UVSS considère `id` comme obligatoire pour tout périphérique physique présent.
Cet invariant vient d'emhttpd, pas d'une identité reconstruite par UVSS.

Dans `devs.ini`, emhttpd résout l'identité avant d'intégrer un périphérique
utilisable. Si cette résolution échoue, il journalise notamment
`device /dev/%s problem getting id` et ignore le périphérique. Un périphérique
physique publié dans `devs.ini` possède donc un `id` non vide.

Dans `disks.ini`, un slot avec un périphérique physique présent reçoit l'ID
correspondant. Les slots sans périphérique conservent un ID vide et utilisent
les états `DISK_NP` ou `DISK_NP_MISSING`, qu'UVSS élimine avant la validation de
l'identité.

Une entrée considérée présente mais sans ID constitue donc une violation du
format attendu, une incohérence temporaire ou un changement du contrat
emhttpd. Conserver la validation stricte et ses tests défensifs : ne pas
inventer une identité depuis `/dev/sdX`, ne pas utiliser le nom de section
comme pseudo-identité et ne pas ignorer silencieusement cette entrée.

#### Control plane Unraid

Le daemon est l'unique writer des politiques disque persistées. La WebUI les
lit et les modifie par l'API locale sur socket Unix ; PHP et les scripts
n'écrivent pas `disk-policies.json`, et aucune CLI d'administration parallèle
n'est prévue. `service.sh` reste un trampoline WebGUI pour le cycle de vie. Une
panne de ce control plane ne doit arrêter ni la collecte ni VSOCK.

#### Collecte HBA et commandes externes

Les identités HBA proviennent de `/sys/class/scsi_host` pour les deux backends ;
les pages Manufacturing MPT3 et les champs d'identité StorCLI ne sont pas des
sources parallèles. Pour `mpt3sas`, `unique_id` fournit directement le numéro
IOC attendu par `MPT3COMMAND` ; aucun ioctl de découverte préalable n'est
utilisé.

SMART et StorCLI ne partagent volontairement pas de helper générique
d'exécution : leurs chaînes de processus et leurs modes de défaillance observés
sont différents.

##### SMART

`smartctl_type` peut réellement lancer `smartctl`. Cette chaîne de processus
connue justifie le process group dédié, tué à l'annulation, afin que le wrapper
ne laisse pas la commande enfant continuer seule.

##### StorCLI

StorCLI est appelé directement avec `exec.CommandContext` et `WaitDelay`. Avec
`StorCLI 007.3404.0000.0000 - April 18, 2025`, la commande exacte utilisée pour
les températures n'a présenté qu'un seul processus sur Unraid :

```text
storcli64 /c0 show temperature
```

Aucun enfant, changement de SID ou PGID, ni processus survivant n'a été observé
sur ce chemin. L'analyse statique du binaire montre néanmoins d'autres chemins
capables de créer des processus, d'appeler `setsid()`, de gérer des événements
asynchrones ou de lancer des commandes externes. L'observation ne constitue
donc pas une garantie générale pour toutes les commandes StorCLI.

Pendant son fonctionnement normal, le processus a notamment été observé dans
la séquence d'états `D -> Dl -> Rl -> Dl -> Sl -> exit`. L'état `D` est un
sommeil kernel non interruptible : si le driver ou le HBA reste bloqué, un
`SIGKILL` peut rester pending jusqu'au retour de l'appel kernel.

`Cmd.WaitDelay` n'est donc pas une borne absolue. `Cmd.Wait()` commence par
`Process.Wait()` et attend la terminaison réelle du processus. Quand le contexte
expire, `os/exec` peut appeler `Cancel`, envoyer `SIGKILL`, réessayer après
`WaitDelay` et fermer les pipes encore ouverts. Cela borne notamment le drainage
de pipes hérités par un descendant et les processus normalement interruptibles,
mais ne rend pas interruptible un task bloqué en `D`.

Un process group StorCLI n'améliorerait pas ce mode de défaillance ; un
descendant appelant `setsid()` échapperait en outre au groupe initial. Un cgroup
permettrait d'identifier ou d'isoler les processus, mais pas d'interrompre un
task en `D`. UVSS n'ajoute donc ni `Setpgid`, ni `kill(-PID)`, ni cgroup, ni
superviseur concurrent pour ce backend.

Réexaminer ce choix seulement si la commande utilisée par UVSS présente
réellement un enfant persistant, un processus détaché ayant un impact, une fuite
répétable, plusieurs instances StorCLI simultanées causées par UVSS, un blocage
du publisher ou une expiration incorrecte des snapshots HBA.

##### Collector HBA

La protection contre un backend bloqué est architecturale. `refresh()` effectue
l'I/O synchrone sans tenir `hbaCollector.mu`, de sorte que `snapshot()` et
`status()` restent disponibles. Le dernier snapshot réussi reste lisible
pendant `interval + hbaCollectionTimeout`, puis expire normalement ; le
publisher continue et signale l'erreur HBA, tandis que les diagnostics indiquent
l'état stale. Si l'appel finit par revenir après sa deadline, son résultat est
rejeté comme timeout.

La boucle de collecte attend ce retour avant l'itération suivante : UVSS ne
lance donc pas une nouvelle commande StorCLI pendant que la précédente reste
bloquée. Il peut y avoir au maximum une collecte HBA ainsi immobilisée, sans
bloquer la collecte disque, la publication VSOCK ou les diagnostics. Les tests
simulent un backend synchrone ignorant temporairement son contexte ; ils ne
prétendent pas reproduire un vrai task kernel en état `D`.

Le backend StorCLI conserve l'association entre ses index et l'inventaire
sysfs tant que la lecture réussit avec le même ensemble d'index. Dans
l'architecture cible, VM Unraid avec passthrough PCI, un remplacement physique
invisible au même BDF sans disparition observée ne justifie pas un mécanisme de
hot-swap supplémentaire. Un tel remplacement peut conserver une identité
périmée jusqu'au redémarrage du daemon ; réexaminer ce compromis si le périmètre
matériel ou l'architecture change.

Les snapshots HBA publiés sont immuables. Une copie superficielle du slice
suffit donc ; les valeurs pointées ne sont pas modifiées. Leur expiration reste
indépendante de la collecte disque afin de protéger le failsafe hwmon.

#### ABI MPT3 et IO Unit Page 7

La transcription amd64 de `MPT3COMMAND` a été vérifiée contre les headers
publics mpt3sas, le driver Linux, le SAS3008 utilisé pour les essais et le
comportement du binaire StorCLI analysé. Le projet reste limité à Linux/amd64.
`TestMPT3CommandABI` verrouille le layout userspace, mais ne prouve pas l'ABI du
noyau chargé ; une rupture conservant la même taille reste un risque résiduel.

La lecture de IO Unit Page 7 suit volontairement cette séquence :

1. requête `PAGE_HEADER` ;
2. allocation selon le `PageLength` retourné par le firmware ;
3. requête `PAGE_READ_CURRENT` avec le header retourné ;
4. décodage des seuls offsets historiques IOC et Board connus.

Une `PageVersion` exacte et une taille exacte ne sont pas exigées. Les layouts
MPI et StorCLI compilent les champs connus en dur, tandis que `PageLength`
permet les extensions additives. L'historique observé conserve ces offsets et
utilise les champs réservés ou ajoute des champs ; il ne constitue pas une
garantie absolue de Broadcom. Une rupture réelle des offsets imposerait une
nouvelle analyse.

IOC et Board sont deux sondes indépendantes, conformément à la sémantique
hwmon/mpt3sas. UVSS ne publie pas leur maximum et ne synthétise pas une Board
absente ; le consommateur choisit la sonde pertinente pour sa ventilation.

### Politique des températures

Les collecteurs ne décident pas si une température est physiquement plausible.
Ils publient fidèlement une mesure lorsque la source indique qu'elle existe,
que son unité est supportée et que sa valeur peut être décodée et représentée
correctement. Ils la rejettent uniquement lorsque la source ou le protocole la
marque absente ou invalide, lorsque son unité n'est pas supportée, ou lorsque
sa représentation est invalide ou impossible à décoder.

Il ne faut donc pas ajouter de plage arbitraire telle que `0..120 °C`, clamper
les températures, ni supprimer les tests de valeurs négatives ou élevées qui
vérifient le décodage correct d'un protocole.

La sentinelle `0 °C` publiée pendant l'état disque `waking` est volontaire. Elle
évite qu'une indisponibilité transitoire au réveil déclenche le failsafe hwmon à
`100 °C`, puis revienne immédiatement à la température physique, ce qui
provoquerait un yoyo inutile des ventilateurs. Elle ne doit pas être remplacée
par `Unavailable`. Comme `0 °C` peut aussi être une mesure physique, seul l'état
permet de la distinguer des sentinelles `standby` et `waking`.

### Côté Proxmox

Le récepteur :

- valide les snapshots ;
- maintient la topologie hwmon via configfs ;
- pousse chaque température vers le node `/dev/virt-temp/*` de sa sonde ;
- persiste uniquement la topologie, jamais les températures ;
- publie un événement runtime vide après une réconciliation complète qui
  modifie la topologie, ainsi qu'au premier snapshot reçu lorsqu'une topologie
  restaurée depuis le cache est déjà entièrement réconciliée.

Le récepteur ne connaît aucun consommateur et ne communique pas avec le manager
systemd. Une unité `.path` extérieure au processus transforme l'événement
runtime en activation de `unraid-vsock-hwmon-topology.service`, auquel les
consommateurs s'abonnent explicitement.

#### Confinement systemd du receiver

Le receiver reste exécuté par root : il doit manipuler la topologie configfs,
écrire les températures dans les miscdevices `virt-temp` et effectuer le setup
nécessaire au service. Ce besoin ne lui donne pas pour autant un accès root sans
limites. L'unité systemd réduit explicitement son périmètre :

- la seule capability conservée permet le bind du port VSOCK privilégié ;
- les sockets sont limités à `AF_VSOCK` et l'acquisition de nouveaux privilèges
  est interdite ;
- le système de fichiers est protégé, les homes sont masqués et `/tmp` est
  privé ;
- seuls les répertoires d'état et d'exécution gérés par systemd ainsi que le
  chemin configfs de `virt_temp` sont rendus accessibles en écriture ;
- le chargement du module reste isolé dans `ExecStartPre`, avec le préfixe `+`
  qui demande explicitement à systemd cette élévation hors du confinement du
  processus principal.

Ces restrictions forment un contrat de sécurité, pas une collection de réglages
facultatifs. Leur retrait ou leur élargissement exige une justification liée à
un besoin réel et la mise à jour du test statique de l'unité ; la VM conserve la
responsabilité de vérifier que le service fonctionne effectivement sous ce
confinement.

#### Lifetime de l'événement de topologie

`RuntimeDirectoryPreserve=yes` conserve volontairement
`/run/unraid-vsock-sensors/topology-changed` pendant les arrêts et redémarrages
normaux du receiver. L'unité `.path` surveille ce fichier avec `PathChanged` :
le supprimer puis le recréer pendant qu'elle est active pourrait transformer le
lifecycle du service en faux changement de topologie.

Le fichier disparaît naturellement au redémarrage de l'hôte. Lors d'une purge
du paquet, `postrm` doit respecter cet ordre : arrêter le watcher
`unraid-vsock-hwmon-topology.path`, le désactiver, puis seulement supprimer le
runtime directory et son événement. Cette séquence empêche la suppression du
paquet de déclencher un consumer au moment où sa topologie est retirée ; elle
fait partie du contrat de lifecycle.

La collecte et la publication sont indépendantes : une opération disque ou HBA
lente ne bloque pas le heartbeat VSOCK. Le protocole reste un flux JSON
persistant délimité par des retours à la ligne et validé dans
`internal/sensors`. Toute rupture volontaire exige une nouvelle
`ProtocolVersion` et les tests correspondants.

Configfs porte l'existence et les métadonnées des sondes ; les miscdevices
`/dev/virt-temp/*` reçoivent leurs températures runtime. Le receiver est
l'unique writer supporté de configfs pendant son fonctionnement. Les détails de
ce contrat, notamment l'absence volontaire de staging transactionnel côté
noyau, appartiennent à [`virt-temp/README.md`](virt-temp/README.md).

La réconciliation configfs est idempotente et non transactionnelle. Un échec
partiel peut laisser un état kernel intermédiaire, mais
`hwmonInventory.sensors` continue de décrire la dernière topologie entièrement
réconciliée et `needsReconcile` force une nouvelle tentative. `reconfigured`
n'est émis qu'après le retour au succès de toutes les familles : la réussite
d'une famille ne masque pas l'échec de l'autre. À la création d'une sonde, la
température est écrite avant son premier `label`, car ce dernier la rend visible
par hwmon.

Le cache contient uniquement la dernière topologie valide de chaque famille,
jamais les températures. Une reconfiguration réussie fixe `cacheDirty` jusqu'à
ce que cette topologie soit écrite durablement ; si l'écriture échoue, le
prochain snapshot la retente même sans nouveau changement. Cet état de
persistance reste distinct de `needsReconcile`, qui décrit une réconciliation
kernel encore nécessaire.

Le module kernel doit rester simple. La logique métier et la découverte
matérielle appartiennent autant que possible à l'espace utilisateur.

### Diagnostics

Les diagnostics projettent un snapshot runtime en lecture seule. Ils ne
déclenchent jamais SMART, `sdspin`, collecte HBA ni refresh, et passent par les
statuts ou snapshots cohérents des collectors plutôt que par leurs mutex. Les
IDs pouvant contenir des numéros de série, l'avertissement de masquage avant
partage fait partie du contrat utilisateur.

### Failsafe

En cas d'incertitude thermique, le comportement doit rester conservateur.

Quelques règles importantes :

- une erreur d'inventaire ne doit pas publier un inventaire partiel ;
- une topologie précédente valide doit être conservée lors d'une erreur ;
- une détection de bus disque impossible doit inclure le disque en mode `Auto` ;
- une sonde qui n'est plus rafraîchie passe au failsafe kernel ;
- les valeurs physiques ne doivent pas être arbitrairement bornées dans les
  couches qui ne sont pas responsables de leur plausibilité.

Éviter d'ajouter des abstractions génériques lorsque les collecteurs ont des
contraintes réellement différentes.

### Compatibilité de mise à jour

Certains éléments qui ressemblent à du code mort restent nécessaires aux mises
à jour :

- `HBA.Temp` conserve la projection historique à une seule sonde du protocole ;
- les IDs `serial:` restent acceptés lors de la lecture des anciens caches ;
- l'ancienne clé `HBA_INTERVAL` reste tolérée mais ignorée par le script rc.

Ne pas les supprimer sans migration explicite garantissant que les anciennes
configurations, snapshots et caches ne sont plus rencontrés.

## Formatage et conventions

Le code Go doit être formaté avec :

```sh
make fmt
```

Les fichiers texte du dépôt utilisent LF.

Les scripts doivent passer ShellCheck lorsqu'il s'applique.

Préférer des erreurs contenant suffisamment de contexte pour permettre un
diagnostic sans journalisation supplémentaire.

Les erreurs répétitives de collecte doivent éviter de produire du spam dans les
logs lorsqu'un mécanisme de transition ou de type `sticky error` existe déjà.

## Tests lors d'une modification

Une modification locale classique devrait au minimum passer :

```sh
make check
make test-race
```

Une modification touchant l'une des zones suivantes devrait également passer
les tests VM concernés :

- `virt-temp`;
- publication hwmon ;
- cache de topologie ;
- paquet Debian ;
- DKMS ;
- service systemd ;
- intégration avec de vrais block devices Linux.

Avant une release complète :

```sh
make test-vm
```

## Construction des artefacts

Construire tous les artefacts :

```sh
make artifacts
```

Avec une version explicite :

```sh
make artifacts VERSION=2.0.0
```

Une prerelease est également acceptée :

```sh
make artifacts VERSION=2.0.0-rc.1
```

Les artefacts sont créés dans :

```text
dist/
```

Le projet cible Linux amd64.

### Paquet Proxmox

Construire uniquement le paquet Debian avec :

```sh
make hwmon-package
```

`virt-temp/package.sh` prépare une arborescence temporaire puis appelle
`dpkg-buildpackage` et debhelper 13. `dh_installsystemd` installe et active le
receiver et l'unité `.path` sans arrêter le service avant une mise à jour. Les
scripts de maintenance gèrent DKMS explicitement : le `prerm` généré par
`dh_dkms` retirerait sinon l'ancienne version avant que la nouvelle ait prouvé
qu'elle peut être construite et installée.

L'upgrade conserve donc l'ancienne version DKMS enregistrée et ses sources de
secours jusqu'à la validation complète du remplacement. Son ordre est
volontaire :

1. construire la nouvelle version pour tous les kernels ciblés ;
2. l'installer pour tous ces kernels ;
3. seulement ensuite arrêter le watcher de topologie et le receiver ;
4. décharger l'ancien module et charger le nouveau ;
5. redémarrer le receiver lorsqu'il était actif ou reste activé ;
6. vérifier que l'interface configfs `virt_temp` existe ;
7. supprimer les anciennes versions DKMS et leurs sources préservées.

Construire tous les kernels avant la première installation évite un état où le
nouveau module remplace déjà l'ancien pour certains kernels alors qu'il est
inconstructible pour un autre. Comme le build et l'installation précèdent
l'arrêt du service, leur échec laisse l'ancien module chargé et ses sources DKMS
disponibles. Les anciens enregistrements ne sont retirés qu'après la preuve que
le remplacement expose configfs, afin de rester disponibles pour une réparation
ou une suppression propre jusque-là. Cette garantie est la raison pour laquelle
le lifecycle ne délègue pas directement le retrait de l'ancienne version au
`prerm` standard de `dh_dkms`.

#### FD externe pendant un upgrade

Un processus extérieur qui conserve un FD `/dev/virt-temp/*` pendant un upgrade
est hors du contrat supporté. Les `file_operations` du miscdevice appartiennent
à `THIS_MODULE` : même après la suppression configfs de la sonde et le passage
des écritures à `ENODEV`, le FD ouvert retient le module et peut faire échouer
`modprobe -r virt_temp`. Le receiver reste l'unique writer supporté en
fonctionnement normal ; les manipulations manuelles exigent son arrêt et la
fermeture des descripteurs avant un upgrade.

Le chemin `remove` restaure le service après un échec de déchargement afin de
laisser le paquet utilisable, mais cette récupération ne constitue pas une
garantie équivalente pour l'upgrade. Le `postinst` n'ajoute pas de rollback
spéculatif : après fermeture du FD, la configuration interrompue peut reprendre
avec `dpkg --configure -a`. Supporter ce scénario demanderait d'abord un test VM
d'upgrade réel avec un FD externe ouvert, puis un rollback minimal fondé sur le
comportement observé de dpkg, systemd, configfs et DKMS.

Une construction directe avec `dpkg-buildpackage -b -us -uc` reste possible
quand `debian/changelog` porte la version voulue. Sous Debian 13, installer Go
1.27 séparément ; le script de projet utilise ce Go local et passe `-d` pour ne
pas bloquer sur les `Build-Depends` Go indisponibles dans la distribution.

Pour choisir la version ou la révision Debian :

```sh
make hwmon-package VERSION=X.Y.Z
make hwmon-package VERSION=X.Y.Z DEBIAN_REVISION=2
```

Le résultat est écrit dans
`dist/unraid-vsock-sensors-hwmon_X.Y.Z-N_amd64.deb`.

## Versionnement

Les versions suivent SemVer.

Exemples :

```text
2.0.0
2.0.0-rc.1
```

Le préfixe `v` n'est pas fourni à `make`.

Le paquet Debian convertit une prerelease SemVer afin de préserver l'ordre
Debian, par exemple :

```text
2.0.0-rc.1
→ 2.0.0~rc.1-1
```

Une correction limitée au paquet Debian peut augmenter uniquement sa révision :

```sh
make hwmon-package VERSION=2.0.0 DEBIAN_REVISION=2
```

## Tester un paquet Unraid de développement

Sur une installation où le plugin existe déjà :

```sh
make unraid-package
scp dist/<paquet>.txz root@NAS:/tmp/
```

Puis sur Unraid :

```sh
/etc/rc.d/rc.unraid-vsock-sensors stop
upgradepkg --install-new --reinstall /tmp/<paquet>.txz
/etc/rc.d/rc.unraid-vsock-sensors start
```

Installer uniquement le `.txz` ne crée pas une installation persistante du
plugin. Une première installation complète doit passer par un descripteur
`.plg`.

## Préparer une release

Depuis un worktree propre :

```sh
make release VERSION=X.Y.Z
```

Cette commande :

- valide la version ;
- exécute `make check` ;
- exécute `make test-race` ;
- exécute les tests VM ;
- construit les artefacts ;
- actualise le descripteur `.plg`.

Elle ne crée ni commit, ni tag et ne pousse rien.

Vérifier ensuite le changement du `.plg` :

```sh
git diff -- unraid-plugin/unraid-vsock-sensors.plg
```

Puis :

```sh
git add unraid-plugin/unraid-vsock-sensors.plg
git commit -m "Publie le descripteur Unraid X.Y.Z"
git tag -a vX.Y.Z -m "Release vX.Y.Z"
```

## Publication GitHub

Le remote `github` correspond au dépôt public distribuant le plugin.

`origin` correspond au dépôt Gitea de développement.

Publier d'abord le tag :

```sh
git push github vX.Y.Z
```

Créer ensuite la GitHub Release et y téléverser :

```text
dist/unraid-vsock-sensors-X.Y.Z-x86_64-1.txz
dist/unraid-vsock-sensors-hwmon_X.Y.Z-1_amd64.deb
```

Vérifier que les assets sont accessibles :

```sh
curl -fIL https://github.com/milouz1985/unraid-vsock-sensors/releases/download/vX.Y.Z/unraid-vsock-sensors-X.Y.Z-x86_64-1.txz

curl -fIL https://github.com/milouz1985/unraid-vsock-sensors/releases/download/vX.Y.Z/unraid-vsock-sensors-hwmon_X.Y.Z-1_amd64.deb
```

Ne pousser `main` sur GitHub qu'après cette vérification : le `.plg` publié sur
`main` référence directement le `.txz` de la release.

Enfin :

```sh
git push github main
git push origin main vX.Y.Z
```

## Commits

Les commits doivent rester ciblés et décrire l'intention du changement.

Exemples :

```text
fix(disks): rejeter les IDs dupliqués dans chaque source Unraid
refactor(hba): fixer l'intervalle selon le backend
refactor(unraid): simplifier le bootstrap de la page
docs: alléger et dédupliquer les README
```

Éviter de mélanger refactor, changement fonctionnel et documentation sans lien
dans un même commit.

# unraid-vsock-sensors

`unraid-vsock-sensors` transmet les températures des disques et des contrôleurs
HBA d'une VM Unraid vers son hôte Proxmox via `AF_VSOCK`, sans réseau IP entre
les deux systèmes.

Côté Unraid, l'agent réutilise les températures déjà collectées par `emhttpd`.
En fonctionnement normal, il lit les températures déjà collectées par Unraid.
Si le polling SMART d'Unraid s'interrompt, un fallback direct et borné prend
temporairement le relais sans interroger les HDD en veille.

Côté Proxmox, les températures sont exposées comme sondes Linux `hwmon`
utilisables notamment par CoolerControl, fan2go, fancontrol ou lm-sensors.

## Architecture

```text
VM Unraid                                      Hôte Proxmox
┌────────────────────────────┐                 ┌─────────────────────────────┐
│ emhttpd                    │                 │ unraid-vsock-sensors hwmon  │
│  └─ disks.ini, devs.ini    │                 │            │                │
│ /dev/mpt3ctl ou StorCLI    │     AF_VSOCK    │ /dev/virt-temp              │
│            │               │                 │            │                │
│ unraid-vsock-sensors serve ├────────────────►│            ▼                │
└────────────────────────────┘                 │ sondes hwmon natives        │
                                               └─────────────────────────────┘
```

Le même binaire fournit :

- `serve` côté Unraid ;
- `hwmon` côté Proxmox.

Le récepteur vérifie le CID de la VM et la version du protocole. Les exemples
utilisent CID `3` et port `990`.

### Socket de contrôle

Le daemon `serve` écoute une API HTTP locale sur une socket Unix, par défaut :

```text
/run/unraid-vsock-sensors/control.sock
```

Elle est le canal local des opérations explicites de contrôle : la WebUI et la
CLI y accèdent pour l'inventaire de gestion, les disk policies et le refresh
manuel. Endpoints :

- `GET /v1/disks` : inventaire de gestion (ID, nom, device, transport, bus,
  politique, inclusion). Il est lu à la demande depuis le fichier de
  politiques et les fichiers d'inventaire Unraid, avec `validateIncluded=false`
  : un disque incomplet ou invalide reste visible afin de pouvoir être exclu.
  Un fichier `disk-policies.json` invalide est signalé en `422`.
- `GET /v1/policies` : valide uniquement le fichier de politiques persisté,
  indépendamment de l'état thermique du collecteur ;
- `PUT /v1/disk-policy` : enregistre une politique (`auto`/`include`/`exclude`)
  pour un ID stable, persiste puis déclenche une actualisation ;
- `DELETE /v1/disk-policies` : supprime toutes les politiques et actualise ;
- `POST /v1/refresh` : demande une actualisation de la collecte disque.

Le heartbeat emhttpd `poll_attributes` n'est pas transporté par cette socket :
il est fréquent et ne porte aucune donnée, il est délivré hors bande en
`SIGUSR2` (hook Unraid → `rc … poll` → `SIGUSR2` → daemon), qui enregistre le
heartbeat et demande une actualisation.

La socket suit le cycle de vie du daemon. Au démarrage, une socket résiduelle
n'est supprimée que si un probe `ECONNREFUSED` prouve qu'aucun processus ne
l'écoute ; un timeout ou une erreur de permission la laisse en place. Le
`net.UnixListener` est configuré avec `SetUnlinkOnClose` : un shutdown propre
supprime le pathname lors de la fermeture du listener, tandis qu'un crash
brutal peut laisser une socket stale que `prepareSocket` détecte au démarrage
suivant via `ECONNREFUSED`. Le shutdown du control plane est synchrone avant le
retour de `serve`, et une erreur inattendue de `http.Server.Serve` arrête le
daemon plutôt que de laisser un processus sans control plane.

La CLI `disks …` est un client de cette socket ; si le daemon n'est pas lancé,
elle échoue avec `unraid-vsock-sensors daemon is not running`.

## Installation

### 1. Ajouter AF_VSOCK à la VM Unraid

Dans `/etc/pve/qemu-server/<VMID>.conf` :

```text
args: -device vhost-vsock-pci,guest-cid=3
```

Si une ligne `args:` existe déjà, ajouter uniquement l'option `-device`.

Le CID doit être unique parmi les VM actives. Arrêter puis redémarrer
complètement la VM après modification.

### 2. Installer le plugin Unraid

Dans **Plugins → Install Plugin** :

```text
https://raw.githubusercontent.com/milouz1985/unraid-vsock-sensors/refs/heads/main/unraid-plugin/unraid-vsock-sensors.plg
```

La configuration se trouve dans **Settings → Unraid VSOCK Sensors**.

Paramètres principaux :

- **VSOCK port** : `990` ;
- **HBA monitoring** : `enabled` ou `disabled` ;
- **HBA backend** : `Native /dev/mpt3ctl` ou `StorCLI` ;
- **HBA refresh interval** : `15s` par défaut avec `mpt3ctl`, `30s` avec
  StorCLI.

Le plugin propose :

- `mpt3ctl` : `Default`, `10s`, `15s`, `30s`, `1m` ;
- StorCLI : `Default`, `30s`, `1m`, `5m`.

StorCLI utilise volontairement un intervalle plus long car sa collecte est plus
coûteuse que l'accès ioctl natif MPT3.

La page **Diagnostics**, accessible depuis la page du plugin, affiche en lecture
seule l'état réel du daemon. La commande `unraid-vsock-sensors diagnostics`
renvoie le même instantané JSON local. Elle ne déclenche aucune collecte SMART.
Un heartbeat emhttpd stale active temporairement le fallback SMART direct ; cela
n'indique pas nécessairement une panne de disque. Un lien VSOCK déconnecté
signifie que le récepteur Proxmox n'est pas joignable. Les températures des
disques en veille peuvent naturellement être indisponibles. Les IDs disque du
diagnostic peuvent contenir des numéros de série : les masquer avant partage.

Le backend `mpt3ctl` ne nécessite aucun utilitaire externe mais reste
expérimental. Il est utilisé par l'auteur avec un LSI SAS3008.

StorCLI nécessite la commande `storcli`, disponible notamment via le plugin
Unraid `storcli64`.

Vérification :

```sh
/etc/rc.d/rc.unraid-vsock-sensors status
/usr/local/sbin/unraid-vsock-sensors version
grep 'unraid-vsock-sensors' /var/log/syslog | tail -n 50
```

### 3. Installer le paquet Proxmox

```sh
apt update
apt install "proxmox-headers-$(uname -r)" \
  ./unraid-vsock-sensors-hwmon_X.Y.Z-1_amd64.deb
```

Le paquet installe le récepteur, le module DKMS `virt-temp` et le service
`unraid-vsock-hwmon.service`.

## Configuration Proxmox

Configuration par défaut :

```sh
UNRAID_VSOCK_CID=3
UNRAID_VSOCK_PORT=990
UNRAID_VSOCK_CACHE=/var/lib/unraid-vsock-sensors/hwmon-inventory.json
# UNRAID_VSOCK_RESTART_UNITS=coolercontrold.service
```

Fichier :

```text
/etc/default/unraid-vsock-hwmon
```

Pour relancer automatiquement des consommateurs hwmon après une modification
de topologie :

```sh
UNRAID_VSOCK_RESTART_UNITS=coolercontrold.service,fan2go.service
```

Seules les unités déjà actives sont relancées.

Après modification :

```sh
systemctl restart unraid-vsock-hwmon.service
```

## Vérification

Sur Proxmox :

```sh
dkms status -m virt-temp
systemctl status unraid-vsock-hwmon.service
journalctl -u unraid-vsock-hwmon.service -n 50 --no-pager
ls -l /dev/virt-temp
sensors
```

Des sondes telles que celles-ci doivent apparaître :

```text
unraid_disk1
unraid_hdd_maximum
unraid_sas3008
```

## Collecte des disques

Les disques assignés proviennent de :

```text
/var/local/emhttp/disks.ini
```

Les unités non assignées proviennent de :

```text
/var/local/emhttp/devs.ini
```

`disks.ini` reste prioritaire lorsqu'un même ID apparaît dans les deux sources.
Les copies de cet ID dans `devs.ini` sont ignorées avant toute validation de
leurs champs ou de leurs doublons.

Deux entrées actives partageant le même ID dans une même source invalident
l'inventaire : aucun inventaire partiel n'est publié.

L'identité repose sur l'ID stable fourni par Unraid, jamais sur `/dev/sdX`.

L'emhttpd analysé tronque ces IDs à 79 octets. Cette limite observée n'est pas
une API Unraid garantie.

### Disques USB

En mode `Auto`, UVSS inspecte la topologie sysfs.

Un disque physiquement derrière USB est exclu même si emhttpd le présente
comme `transport=ata`.

Si le bus ne peut pas être déterminé, le disque est inclus par sécurité
thermique.

La clé de démarrage `flash` est toujours exclue.

L'interface permet trois politiques par ID stable :

- `Auto` ;
- `Include` ;
- `Exclude`.

Les overrides sont conservés dans :

```text
/boot/config/plugins/unraid-vsock-sensors/disk-policies.json
```

Le daemon `unraid-vsock-sensors serve` est le seul processus autorisé à écrire
ce fichier. La CLI et l'interface Web y accèdent via la socket de contrôle
locale du daemon (voir « Socket de contrôle ») ; les mutations y sont
sérialisées par un verrou intra-processus et l'écriture reste atomique
(fichier temporaire + `rename`).

Un fichier invalide est ignoré par le daemon : tous les disques repassent alors
en `Auto` et l'interface signale l'erreur avec un bouton de réinitialisation.

Les sous-commandes `disks list`, `disks set`, `disks validate`, `disks reset`
et `disks refresh` sont des clients de la socket de contrôle ; elles acceptent
`--control-socket` pour cibler une autre socket (tests). `disks set` prend
`--id-base64` (ID stable encodé en base64) et `--policy`. Le heartbeat
`poll_attributes` n'est pas une commande `disks` : il est délivré en `SIGUSR2`.

### Source de température

UVSS consomme les champs `temp` et `spundown` déjà maintenus par `emhttpd`.
Lorsque le heartbeat `poll_attributes` est sain, Unraid est l'autorité pour
ces deux champs. UVSS ne cherche pas à déterminer indépendamment l'âge physique
de la mesure exposée par emhttpd.

Le champ `spundown` a toujours la priorité sur `temp` :
- `spundown=1` → état `standby`, température synthétique 0 ;
- `spundown=0` + `temp` numérique fini → mesure valide ;
- `spundown=0` + `temp` indisponible/invalide → `waking` si le polling est actif
  et l'état précédent était `standby`, `unavailable` sinon.

L'événement Unraid `poll_attributes` envoie `SIGUSR2` au daemon (via
`rc … poll`), qui enregistre le heartbeat et demande une actualisation
immédiate. Ce signal est privilégié au passage par la socket de contrôle parce
que le heartbeat est fréquent et ne porte aucune donnée : il évite de lancer un
processus par événement. Un watchdog de cinq secondes couvre les événements
perdus et les changements d'état.

Si aucun événement `poll_attributes` n'arrive pendant
`poll_attributes + 15 secondes` (45 s avec le réglage 30 s), UVSS interroge
temporairement les disques via `smartctl_type` avec `-n standby,3`. Pour les HDD
ATA, il vérifie d'abord l'état avec `sdspin` et n'interroge que les disques
actifs ; les HDD d'un autre bus ou de type inconnu restent indisponibles par
prudence. La première collecte directe démarre dès la détection du blocage ;
les suivantes suivent l'intervalle `poll_attributes`, même si le watchdog
continue de vérifier l'état toutes les cinq secondes. Entre deux collectes,
UVSS conserve la dernière mesure sans relancer SMART. Les commandes ont un
timeout et une concurrence bornée. Le premier nouvel événement
`poll_attributes` rétablit aussitôt la source native.

Si une lecture directe échoue, UVSS marque la température indisponible au lieu
de republier une ancienne mesure. Le failsafe hwmon de 10 secondes peut alors
prendre le relais. `poll_attributes=0` désactive ce fallback, puisqu'aucun
événement périodique n'est attendu. Dans ce mode, les disques actifs sont
marqués indisponibles car leur température ne peut pas être considérée comme
fraîche ; les disques en veille restent en `standby` avec la sentinelle
synthétique.

Lorsqu'un disque est en état `standby`, UVSS le conserve dans l'inventaire avec
`Temp=0` comme sentinelle synthétique de contrôle. Cette valeur ne représente
alors pas une température physique ; une vraie mesure SMART à `0 °C` reste
valide. Les diagnostics utilisent l'état thermique interne pour afficher la
sentinelle comme `standby`, sans température courante. Après un réveil observé
sans interruption de visibilité sur l'inventaire, UVSS conserve temporairement
cette sentinelle, avec l'état `waking`, pendant qu'il attend la première
température emhttpd valide. UVSS ne conserve ni ne republie lui-même sa mesure
pré-standby pendant le réveil. Tant qu'emhttpd n'expose pas de température
numérique, UVSS publie la sentinelle `waking`. Dès qu'emhttpd fournit une valeur
numérique finie, elle fait autorité, même si elle est identique à celle observée
avant la veille ; UVSS ne peut pas établir l'âge physique de cette mesure.
Cette attente est limitée à :

```text
poll_attributes + 5 secondes
```

La première observation numérique valide remplace immédiatement la sentinelle.
Si elle n'arrive pas avant l'expiration de cette fenêtre, le disque devient
`Unavailable` et le failsafe hwmon peut prendre le relais. Une observation
invalide d'un disque actif sans transition préalable depuis `standby` devient
indisponible immédiatement. Une erreur d'inventaire casse la continuité : au
retour, aucune wake grace n'est accordée à un disque actif sans mesure valide.
Une nouvelle mesure valide ou un nouveau standby explicitement observé rétablit
normalement un état disponible.

`poll_attributes` est lu dans `/var/local/emhttp/var.ini`. Après une lecture
valide, une erreur transitoire conserve le dernier intervalle connu pour la
détection du polling bloqué tout en restant visible dans les diagnostics. Tant
qu'aucune valeur valide n'a encore été lue depuis le démarrage, UVSS utilise un
défaut interne de `30s`. La valeur valide `0` est conservée comme telle et ne
peut pas être confondue avec l'absence de configuration. UVSS ne modifie pas la
configuration Unraid.

## Contrôleurs HBA

Deux backends sont disponibles :

- `mpt3ctl` : requêtes MPI CONFIG en lecture seule via `/dev/mpt3ctl` ;
- StorCLI : température ROC issue de sa sortie JSON.

Le backend natif récupère également le modèle, l'adresse SAS et l'adresse PCI
lorsqu'elles sont disponibles.

StorCLI redécouvre l'identité des contrôleurs après une erreur ou un changement
de l'ensemble de leurs index. Un remplacement ou une reconfiguration à chaud
qui conserve les mêmes index peut nécessiter un redémarrage d'UVSS pour
redécouvrir l'identité du matériel.

Une collecte HBA possède un deadline de 15 secondes. Un ioctl natif pouvant
rester bloqué au-delà de ce délai, le dernier snapshot valide expire
indépendamment et tout résultat revenu trop tard est rejeté.

## Topologie et failsafe

Chaque sonde possède son propre périphérique hwmon et reste donc toujours
`temp1`.

L'identité stable repose sur :

- l'ID Unraid pour un disque ;
- l'adresse SAS, PCI ou le numéro de série pour un HBA.

Les noms `hwmonX`, `/dev/sdX` et les index locaux des contrôleurs ne sont pas
utilisés comme identité.

La topologie est persistée dans `UNRAID_VSOCK_CACHE` et restaurée au démarrage
avec la température failsafe.

Un snapshot valide, même vide, est autoritaire. Un snapshot en erreur conserve
au contraire la topologie précédente.

Après dix secondes sans mise à jour, une sonde `virt_temp` passe à :

```text
100 °C
```

Une nouvelle donnée valide rétablit automatiquement la température.

Les détails du module et du protocole `/dev/virt-temp` sont documentés dans
[`virt-temp/README.md`](virt-temp/README.md).

## Mise à jour et désinstallation

Une mise à jour du plugin conserve sa configuration et
`disk-policies.json`.

Une désinstallation explicite les supprime.

Sur Proxmox :

```sh
apt install ./unraid-vsock-sensors-hwmon_X.Y.Z-N_amd64.deb
```

Conserver la configuration :

```sh
apt remove unraid-vsock-sensors-hwmon
```

Tout supprimer :

```sh
apt purge unraid-vsock-sensors-hwmon
```

## Développement

Prérequis :

- Go 1.27.0 ou plus récent ;
- ShellCheck.

Commandes principales :

```sh
make check
make test-race
make fuzz-mpt3
make artifacts
make test-vm
```

Les tests d'intégration Proxmox sont documentés dans
[`tests/vm/README.md`](tests/vm/README.md).

Pour préparer une release :

```sh
make release VERSION=X.Y.Z
```

Cette commande exécute les vérifications, les tests VM, construit les artefacts
et actualise le `.plg`. Elle ne crée ni commit, ni tag et ne pousse rien.

Publier d'abord le tag et les assets GitHub, vérifier leurs URLs, puis seulement
pousser le nouveau `main` contenant le `.plg`.

## Sécurité

AF_VSOCK n'est pas un mécanisme général d'authentification.

L'agent se connecte au CID hôte standard `2`. Le récepteur n'accepte que le CID
configuré, vérifie la version du protocole et limite chaque snapshot à 1 Mio.

## Licence

Le programme Go, les scripts et l'interface Unraid sont sous
`GPL-3.0-or-later`.

Le module `virt-temp` est sous `GPL-2.0-only`.

Voir [`THIRD_PARTY_NOTICES.md`](THIRD_PARTY_NOTICES.md) pour les dépendances et
attributions tierces.

## Développement assisté par IA

Une part importante du code et de la documentation a été produite avec
l'assistance d'une IA. Les changements sont néanmoins relus et les chemins
critiques sont testés.

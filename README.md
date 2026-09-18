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

Le daemon `serve` expose à la WebUI une petite API HTTP locale sur :

```text
/run/unraid-vsock-sensors/control.sock
```

PHP parle directement à cette socket : aucun processus CLI intermédiaire n'est
lancé pour afficher ou modifier les politiques disque. L'API est volontairement
limitée à trois routes :

- `GET /v1/disks` : inventaire de gestion (ID, nom, device, transport, bus,
  politique, sélection et éligibilité). `selected` représente le résultat des
  règles de sélection, `eligible` la validité des champs nécessaires au
  collecteur thermique, et `validation_error` explique une entrée non éligible ;
- `PUT /v1/disk-policy` : enregistre une politique (`auto`/`include`/`exclude`)
  pour un ID stable puis demande une actualisation de la collecte ;
- `DELETE /v1/disk-policies` : supprime tous les overrides puis actualise.

Le heartbeat emhttpd `poll_attributes` reste hors bande : le hook Unraid appelle
`rc … poll`, qui envoie `SIGUSR2` au daemon sans passer par HTTP.

Au démarrage, une socket résiduelle n'est supprimée que si `ECONNREFUSED`
prouve qu'aucun processus ne l'écoute. Un shutdown propre ferme le serveur de
façon synchrone. Une erreur ultérieure du listener est journalisée sans arrêter
la collecte thermique ni la publication VSOCK ; un restart du service recrée
alors le control plane.

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
- **HBA backend** : `Native /dev/mpt3ctl` ou `StorCLI`.

L'intervalle de collecte HBA est fixé automatiquement à `15s` avec `mpt3ctl`
et à `30s` avec StorCLI. StorCLI utilise volontairement un intervalle plus long
car sa collecte est plus coûteuse que l'accès ioctl natif MPT3.

La page **Diagnostics**, accessible depuis la page du plugin, lit en lecture
seule l'instantané JSON publié par le daemon. Elle ne déclenche aucune collecte
SMART. Un heartbeat emhttpd stale active temporairement le fallback SMART direct ; cela
n'indique pas nécessairement une panne de disque. Un lien VSOCK déconnecté
signifie que le récepteur Proxmox n'est pas joignable. Les disques en veille restent présents avec une sentinelle synthétique à `0 °C` ;
les diagnostics l’affichent comme un état `standby` sans température physique. Les
IDs disque du diagnostic peuvent contenir des numéros de série : les masquer avant partage.

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
ce fichier. La WebUI lui envoie les mutations directement via la socket de
contrôle ; elles sont sérialisées par le store en mémoire et l'écriture reste
atomique (fichier temporaire + `rename`).

Si le fichier devient invalide après une lecture valide, le collecteur conserve
les dernières politiques valides et expose l'erreur dans les diagnostics. Au
premier démarrage, tant qu'aucune configuration valide n'a été lue, le fallback
reste `Auto`. La WebUI permet de réinitialiser le fichier invalide.

Le heartbeat `poll_attributes` est indépendant de cette API et reste délivré en
`SIGUSR2`.

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
client Go ou une requête HTTP locale supplémentaire à chaque événement. Un watchdog de cinq secondes couvre les événements
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

Une mise à jour comme une désinstallation du plugin conserve sa configuration et
`disk-policies.json` sous `/boot/config/plugins/unraid-vsock-sensors`. Supprimer
manuellement ce répertoire pour effectuer une purge complète.

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

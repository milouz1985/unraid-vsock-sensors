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
│  └─ disks.ini, devs.ini    │                 │       │              │      │
│ /dev/mpt3ctl ou StorCLI    │     AF_VSOCK    │       ▼              ▼      │
│            │               │                 │ configfs + /dev   événement │
│ unraid-vsock-sensors serve ├────────────────►│       │           systemd   │
└────────────────────────────┘                 │       ▼              │      │
                                               │ sondes hwmon    abonnements │
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
prouve qu'aucun processus ne l'écoute. Une socket déjà utilisée par une autre
instance empêche le démarrage ; toute autre erreur initiale est journalisée sans
bloquer la collecte thermique ni la publication VSOCK. Un shutdown ferme le
listener puis laisse jusqu'à sept secondes aux requêtes acceptées pour se terminer ;
une expiration est journalisée et une mutation encore active peut rester non
appliquée. Une erreur ultérieure du listener reste elle aussi isolée du data
plane ; un restart du service recrée alors le control plane.

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

Le paquet installe le récepteur, le module DKMS `virt-temp`, le service
`unraid-vsock-hwmon.service` et les unités systemd de notification de
topologie. Il dépend de `proxmox-default-headers` pour reconstruire le module
lors des mises à jour de noyau. La configuration existante dans
`/etc/default/unraid-vsock-hwmon` est conservée pendant les mises à jour.

## Configuration Proxmox

Configuration par défaut :

```sh
UNRAID_VSOCK_CID=3
UNRAID_VSOCK_PORT=990
UNRAID_VSOCK_CACHE=/var/lib/unraid-vsock-sensors/hwmon-inventory.json
```

Fichier :

```text
/etc/default/unraid-vsock-hwmon
```

### Abonnement aux changements de topologie

Après une réconciliation complète qui modifie la topologie hwmon, le récepteur
modifie le fichier vide suivant :

```text
/run/unraid-vsock-sensors/topology-changed
```

L'événement est également émis au premier snapshot reçu de la VM lorsqu'une
topologie a été restaurée depuis le cache, même si aucune sonde n'est ajoutée,
retirée ou renommée. Il reste différé tant qu'une famille nécessite encore une
réconciliation.

`unraid-vsock-hwmon-topology.path` convertit cet événement en activation de
`unraid-vsock-hwmon-topology.service`. Le récepteur ne communique pas avec
systemd et ne connaît aucun consommateur.

Le fichier d'événement ne contient ni commande ni donnée de sonde. Il est
conservé pendant les arrêts et redémarrages du receiver afin que sa suppression
ne soit jamais interprétée comme un changement de topologie, puis disparaît
naturellement au redémarrage de l'hôte ou lors de la purge du paquet.

Lorsqu'un service doit simplement être redémarré s'il est déjà actif, activer
une instance du template générique. L'instance est le nom simple du service,
sans suffixe de type. Pour un service fictif `foo.service` :

```sh
systemctl enable unraid-vsock-hwmon-restart@foo.service
```

L'instance `unraid-vsock-hwmon-restart@foo.service` exécute
`systemctl try-restart --no-block -- foo.service` : un service inactif n'est
pas démarré. Ce template vise les services classiques non instanciés.
Désabonnement :

```sh
systemctl disable unraid-vsock-hwmon-restart@foo.service
```

Un logiciel peut aussi fournir sa propre unité oneshot pour effectuer un
reload, rescan ou refresh, puis déclarer :

```ini
[Unit]
Description=Refresh foo when the hwmon topology changes

[Service]
Type=oneshot
ExecStart=/usr/bin/foo --rescan

[Install]
WantedBy=unraid-vsock-hwmon-topology.service
```

L'abonnement est toujours une décision explicite de l'administrateur ou du
logiciel concerné. Le paquet ne détecte aucun consommateur.

`UNRAID_VSOCK_RESTART_UNITS` n'est plus pris en charge. Une ligne existante dans
`/etc/default/unraid-vsock-hwmon` est conservée mais reste sans effet : le
récepteur ne contrôle plus les unités consommatrices. Après une mise à jour,
activer explicitement chaque abonnement nécessaire, par exemple :

```sh
systemctl enable unraid-vsock-hwmon-restart@coolercontrold.service
```

CoolerControl n'est ici qu'un exemple, pas un consommateur géré par UVSS. Le
template utilise `try-restart` : lors d'un événement, une unité inactive reste
inactive.

Après une modification du CID, du port ou du chemin de cache :

```sh
systemctl restart unraid-vsock-hwmon.service
```

Si `UNRAID_VSOCK_CACHE` désigne un répertoire extérieur au chemin par défaut,
l'autoriser également dans un drop-in systemd :

```ini
[Service]
ReadWritePaths=/chemin/du/cache
```

Puis exécuter `systemctl daemon-reload` et redémarrer le service.

## Vérification

Sur Proxmox :

```sh
dpkg -s unraid-vsock-sensors-hwmon
dkms status -m virt-temp
systemctl status unraid-vsock-hwmon.service
systemctl status unraid-vsock-hwmon-topology.path
journalctl -u unraid-vsock-hwmon.service -n 50 --no-pager
find /dev/virt-temp -maxdepth 1 -type c -ls
sensors
```

`dpkg -s` révèle notamment un paquet `half-configured`. L'état de l'unité
`.path` permet de diagnostiquer séparément le watcher des changements de
topologie.

Des sondes telles que celles-ci doivent apparaître :

```text
unraid_disk1
unraid_hdd_maximum
unraid_sas3008
```

### Récupération du paquet Proxmox

Si les headers du noyau courant manquent :

```sh
apt install "proxmox-headers-$(uname -r)"
apt --fix-broken install
```

Après une installation interrompue :

```sh
dpkg --configure -a
apt --fix-broken install
```

Lors d'un échec de compilation pendant une mise à jour, le paquet reste à
réparer mais l'ancienne version DKMS et ses sources sont conservées sur disque
pour le prochain démarrage. Les fichiers userspace de l'ancien paquet ne sont
pas restaurés. Après correction de la cause, terminer avec
`apt --fix-broken install`.

Le downgrade en place d'une version 3.x vers une version 2.x antérieure à
l'interface configfs n'est pas supporté et peut laisser le paquet dans un état
half-configured. Dans cet état, `apt remove` retire toutes les versions DKMS et
leurs sources de secours, tout en conservant la configuration et le cache.

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

Une entrée sélectionnée doit également être éligible à la collecte thermique.
Une identité stable ou des champs `rotational`/`spundown` invalides sur une
entrée sélectionnée invalident l'inventaire thermique plutôt que de publier une
vue partielle. L'entrée reste visible dans l'inventaire de gestion pour
diagnostiquer le problème et, si son ID stable est valide, la placer en
`Exclude`.

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
que le heartbeat est fréquent et ne porte aucune donnée : il évite une requête
HTTP locale supplémentaire à chaque événement. Un watchdog de cinq secondes
couvre les événements perdus et les changements d'état.

Le heartbeat est considéré stale lorsque son absence dépasse
`poll_attributes + 15 secondes`. En l'absence d'un nouvel événement, le
watchdog de cinq secondes détecte ce dépassement au refresh suivant. UVSS
interroge alors temporairement les disques via `smartctl_type` avec
`-n standby,3`. Pour les HDD ATA, il vérifie d'abord l'état avec `sdspin` et
n'interroge que les disques actifs ; les HDD d'un autre bus ou de type inconnu
restent indisponibles par prudence. La première collecte directe démarre dès la
détection du blocage ;
les suivantes suivent l'intervalle `poll_attributes`, même si le watchdog
continue de vérifier l'état toutes les cinq secondes. Entre deux collectes,
UVSS conserve la dernière mesure sans relancer SMART. Les commandes ont un
timeout et une concurrence bornée. Le premier nouvel événement
`poll_attributes` rétablit aussitôt la source native.

Si une lecture directe échoue, UVSS marque la température indisponible au lieu
de republier une ancienne mesure. Le failsafe hwmon, configuré à 10 secondes
par défaut, peut alors prendre le relais. `poll_attributes=0` désactive ce
fallback, puisqu'aucun événement périodique n'est attendu. Dans ce mode, les
disques actifs sont marqués indisponibles car leur température ne peut pas être
considérée comme fraîche ; les disques en veille restent en `standby` avec la
sentinelle synthétique.

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
- StorCLI : température ROC et, lorsqu'elle existe, température du contrôleur
  issues de sa sortie JSON.

Un HBA peut exposer deux sondes distinctes : `IOC` et `Board`. Le backend
`mpt3ctl` suit les unités et indicateurs de présence de la page IO Unit 7 ;
StorCLI associe `ROC temperature` à IOC et `Ctrl temperature` (ou
`Controller temperature`) à Board. Une sonde absente ou dont l'unité n'est pas
supportée n'est pas créée. L'identité du contrôleur reste commune aux deux
sondes, dont les labels indiquent explicitement le type.

Pour les deux backends, l'identité vient de `/sys/class/scsi_host` : UVSS relie
le contrôleur à son adresse PCI, puis lit `host_sas_address` et `board_name`
lorsqu'ils sont exposés par le pilote. Pour `mpt3sas`, le même host sysfs fournit
aussi le numéro IOC via `unique_id` ; `/dev/mpt3ctl` sert uniquement à lire la
température par MPI CONFIG. `mpt3ctl` ne lit donc plus les pages Manufacturing
du firmware ; StorCLI ne fournit plus l'identité publiée.

StorCLI redécouvre l'association entre ses index et l'inventaire sysfs après une
erreur ou un changement de l'ensemble de leurs index. Un remplacement ou une
reconfiguration à chaud qui conserve les mêmes index peut nécessiter un
redémarrage d'UVSS pour redécouvrir l'identité du matériel.

Une collecte HBA possède un contexte avec un délai de 15 secondes, mais ce délai
ne peut pas interrompre un appel bloqué dans le noyau. L'I/O backend reste hors
du verrou du collector : le dernier snapshot valide demeure lisible pendant
l'intervalle normal augmenté de ces 15 secondes, puis expire pendant que la
publication VSOCK et les diagnostics continuent. Aucun autre appel HBA n'est
lancé avant le retour du précédent, et tout résultat revenu trop tard est
rejeté.

## Topologie et failsafe

Chaque sonde possède son propre périphérique hwmon et reste donc toujours
`temp1`.

L'identité stable repose sur :

- l'ID Unraid pour un disque ;
- l'adresse SAS exposée par sysfs, ou à défaut l'adresse PCI, pour un HBA.

Les noms `hwmonX`, `/dev/sdX` et les index locaux des contrôleurs ne sont pas
utilisés comme identité.

La topologie est persistée dans `UNRAID_VSOCK_CACHE` et restaurée au démarrage
avec la température failsafe.

Un snapshot valide, même vide, est autoritaire. Un snapshot en erreur conserve
au contraire la topologie précédente.

Par défaut, après dix secondes sans mise à jour, une sonde `virt_temp` passe à :

```text
100 °C
```

Une nouvelle donnée valide rétablit automatiquement la température.

Les détails de la création des sondes via configfs et de leur alimentation via
`/dev/virt-temp/*` sont documentés dans [`virt-temp/README.md`](virt-temp/README.md).

## Mise à jour et désinstallation

Une mise à jour comme une désinstallation du plugin conserve sa configuration et
`disk-policies.json` sous `/boot/config/plugins/unraid-vsock-sensors`. Supprimer
manuellement ce répertoire pour effectuer une purge complète.

Sur Proxmox :

```sh
apt install ./unraid-vsock-sensors-hwmon_X.Y.Z-N_amd64.deb
```

Un upgrade avec un processus extérieur conservant ouvert un FD
`/dev/virt-temp/*` n'est pas supporté : fermer ces descripteurs avant
l'installation. S'ils empêchent le déchargement du module, fermer le FD puis
reprendre la configuration avec `dpkg --configure -a`.

Un processus extérieur qui conserve ouvert le FD d'une sonde peut empêcher le
déchargement de `virt_temp`. La suppression échoue alors proprement et, si le
service était actif, tente de le relancer pour restaurer la topologie depuis le
cache ; l'ancien FD retourne néanmoins `ENODEV`. Fermer le FD puis relancer la
suppression. Le paquet ne tue pas le processus extérieur.

Conserver la configuration :

```sh
apt remove unraid-vsock-sensors-hwmon
```

Les abonnements créés avec le template
`unraid-vsock-hwmon-restart@.service` appartiennent à l'administrateur et sont
conservés. Les désactiver explicitement avant la désinstallation s'ils ne sont
plus nécessaires.

Purger la configuration et le cache :

```sh
apt purge unraid-vsock-sensors-hwmon
```

La purge supprime la configuration et le cache. Elle ne retire spécialement ni
les abonnements au template UVSS, ni les unités tierces directement abonnées au
dispatcher de topologie : ces abonnements explicites restent sous la
responsabilité de l'administrateur.

## Développement

Les prérequis, commandes de validation, constructions et procédures de release
sont documentés dans [`CONTRIBUTING.md`](CONTRIBUTING.md). L'intégration Proxmox
est détaillée dans [`tests/vm/README.md`](tests/vm/README.md).

## Sécurité

AF_VSOCK n'est pas un mécanisme général d'authentification.

L'agent se connecte au CID hôte standard `2`. Le récepteur n'accepte que le CID
configuré, vérifie la version du protocole et limite chaque snapshot à 1 Mio.

Le receiver reste lancé en `root` pour administrer configfs et écrire dans les
miscdevices, mais son processus principal ne conserve que
`CAP_NET_BIND_SERVICE`, nécessaire au port privilégié `990`. L'unité restreint
les sockets à `AF_VSOCK`, interdit l'accès D-Bus et systemd, masque les
répertoires personnels et rend le système de fichiers non modifiable hors de
son `StateDirectory`, de son `RuntimeDirectory` et des interfaces `virt_temp`.
Le `modprobe` exécuté avant le daemon reste explicitement privilégié.

Les abonnements de topologie restent créés par root. Un receiver compromis peut
déclencher répétitivement les abonnés déjà autorisés, mais ne peut ni les
choisir ni en créer de nouveaux.

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

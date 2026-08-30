# unraid-vsock-sensors

`unraid-vsock-sensors` transmet les températures des disques et des contrôleurs
HBA d'une VM Unraid vers son hôte Proxmox par `AF_VSOCK`.

Sur Proxmox, ces températures peuvent être :

- exposées comme sondes Linux `hwmon` natives pour CoolerControl, fan2go,
  fancontrol ou lm-sensors ;
- interrogées directement en ligne de commande ;
- utilisées sans réseau IP entre la VM et l'hôte.

Le serveur lit le cache de températures d'Unraid. Il n'exécute jamais
`smartctl` et ne réveille donc pas les disques en veille.

## Architecture

```text
VM Unraid                                      Hôte Proxmox
┌────────────────────────────┐                 ┌─────────────────────────────┐
│ disks.ini                  │                 │ unraid-vsock-sensors hwmon  │
│ /dev/mpt3ctl (HBA)         │                 │            │                │
│            │               │     AF_VSOCK    │            ▼                │
│ unraid-vsock-sensors serve ├────────────────►│ /dev/virt-temp              │
└────────────────────────────┘                 │            │                │
                                               │            ▼                │
                                               │ unraid_storage / unraid_hba │
                                               └─────────────────────────────┘
```

Deux composants utilisent le même binaire :

- le plugin Unraid lance la commande `serve` dans la VM ;
- le paquet Debian Proxmox lance la commande `hwmon` sur l'hôte et installe le
  module noyau DKMS `virt-temp`.

Le CID VSOCK et le port doivent être identiques des deux côtés. Les exemples
ci-dessous utilisent le CID `3` et le port `990`, qui sont aussi les valeurs
par défaut.

## Installation

### 1. Ajouter AF_VSOCK à la VM

Sur Proxmox, identifier le numéro de la VM Unraid :

```sh
qm list
```

Éditer `/etc/pve/qemu-server/<VMID>.conf` et ajouter :

```text
args: -device vhost-vsock-pci,guest-cid=3
```

Si une ligne `args:` existe déjà, ajouter seulement
`-device vhost-vsock-pci,guest-cid=3` à cette ligne. Le CID doit être unique
parmi les VM exécutées sur le même hôte.

Arrêter puis redémarrer complètement la VM pour créer le périphérique. Un
simple redémarrage de service dans Unraid ne suffit pas.

### 2. Installer le serveur dans Unraid

Dans **Plugins → Install Plugin**, fournir l'URL du descripteur `.plg` publié :

```text
https://git.lan.home/francois/unraid-vsock-sensors/raw/branch/main/unraid-plugin/unraid-vsock-sensors.plg
```

Ouvrir ensuite **Settings → Unraid VSOCK Sensors** et vérifier :

- **VSOCK port** : `990` ;
- **HBA monitoring** : `enabled` si un HBA compatible est disponible, sinon
  `disabled` ;
- **HBA backend** : `Native /dev/mpt3ctl` pour un contrôleur `mpt3sas`, ou
  `StorCLI` lorsque cet utilitaire est installé ;
- **HBA refresh interval** : `15 seconds` avec `mpt3ctl` ou `30 seconds` avec
  StorCLI.

Le serveur lit directement `/dev/mpt3ctl` pour les contrôleurs gérés par
`mpt3sas` : aucun utilitaire supplémentaire n'est nécessaire. Le backend
StorCLI exige que la commande `storcli` soit installée, directement avec son
paquet ou avec le plugin Unraid
[`storcli64`](https://forums.unraid.net/topic/192112-plugin-storcli64/). Aucun
repli automatique n'est effectué : une erreur du backend sélectionné est
signalée telle quelle. Le mode `disabled` ne consulte aucun contrôleur. Une
ancienne valeur `auto` de `HBA_MODE` est interprétée comme `enabled`.

Le backend `/dev/mpt3ctl` doit être considéré comme **expérimental**. Son
implémentation suit l'ABI et les structures du pilote `mpt3sas` du noyau Linux
upstream. Elle est utilisée en production par l'auteur sur un LSI SAS3008 avec
le pilote `mpt3sas` 54.100.00.00, mais n'a pas encore été validée sur un large
éventail de contrôleurs, de firmwares et de versions du pilote. StorCLI reste
donc disponible comme alternative explicite.

Vérification depuis le terminal Unraid :

```sh
/etc/rc.d/rc.unraid-vsock-sensors status
/usr/local/sbin/unraid-vsock-sensors version
grep 'unraid-vsock-sensors' /var/log/syslog | tail -n 50
```

### 3. Installer l'intégration hwmon sur Proxmox

Copier le `.deb` sur Proxmox, puis exécuter les commandes suivantes en tant que
`root`. Adapter le nom du fichier à la version téléchargée :

```sh
apt update
apt install "proxmox-headers-$(uname -r)" \
  ./unraid-vsock-sensors-hwmon_X.Y.Z-1_amd64.deb
```

Cette commande :

- installe DKMS et les outils de compilation nécessaires ;
- installe le méta-paquet `proxmox-default-headers`, afin que les headers suivent
  automatiquement les mises à jour du noyau Proxmox par défaut ;
- compile `virt-temp` pour le noyau Proxmox actif ;
- installe le binaire dans `/usr/bin` ;
- active et démarre `unraid-vsock-hwmon.service`, qui charge explicitement le
  module avant de lancer l'agent.

Le fichier `/etc/default/unraid-vsock-hwmon` est créé seulement s'il n'existe
pas. Une configuration provenant de l'ancien installateur tarball est conservée
et les anciens fichiers sont migrés automatiquement.

Le header explicite de `$(uname -r)` dans la commande garantit aussi
l'installation sur le noyau actuellement démarré, notamment avant un reboot
suivant une mise à jour de noyau. Le méta-paquet prend ensuite en charge les
futurs noyaux de la branche Proxmox par défaut.

Le warning APT indiquant qu'un téléchargement est effectué sans sandbox est
sans gravité lorsque le `.deb` se trouve dans `/root`. Le placer dans `/tmp`
évite ce message.

## Configuration sur Proxmox

Le fichier `/etc/default/unraid-vsock-hwmon` contient :

```sh
UNRAID_VSOCK_CID=3
UNRAID_VSOCK_PORT=990
UNRAID_VSOCK_INTERVAL=1s
UNRAID_VSOCK_CACHE=/var/lib/unraid-vsock-sensors/hwmon-inventory.json
# UNRAID_VSOCK_RESTART_UNITS=coolercontrold.service
```

- `UNRAID_VSOCK_CID` désigne la VM Unraid configurée dans Proxmox ;
- `UNRAID_VSOCK_PORT` doit correspondre au port du plugin Unraid ;
- `UNRAID_VSOCK_INTERVAL` définit la fréquence de lecture du cache par l'agent
  Proxmox. Il ne détermine pas la fréquence SMART d'Unraid ;
- `UNRAID_VSOCK_CACHE` conserve la structure des canaux entre deux démarrages ;
- `UNRAID_VSOCK_RESTART_UNITS` accepte une liste d'unités systemd séparées par
  des virgules. Les unités actives sont redémarrées après la restauration du
  cache ou un changement de topologie, afin qu'elles rescannent les hwmon.

Pour CoolerControl :

```sh
UNRAID_VSOCK_RESTART_UNITS=coolercontrold.service
```

Pour plusieurs consommateurs :

```sh
UNRAID_VSOCK_RESTART_UNITS=coolercontrold.service,fan2go.service
```

La valeur est vide par défaut : le projet ne suppose pas quel logiciel de
ventilation est installé. `systemctl try-restart` ne démarre que les unités déjà
actives.

Après une modification :

```sh
systemctl restart unraid-vsock-hwmon.service
```

## Vérifier le fonctionnement

Sur Proxmox :

```sh
dpkg -s unraid-vsock-sensors-hwmon | grep '^Status:'
dkms status -m virt-temp
systemctl status unraid-vsock-hwmon.service
journalctl -u unraid-vsock-hwmon.service -n 50 --no-pager
ls -l /dev/virt-temp
sensors
```

Résultats attendus :

- le paquet est `install ok installed` ;
- DKMS indique `virt-temp/X.Y.Z ... installed` pour le noyau actif ;
- le service est `active (running)` ;
- `sensors` affiche `unraid_storage` et, si activé, `unraid_hba`.

Pour afficher directement l'inventaire reçu sans passer par le module :

```sh
unraid-vsock-sensors get --cid 3 --port 990 --json
```

## Sondes publiées

`unraid_storage` contient :

- un canal par disque interne ;
- `HDD maximum`, `SATA SSD maximum` ou `NVMe SSD maximum` lorsqu'au moins deux
  disques appartiennent au groupe correspondant.

`unraid_hba` contient un canal par contrôleur lorsque la collecte HBA est
activée.

Les disques USB, les slots Unraid non assignés (`DISK_NP`) et la clé USB de
démarrage `flash` ne sont pas publiés. Les SSD utilisant un autre transport que
SATA ou NVMe restent accessibles en ligne de commande, mais ne créent pas de
canal maximum dédié.

Un disque en veille (`temp="*"` et `spundown="1"`) est conservé dans
l'inventaire avec une température de `0 °C`. Cette valeur signifie que la sonde
est inactive et évite de déclencher le failsafe pendant un spindown normal.

Unraid met à jour l'état de rotation et la température séparément. Juste après
un spin-up, `disks.ini` peut donc contenir temporairement `temp="*"` avec
`spundown="0"`, jusqu'au prochain relevé SMART réglé par `poll_attributes`.
Le serveur accorde alors une grâce de
`poll_attributes + 5 secondes`, sans la plafonner avant le prochain relevé
configuré. Pendant cette grâce, il conserve
la dernière température valide du disque, ou publie la sentinelle `0 °C` si
aucune mesure précédente n'existe. Une nouvelle température interrompt
immédiatement la grâce.

Si la température reste absente à l'expiration, le disque et le maximum de sa
catégorie (HDD, SATA SSD ou NVMe) passent explicitement au failsafe de `100 °C`.
Une autre température invalide déclenche ce failsafe sans grâce. La valeur
`poll_attributes` est lue comme donnée dans `/boot/config/disk.cfg` ; une valeur
nulle ou absente utilise un repli prudent de deux minutes.

## Inventaire persistant et changement de topologie

Après le premier relevé valide, l'agent enregistre la structure des canaux dans
`UNRAID_VSOCK_CACHE`. Au démarrage suivant, il la restaure immédiatement avec
des températures failsafe de `100 °C`, sans attendre la VM Unraid. Les logiciels
comme CoolerControl peuvent ainsi découvrir les canaux pendant le boot de
Proxmox, même si Unraid met plusieurs minutes à démarrer.

Sans cache, le premier relevé non vide de chaque famille configure ses canaux.
Un relevé vide est ignoré afin de ne pas figer un démarrage incomplet d'Unraid
ou du backend HBA. Seul le mode HBA explicitement `disabled` autorise un inventaire
HBA vide.

L'identité d'une sonde repose ensuite uniquement sur son ID stable : ID Unraid
pour un disque, puis adresse SAS, adresse PCI ou numéro de série pour un HBA. Les
indices locaux tels que l'IOC mpt3ctl ou le contrôleur StorCLI `/c0` ne sont
exposés ni dans le JSON, ni dans les sélecteurs, ni dans le cache hwmon.
`mpt3ctl` relit l'identité et la température dans chaque relevé. StorCLI conserve
la correspondance `/cN` sans expiration et surveille l'empreinte des HBA dans
`/sys/class/scsi_host`. Un changement de cette empreinte, une erreur de lecture
ou un ensemble de contrôleurs différent invalide immédiatement le cache et
déclenche une unique redécouverte suivie, si nécessaire, d'une nouvelle tentative.
Les deux backends produisent en priorité le même ID `sas:<adresse>` ; l'adresse
PCI puis le numéro de série servent de replis lorsqu'elle est indisponible.
Le label HBA est construit à partir du modèle et de l'adresse PCI, avec l'ID
stable comme repli, puis conservé tant que cet ID reste présent.

Pendant l'exécution :

- une erreur globale de lecture ne modifie jamais le cache et laisse toute la
  famille disque atteindre le failsafe ; après une éventuelle grâce de spin-up,
  une température indisponible ou invalide place son disque et le maximum de sa
  catégorie au failsafe ;
- une lecture HBA sans métadonnées correspondantes est ignorée sans
  interrompre l'actualisation des autres contrôleurs ;
- un inventaire Unraid valide contenant des ID ajoutés ou retirés remplace
  automatiquement la famille hwmon concernée et met à jour le cache ;
- un changement de `/dev/sdX`, de nom affiché ou d'index IOC ne modifie pas
  l'identité si l'ID stable reste identique ;
- les consommateurs configurés dans `UNRAID_VSOCK_RESTART_UNITS` sont relancés
  après la reconfiguration afin de découvrir les nouveaux canaux.

## Failsafe et fraîcheur des mesures

Chaque canal non actualisé pendant 10 secondes retourne `100 °C`. Cela couvre
l'arrêt du serveur ou de l'agent, une perte VSOCK, une erreur de lecture et la
disparition d'une sonde attendue.

La température des disques vient de `/var/local/emhttp/disks.ini`. Sa fraîcheur
dépend de **Tunable (poll_attributes)** dans Unraid : interroger toutes les
secondes peut donc retourner plusieurs fois la même valeur mise en cache.
Pour une régulation thermique réactive, une valeur de 30 à 60 secondes est
recommandée. Cinq minutes constitue une limite haute raisonnable ; au-delà, une
température peut rester ancienne trop longtemps pour piloter efficacement les
ventilateurs. Le serveur continue de démarrer afin de préserver les autres
sondes, mais écrit un avertissement dans son journal lorsque `poll_attributes`
est supérieur à cinq minutes, nul ou absent.

La température HBA vient de IO Unit Page 7, lue avec des commandes MPI CONFIG
strictement en lecture seule via `/dev/mpt3ctl`. Les valeurs Celsius et
Fahrenheit sont converties puis validées dans la plage `0..150 °C`. Chaque
collecte native lit aussi les pages de fabrication pour associer la température
à l'adresse SAS stable et au modèle actuels, indépendamment du numéro IOC. Le
serveur actualise ce relevé en arrière-plan ; les requêtes VSOCK n'attendent
jamais une commande du contrôleur. Lorsque le backend StorCLI est sélectionné,
la température ROC fournie par sa sortie JSON est utilisée à la place.

## Utilisation en ligne de commande

Les sélecteurs de groupe retournent la température maximale :

```sh
unraid-vsock-sensors get --cid 3 --port 990 disk hdd
unraid-vsock-sensors get --cid 3 --port 990 disk ssd
unraid-vsock-sensors get --cid 3 --port 990 disk nvme
unraid-vsock-sensors get --cid 3 --port 990 hba all
```

Un disque ou un HBA peut être interrogé explicitement :

```sh
unraid-vsock-sensors get --cid 3 --port 990 disk disk1
unraid-vsock-sensors get --cid 3 --port 990 disk sdb
unraid-vsock-sensors get --cid 3 --port 990 hba sas:500605b00abc1234
```

Ces commandes écrivent uniquement un nombre en degrés Celsius et conviennent à
une source `cmd` de fan2go. L'option `--json` affiche le snapshot complet avec
les erreurs éventuelles de chaque famille.

## Mise à jour et désinstallation Proxmox

Installer une nouvelle version avec `apt` :

```sh
apt install ./unraid-vsock-sensors-hwmon_X.Y.Z-N_amd64.deb
```

Le suffixe `-N` est la révision Debian du packaging. Il peut augmenter sans que
la version du logiciel change.

Conserver la configuration lors de la suppression :

```sh
apt remove unraid-vsock-sensors-hwmon
```

Supprimer également `/etc/default/unraid-vsock-hwmon` :

```sh
apt purge unraid-vsock-sensors-hwmon
```

## Compiler et tester

Go 1.27.0 ou plus récent est nécessaire sur la machine de développement.

```sh
make check          # exécute go vet et go test
make build          # crée bin/unraid-vsock-sensors
make unraid-package # crée le .txz et le .plg Unraid
make hwmon-package  # crée le .deb Proxmox
make all            # exécute tous les contrôles et construit tous les artefacts
```

Sans `VERSION`, la version est dérivée de Git et reçoit un suffixe `-dev` si le
commit courant n'est pas exactement tagué.

### Tester manuellement un paquet Unraid de développement

Pour tester rapidement une modification sur une machine où le plugin a déjà
été installé, construire le paquet :

```sh
make unraid-package
```

La version et le nom du paquet sont dérivés automatiquement de l'état de Git.
La commande affiche le chemin exact du `.txz` produit. Copier ce fichier sur
Unraid en conservant son nom, utilisé par `upgradepkg` pour identifier sa
version :

```sh
scp dist/<nom-du-paquet-affiché>.txz root@NAS:/tmp/
```

Puis arrêter le service, réinstaller le paquet et le redémarrer depuis Unraid :

```sh
/etc/rc.d/rc.unraid-vsock-sensors stop
upgradepkg --install-new --reinstall \
  /tmp/<nom-du-paquet-affiché>.txz
/etc/rc.d/rc.unraid-vsock-sensors start
```

Cette opération remplace le binaire, la page Web et les scripts du plugin sans
effacer la configuration persistante située dans
`/boot/config/plugins/unraid-vsock-sensors/`.

Cette méthode suppose que le plugin complet a déjà été installé au moyen de son
fichier `.plg`. Installer uniquement le `.txz` ne l'enregistre pas dans le
gestionnaire de plugins et ne garantit pas sa réinstallation après un
redémarrage, puisque le système Unraid est chargé en mémoire. Pour valider une
première installation ou le cycle de démarrage, utiliser un `.plg` dont l'URL
de paquet pointe vers le `.txz` de développement.

La construction régénère également le fichier suivi
`unraid-plugin/unraid-vsock-sensors.plg`. Ne pas commiter ce descripteur pour
une version `-dev` ; seul celui d'une version finale destinée à être publiée
doit être conservé dans Git.

## Publier une release

Partir d'un arbre propre, construire avec la version finale, commiter le `.plg`
produit, puis poser le tag sur ce commit :

```sh
make all VERSION=X.Y.Z
git add unraid-plugin/unraid-vsock-sensors.plg
git commit -m "Publie le descripteur Unraid X.Y.Z"
git tag -a vX.Y.Z -m "Release vX.Y.Z"
git push origin main vX.Y.Z
```

Joindre à la release Gitea :

- `dist/unraid-vsock-sensors-X.Y.Z-x86_64-1.txz` ;
- `dist/unraid-vsock-sensors-hwmon_X.Y.Z-1_amd64.deb`.

Une correction limitée au paquet Debian peut être produite avec une nouvelle
révision :

```sh
make hwmon-package VERSION=X.Y.Z DEBIAN_REVISION=2
```

## Sécurité du transport

AF_VSOCK n'est pas un mécanisme d'authentification général. Le serveur accepte
uniquement le CID hôte standard `2`, une commande fixe `GET`, une requête limitée
à 1 Kio et ne reçoit aucun chemin fourni par le client.

## Références techniques et remerciements

Le backend HBA natif s'appuie sur l'ABI publique du pilote Linux `mpt3sas` et
sur les définitions MPI disponibles dans les sources du noyau, notamment
[`mpt3sas_ctl.h`](https://github.com/torvalds/linux/blob/master/drivers/scsi/mpt3sas/mpt3sas_ctl.h),
[`mpt3sas_ctl.c`](https://github.com/torvalds/linux/blob/master/drivers/scsi/mpt3sas/mpt3sas_ctl.c),
[`mpi2_cnfg.h`](https://github.com/torvalds/linux/blob/master/drivers/scsi/mpt3sas/mpi/mpi2_cnfg.h)
et
[`mpt3sas_hwmon.c`](https://github.com/torvalds/linux/blob/master/drivers/scsi/mpt3sas/mpt3sas_hwmon.c).
Ces références ont servi à implémenter les ioctl de `/dev/mpt3ctl`, les requêtes
MPI CONFIG en lecture seule et le décodage de la température HBA.

L'architecture consistant à publier sur l'hôte Proxmox une sonde `hwmon`
virtuelle alimentée par les températures d'une VM a été inspirée par le projet
GPL-2.0
[`wxxsfxyzm/hdd-temp-monitor`](https://github.com/wxxsfxyzm/hdd-temp-monitor).
Le présent projet étend cette idée avec AF_VSOCK, des inventaires dynamiques et
persistants, plusieurs familles de sondes et une gestion explicite du failsafe.

## Licence

Ce projet est distribué selon les termes de la
[GNU General Public License version 2 uniquement](LICENSE) (`GPL-2.0-only`).
Cette licence est compatible avec celle du pilote Linux `mpt3sas`, du module
`hwmon` du noyau et du projet `hdd-temp-monitor` cités ci-dessus.

## Développement assisté par IA

Ce projet est vibecodé : une part importante du code et de la documentation a
été produite avec l'assistance d'une IA. Les changements sont néanmoins relus,
les chemins critiques sont testés, et le projet est utilisé en production sur
la machine personnelle de son auteur.

Cette transparence ne remplace pas une garantie de fonctionnement sur toutes
les configurations Unraid ou Proxmox. Examiner les changements et tester les
packages dans son propre environnement reste recommandé.

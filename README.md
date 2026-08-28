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
│ StorCLI (HBA, optionnel)   │                 │            │                │
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
- **HBA monitoring** : `enabled` si StorCLI et un HBA compatible sont
  disponibles, sinon `disabled` ;
- **StorCLI refresh interval** : `30 seconds` convient généralement.

Le mode HBA `enabled` exige StorCLI et au moins un contrôleur. Le mode
`disabled` n'exécute jamais StorCLI. Une ancienne configuration `auto` est
interprétée comme `enabled`.

Vérification depuis le terminal Unraid :

```sh
/etc/rc.d/rc.unraid-vsock-sensors status
/usr/local/sbin/unraid-vsock-sensors version
tail -n 50 /var/log/unraid-vsock-sensors.log
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

`unraid_hba` contient un canal par contrôleur StorCLI lorsque la collecte HBA
est activée.

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
Cet état, comme une température invalide, rend indisponible uniquement le canal
du disque. Le maximum de sa catégorie (HDD, SATA SSD ou NVMe) continue d'être
calculé avec les températures disponibles ; il devient indisponible seulement
si aucun membre du groupe n'est mesurable. Les canaux indisponibles atteignent
`100 °C` après `stale_timeout`.

## Inventaire persistant et changement de topologie

Après le premier relevé valide, l'agent enregistre la structure des canaux dans
`UNRAID_VSOCK_CACHE`. Au démarrage suivant, il la restaure immédiatement avec
des températures failsafe de `100 °C`, sans attendre la VM Unraid. Les logiciels
comme CoolerControl peuvent ainsi découvrir les canaux pendant le boot de
Proxmox, même si Unraid met plusieurs minutes à démarrer.

Sans cache, le premier relevé non vide de chaque famille configure ses canaux.
Un relevé vide est ignoré afin de ne pas figer un démarrage incomplet d'Unraid
ou de StorCLI. Seul le mode HBA explicitement `disabled` autorise un inventaire
HBA vide.

L'identité d'une sonde repose ensuite uniquement sur son ID stable : ID Unraid
pour un disque, puis numéro de série, adresse SAS, adresse PCI ou index StorCLI
pour un HBA. Le label est une information d'affichage conservée tant que l'ID
reste présent.

Pendant l'exécution :

- une erreur globale de lecture ne modifie jamais le cache et laisse toute la
  famille disque atteindre le failsafe ; une température indisponible ou
  invalide n'affecte que son disque, tandis que le maximum ignore ce membre ;
- une lecture StorCLI sans métadonnées HBA correspondantes est ignorée sans
  interrompre l'actualisation des autres contrôleurs ;
- un inventaire Unraid valide contenant des ID ajoutés ou retirés remplace
  automatiquement la famille hwmon concernée et met à jour le cache ;
- un changement de `/dev/sdX`, de nom affiché ou d'index StorCLI ne modifie pas
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

La température HBA vient du champ `ROC temperature(Degree Celsius)` de StorCLI.
Le serveur actualise ce cache en arrière-plan ; les requêtes VSOCK n'attendent
jamais l'exécution de StorCLI.

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
unraid-vsock-sensors get --cid 3 --port 990 hba hba0
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

Go 1.25 ou plus récent est nécessaire sur la machine de développement.

```sh
make check          # exécute go vet et go test
make build          # crée bin/unraid-vsock-sensors
make unraid-package # crée le .txz et le .plg Unraid
make hwmon-package  # crée le .deb Proxmox
make all            # exécute tous les contrôles et construit tous les artefacts
```

Sans `VERSION`, la version est dérivée de Git et reçoit un suffixe `-dev` si le
commit courant n'est pas exactement tagué.

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

## Développement assisté par IA

Ce projet est vibecodé : une part importante du code et de la documentation a
été produite avec l'assistance d'une IA. Les changements sont néanmoins relus,
les chemins critiques sont testés, et le projet est utilisé en production sur
la machine personnelle de son auteur.

Cette transparence ne remplace pas une garantie de fonctionnement sur toutes
les configurations Unraid ou Proxmox. Examiner les changements et tester les
packages dans son propre environnement reste recommandé.

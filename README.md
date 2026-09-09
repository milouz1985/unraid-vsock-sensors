# unraid-vsock-sensors

`unraid-vsock-sensors` transmet les températures des disques et des contrôleurs
HBA d'une VM Unraid vers son hôte Proxmox par `AF_VSOCK`.

Sur Proxmox, ces températures peuvent être :

- exposées comme sondes Linux `hwmon` natives pour CoolerControl, fan2go,
  fancontrol ou lm-sensors ;
- utilisées sans réseau IP entre la VM et l'hôte.

L'agent Unraid lit l'inventaire et l'état de rotation, puis relève en
arrière-plan la température des disques actifs avec le helper
`smartctl_type`. Il ne consulte pas les disques déjà signalés en veille et
utilise `smartctl -n standby` pour couvrir un changement d'état concurrent.
Il maintient une connexion VSOCK vers Proxmox et y pousse chaque seconde le
dernier snapshot complet. Cette publication périodique sert aussi de heartbeat.
Chaque snapshot porte un numéro de protocole entier ; le récepteur refuse une
version incompatible. La version logicielle reste indépendante de ce numéro.

## Architecture

```text
VM Unraid                                      Hôte Proxmox
┌────────────────────────────┐                 ┌─────────────────────────────┐
│ disks.ini + smartctl_type  │                 │ unraid-vsock-sensors hwmon  │
│ /dev/mpt3ctl (HBA)         │                 │            │                │
│            │               │     AF_VSOCK    │            ▼                │
│ unraid-vsock-sensors serve ├────────────────►│ /dev/virt-temp              │
└────────────────────────────┘                 │            │                │
                                               │            ▼                │
                                               │ 1 périphérique hwmon/sonde  │
                                               └─────────────────────────────┘
```

Deux composants utilisent le même binaire :

- le plugin Unraid lance la commande `serve` dans la VM ;
- le paquet Debian Proxmox lance la commande `hwmon` sur l'hôte et installe le
  module noyau DKMS `virt-temp`.

Le port doit être identique des deux côtés. Le récepteur Proxmox vérifie aussi
que les snapshots viennent du CID configuré pour la VM Unraid. Les exemples
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

### 2. Installer l'agent dans Unraid

Dans **Plugins → Install Plugin**, fournir l'URL du descripteur `.plg` publié :

```text
https://raw.githubusercontent.com/milouz1985/unraid-vsock-sensors/refs/heads/main/unraid-plugin/unraid-vsock-sensors.plg
```

Ouvrir ensuite **Settings → Unraid VSOCK Sensors** et vérifier :

- **VSOCK port** : `990` ;
- **Disk SMART refresh interval** : `30 seconds` ;
- **HBA monitoring** : `enabled` si un HBA compatible est disponible, sinon
  `disabled` ;
- **HBA backend** : `Native /dev/mpt3ctl` pour un contrôleur `mpt3sas`, ou
  `StorCLI` lorsque cet utilitaire est installé ;
- **HBA refresh interval** : `15 seconds` avec `mpt3ctl` ou `30 seconds` avec
  StorCLI.

Le plugin transmet ces deux intervalles à l'agent. La collecte disque vaut
`30s` par défaut dans les deux cas. Pour le HBA, le plugin choisit `15s` avec
`mpt3ctl` et `30s` avec StorCLI ; lancé manuellement sans option, le binaire
utilise le défaut générique de `30s`, quel que soit le backend.

L'agent lit directement `/dev/mpt3ctl` pour les contrôleurs gérés par
`mpt3sas` : aucun utilitaire supplémentaire n'est nécessaire. Le backend
StorCLI exige que la commande `storcli` soit installée, directement avec son
paquet ou avec le plugin Unraid
[`storcli64`](https://forums.unraid.net/topic/192112-plugin-storcli64/). Aucun
repli automatique n'est effectué : une erreur du backend sélectionné est
signalée telle quelle. Le mode `disabled` ne consulte aucun contrôleur. Le
réglage `HBA_MODE` accepte uniquement les valeurs `enabled` et `disabled`.

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
- compile `virt-temp` pour le noyau Proxmox actif et pour chaque autre noyau
  déjà présent dont les en-têtes sont installés ;
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
UNRAID_VSOCK_CACHE=/var/lib/unraid-vsock-sensors/hwmon-inventory.json
# UNRAID_VSOCK_RESTART_UNITS=coolercontrold.service
```

- `UNRAID_VSOCK_CID` désigne la VM Unraid configurée dans Proxmox ;
- `UNRAID_VSOCK_PORT` doit correspondre au port du plugin Unraid ;
- `UNRAID_VSOCK_CACHE` conserve les périphériques des sondes entre deux démarrages ;
- `UNRAID_VSOCK_RESTART_UNITS` accepte une liste d'unités systemd séparées par
  des virgules. Les unités actives sont redémarrées après le premier snapshot
  valide, puis après un changement de topologie, afin qu'elles rescannent les
  hwmon.

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
- `sensors` affiche un périphérique lisible par sonde, par exemple
  `unraid_disk1`, `unraid_hdd_maximum` ou `unraid_sas3008`.

## Sondes publiées

Le module crée un périphérique hwmon indépendant avec un unique `temp1` pour :

- chaque disque interne ;
- `HDD maximum`, `SATA SSD maximum` ou `NVMe SSD maximum` lorsqu'au moins deux
  disques appartiennent au groupe correspondant.

Chaque contrôleur possède également son propre périphérique lorsque la collecte
HBA est activée. Le nom hwmon est dérivé du label pour rester lisible ; la partie
entre parenthèses, telle que le périphérique bloc ou l'adresse PCI, en est
retirée. Le label complet reste disponible dans `temp1_label`.

### Pourquoi un périphérique par sonde ?

Dans un périphérique agrégé, les fichiers `temp1`, `temp2`, etc. décrivent des
positions, pas l'identité des sondes. L'ajout d'un maximum de groupe ou le
retrait d'un disque peut donc décaler les numéros suivants et faire pointer une
configuration CoolerControl ou fan2go vers une autre température. Conserver les
anciens numéros éviterait ce décalage, mais laisserait après un retrait planifié
des sondes fantômes au failsafe de `100 °C`.

Un périphérique par ID stable évite les deux problèmes : chaque sonde reste
toujours son propre `temp1`, et son retrait supprime son périphérique sans
réaffecter l'identité des autres. Le numéro dynamique `hwmonX` et le nom hwmon
lisible ne sont pas utilisés comme identité ; celle-ci vient du parent platform,
dont le nom encode sans collision l'ID stable.

Cette organisation remplace les anciens périphériques agrégés `unraid_storage`
et `unraid_hba`. Lors de la première mise à niveau vers cette version, il faut
donc sélectionner une fois les nouvelles sources dans CoolerControl ou adapter
les `platform` configurées dans fan2go.

L'agent Unraid exclut les disques USB avant toute lecture SMART et tout envoi
VSOCK. Les slots non assignés (`DISK_NP`) et la clé USB de démarrage `flash`
sont également exclus. Les SSD internes utilisant un autre transport que SATA
ou NVMe possèdent un périphérique individuel, mais ne créent pas de maximum
dédié.

Un disque signalé en veille par `spundown="1"` est conservé dans l'inventaire
avec une température de `0 °C`, sans exécuter de commande SMART. Cette valeur
signifie que la sonde est inactive et évite de déclencher le failsafe pendant
un spindown normal.

Pour chaque disque actif, l'agent appelle le helper Unraid
`/usr/local/sbin/smartctl_type` avec `--json -n standby,3 -A`. Unraid résout
ainsi le périphérique et les éventuels paramètres particuliers du contrôleur.
D'après la [documentation officielle de `smartctl` pour
`-n`](https://github.com/smartmontools/smartmontools/blob/main/src/smartctl.8.in#L896-L915),
le code par défaut `2` peut aussi signaler un échec d'ouverture ou
d'identification. Le code `3` n'est accepté comme veille que si le JSON indique
`STANDBY` ou `SLEEP`. Les NVMe sont interrogés sans l'option `-n standby`.

Une erreur SMART transitoire conserve la dernière température valide pendant
l'intervalle de collecte augmenté de cinq secondes. Si aucune mesure valide
n'existe encore, le disque est immédiatement déclaré indisponible. Si l'erreur
persiste après la grâce, il l'est également. Dans les deux cas, le disque et le
maximum de sa catégorie passent explicitement au failsafe de `100 °C`. Cet
intervalle appartient au service, vaut `30s` par défaut et ne dépend plus du
réglage Unraid **Tunable (poll_attributes)**.

Le récepteur ferme une connexion qui ne fournit aucun snapshot pendant environ
trois secondes afin de permettre une reconnexion propre. Ce délai de transport
ne déclenche pas lui-même le failsafe thermique : celui-ci reste le
`stale_timeout` de 10 secondes appliqué indépendamment par `virt_temp`.

## Inventaire persistant et changement de topologie

Après le premier relevé valide, l'agent enregistre la liste des sondes dans
`UNRAID_VSOCK_CACHE`. Au démarrage suivant, il la restaure immédiatement avec
des températures failsafe de `100 °C`, sans attendre la VM Unraid. Les logiciels
comme CoolerControl peuvent ainsi découvrir les périphériques pendant le boot de
Proxmox, même si Unraid met plusieurs minutes à démarrer.

Sans cache, le premier relevé sans erreur configure sa famille, y compris avec
un inventaire vide. Un snapshot portant `error` ou `hba_error` n'est pas
autoritaire : il ne modifie jamais la topologie précédente et la laisse
atteindre le failsafe. Sans erreur, l'inventaire reçu est autoritaire ; une
liste vide retire donc les périphériques de la famille. Le mode HBA `disabled`
est représenté naturellement par `hbas: []` sans `hba_error`.

L'identité d'une sonde repose ensuite uniquement sur son ID stable : ID Unraid
pour un disque, puis adresse SAS, adresse PCI ou numéro de série pour un HBA. Les
indices locaux tels que l'IOC mpt3ctl ou le contrôleur StorCLI `/c0` ne sont
pas conservés dans le cache hwmon.
`mpt3ctl` relit l'identité et la température dans chaque relevé, mais conserve
par adresse PCI la dernière identité SAS valide afin qu'une erreur transitoire
de la page Manufacturing 5 ne renomme pas la sonde. StorCLI conserve
la correspondance `/cN` découverte lors du premier relevé tant que les lectures
réussissent. Une erreur de lecture, notamment un ensemble de contrôleurs
différent, invalide cette correspondance et déclenche une redécouverte. Lorsqu'un
cache existait déjà, StorCLI effectue ensuite une unique nouvelle tentative.
Les deux backends produisent en priorité le même ID `sas:<adresse>` ; l'adresse
PCI puis le numéro de série servent de replis lorsqu'elle est indisponible.
Le label HBA est construit à partir du modèle et de l'adresse PCI, avec l'ID
stable comme repli, puis conservé tant que cet ID reste présent.

Pendant l'exécution :

- chaque ID stable possède son propre périphérique et reste donc `temp1` sans
  dépendre de l'ordre des autres sondes. Une modification de l'ensemble des ID
  recrée tous les périphériques de la famille ; leurs noms platform et leurs
  identités restent stables, mais leurs numéros dynamiques `hwmonX` peuvent
  changer. La composition d'un maximum HDD, SSD ou NVMe ne fait que modifier sa
  valeur. Un ID retiré ne réaffecte jamais l'identité d'une autre sonde ;
- une erreur globale de lecture, y compris une section active de `disks.ini`
  sans ID ou périphérique, ne modifie jamais le cache et laisse toute la famille
  disque atteindre le failsafe ; après une éventuelle grâce de spin-up, une
  température indisponible ou invalide place son disque et le maximum de sa
  catégorie au failsafe ;
- une erreur HBA invalide le relevé complet : l'inventaire précédent reste
  configuré sans être actualisé et atteint donc le failsafe. StorCLI tente
  auparavant une redécouverte et une nouvelle lecture lorsque celle fondée sur
  sa correspondance en cache échoue ;
- un inventaire Unraid valide contenant des ID ajoutés ou retirés remplace
  automatiquement la famille hwmon concernée et met à jour le cache ;
- un changement de `/dev/sdX`, de nom affiché ou d'index IOC ne modifie pas
  l'identité si l'ID stable reste identique. Le label est fixé lors de la
  configuration et les relevés suivants sont appliqués par ID ;
- les consommateurs configurés dans `UNRAID_VSOCK_RESTART_UNITS` sont relancés
  une première fois dès que la VM répond, même si la topologie restaurée depuis
  le cache est inchangée, puis après chaque reconfiguration afin de découvrir
  les nouveaux périphériques.

## Failsafe et fraîcheur des mesures

Chaque collecteur Unraid invalide son propre cache après son intervalle normal
augmenté du délai maximal de collecte. Un collecteur bloqué finit donc par
publier une erreur pour sa famille ; Proxmox cesse de l'actualiser sans
interrompre l'autre famille.

Chaque canal du module `virt_temp` non actualisé pendant 10 secondes retourne
`100 °C`. Ce garde-fou couvre aussi bien une famille en erreur qu'une perte du
flux VSOCK ou l'arrêt du récepteur Proxmox.

La température des disques vient d'une collecte SMART directe, exécutée en
arrière-plan selon **Disk SMART refresh interval**, réglé à `30s` par défaut.
Le snapshot VSOCK réutilise ce relevé entre deux collectes et permet à Proxmox
de continuer à alimenter le module `virt-temp`. Pour une régulation thermique
réactive, une
valeur de 30 à 60 secondes est recommandée. Cinq minutes constitue une limite
haute raisonnable ; au-delà, une température peut rester ancienne trop
longtemps pour piloter efficacement les ventilateurs. L'agent accepte une
valeur plus longue passée en ligne de commande, mais écrit alors un
avertissement dans son journal.

La température HBA vient de IO Unit Page 7, lue avec des commandes MPI CONFIG
strictement en lecture seule via `/dev/mpt3ctl`. Les valeurs Celsius et
Fahrenheit sont converties puis validées dans la plage `0..150 °C`. Chaque
collecte native lit aussi les pages de fabrication pour associer la température
à l'adresse SAS stable et au modèle actuels, indépendamment du numéro IOC.
L'agent actualise ce relevé en arrière-plan sans bloquer la publication VSOCK.
Lorsque le backend StorCLI est sélectionné,
la température ROC fournie par sa sortie JSON est utilisée à la place.

Une collecte HBA dispose de 15 secondes. Au-delà, l'agent signale une erreur
HBA même si l'ioctl reste bloqué : l'ancien relevé cesse d'être publié et les
périphériques hwmon atteignent leur failsafe après leur délai de 10 secondes sans
actualisation. Un résultat arrivé après l'échéance est rejeté ; une nouvelle
collecte réussie rétablit les mesures. Le cache reste valide pendant l'intervalle
normal entre deux collectes.

## Mise à jour et désinstallation Unraid

Une mise à jour du plugin conserve
`/boot/config/plugins/unraid-vsock-sensors/unraid-vsock-sensors.cfg` et ne le
remplace jamais par les valeurs par défaut du nouveau paquet.

Une désinstallation explicite depuis le gestionnaire de plugins arrête le
service et supprime sa configuration ainsi que le paquet conservé sur la clé
USB. Une réinstallation ultérieure repart donc des valeurs par défaut. Copier
le fichier `.cfg` avant la désinstallation si ses réglages doivent être
réutilisés.

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
make check          # vérifie le Go, les scripts, la page PHP et le script rc
make test-race      # exécute les tests Go avec le détecteur de courses
make build          # crée bin/unraid-vsock-sensors
make unraid-package # crée le .txz et le .plg Unraid
make hwmon-package  # crée le .deb Proxmox
make all            # exécute les contrôles locaux et construit tous les artefacts
```

Avant de produire une release, valider d'abord le parcours complet dans la VM,
puis construire les artefacts locaux uniquement si cette validation réussit :

```sh
make test-vm && make all
```

`make all` ne lance pas de VM ; les cibles `test-vm*` restent explicitement
séparées parce qu'elles nécessitent un environnement Proxmox distant.

Sans `VERSION`, la version est dérivée de Git et reçoit un suffixe `-dev` si le
commit courant n'est pas exactement tagué. Une version explicite s'écrit sans
le préfixe `v`, par exemple `make all VERSION=1.4.2` ; le tag Git correspondant
peut ensuite s'appeler `v1.4.2`.

### Tests d'intégration dans une VM

Le dossier [`tests/vm`](tests/vm/README.md) contient le constructeur d'un
template Debian pour Proxmox et le lanceur des tests dans un clone jetable :

```sh
# Préparation initiale depuis le poste de développement :
cp tests/vm/template.env.example tests/vm/template.env
# Éditer tests/vm/template.env avant de continuer.
make vm-template-sync

# Construire ensuite le template sur Proxmox :
ssh -t root@pve01.lan.home \
    'cd /root/uvss-template-builder && bash build-template.sh'

# Tests usuels :
make test-vm         # parcours complet dans une VM distante jetable
make test-vm-core    # module, hwmon et SMART QEMU uniquement
make test-vm-package # cycle du paquet Debian et de DKMS uniquement
```

La construction requiert `libguestfs-tools` sur Proxmox. Pour remplacer un
template existant après avoir renvoyé les fichiers du builder :

```sh
ssh -t root@pve01.lan.home \
    'cd /root/uvss-template-builder && bash build-template.sh --replace'
```

Les fichiers du builder, l'unique configuration locale `template.env` ignorée
par Git et la clé publique configurée sont copiés sur Proxmox. La procédure
complète, notamment la vérification préalable de l'installation de
`libguestfs-tools`, est décrite dans [`tests/vm/README.md`](tests/vm/README.md).

Le test d'intégration utilise `/dev/virt-temp` et sysfs pour vérifier les
températures, le failsafe et la récupération après rechargement du module.
Il remplace la simulation de l'erreur `ESTALE` et permet de retirer le writer
injecté uniquement pour les tests. Les tests unitaires restent accessibles
avec `make check` et `make test-race`. La VM est supprimée après succès et
conservée après échec.

Le template démarre le noyau Proxmox exact demandé, avec ses headers. Par
défaut, la cible est le noyau courant de l'hôte ; `PVE_KERNEL_RELEASE` permet
de la fixer. Le builder et le lanceur vérifient la version effectivement
démarrée dans la VM. La compilation et le chargement de `virt-temp`, les
tests hwmon/SMART et le cycle installation/mise à jour/remove/purge du paquet
DKMS se déroulent entièrement dans le clone. Le runner pilote Proxmox par SSH,
découvre l'adresse du clone avec QEMU Guest Agent et transfère directement le
working tree par `rsync`. Deux disques SATA QEMU jetables exercent le vrai
`smartctl` et le collecteur disque. Aucun module UVSS n'est compilé ou chargé
sur l'hôte.

Après une mise à jour du noyau de l'hôte, reconstruire le template pour la
nouvelle cible. Les anciens templates Debian doivent également être
reconstruits. Les tests du transport VSOCK entre hôte et invité, d'Unraid et
des contrôleurs physiques restent à réaliser dans leurs environnements
respectifs.

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

Ne pas exécuter `upgradepkg` seul : cette commande remplace le fichier binaire,
mais ne redémarre pas le processus. Le service continuerait alors à exécuter
l'ancien binaire, y compris son ancien protocole VSOCK, tandis que la commande
`/usr/local/sbin/unraid-vsock-sensors version` afficherait déjà la nouvelle
version. La version réellement exécutée peut être vérifiée après le redémarrage :

```sh
pid=$(cat /var/run/unraid-vsock-sensors.pid)
/proc/$pid/exe version
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

AF_VSOCK n'est pas un mécanisme d'authentification général. L'agent Unraid se
connecte uniquement au CID hôte standard `2`. Le récepteur Proxmox n'accepte
que le CID de VM configuré et limite chaque snapshot encadré à 1 Mio.

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
persistants, plusieurs familles de sondes et un failsafe indépendant du flux.

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

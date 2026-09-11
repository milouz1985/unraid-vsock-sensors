# unraid-vsock-sensors

`unraid-vsock-sensors` transmet les températures des disques et des contrôleurs
HBA d'une VM Unraid vers son hôte Proxmox par `AF_VSOCK`, sans réseau IP entre
les deux systèmes.

L'agent lit les températures disque déjà collectées par Unraid dans
`disks.ini`, `devs.ini` et son cache SMART. Il n'exécute jamais `smartctl` et ne
réveille donc pas lui-même les disques. Sur Proxmox, les températures deviennent
des sondes Linux `hwmon` utilisables par CoolerControl, fan2go, fancontrol ou
lm-sensors.

## Architecture

```text
VM Unraid                                      Hôte Proxmox
┌────────────────────────────┐                 ┌─────────────────────────────┐
│ emhttpd                    │                 │ unraid-vsock-sensors hwmon  │
│  └─ disks.ini, devs.ini    │                 │            │                │
│     et cache smart/*       │                 │            ▼                │
│ /dev/mpt3ctl ou StorCLI    │     AF_VSOCK    │ /dev/virt-temp              │
│            │               │                 │            │                │
│ unraid-vsock-sensors serve ├────────────────►│            ▼                │
└────────────────────────────┘                 │ sondes hwmon natives        │
                                               └─────────────────────────────┘
```

Le même binaire fournit les deux services :

- `serve`, installé par le plugin Unraid, collecte et pousse les snapshots ;
- `hwmon`, installé par le paquet Debian, les reçoit et pilote le module DKMS
  `virt-temp`.

Le port VSOCK doit être identique des deux côtés. Le récepteur vérifie également
le CID de la VM et la version du protocole. Les exemples utilisent les valeurs
par défaut : CID `3` et port `990`.

## Installation

### 1. Ajouter AF_VSOCK à la VM

Identifier la VM Unraid sur Proxmox :

```sh
qm list
```

Ajouter dans `/etc/pve/qemu-server/<VMID>.conf` :

```text
args: -device vhost-vsock-pci,guest-cid=3
```

Si une ligne `args:` existe déjà, y ajouter seulement l'option `-device`. Le CID
doit être unique parmi les VM actives de l'hôte. Arrêter puis redémarrer
complètement la VM pour créer le périphérique.

### 2. Installer le plugin Unraid

Dans **Plugins → Install Plugin**, fournir cette URL :

```text
https://raw.githubusercontent.com/milouz1985/unraid-vsock-sensors/refs/heads/main/unraid-plugin/unraid-vsock-sensors.plg
```

Ouvrir ensuite **Settings → Unraid VSOCK Sensors** et vérifier :

- **VSOCK port** : `990` ;
- **Unraid SMART polling interval** : valeur en lecture seule provenant
  d'Unraid ;
- **HBA monitoring** : `enabled` ou `disabled` ;
- **HBA backend** : `Native /dev/mpt3ctl` ou `StorCLI` ;
- **HBA refresh interval** : `15 seconds` avec `mpt3ctl`, `30 seconds` avec
  StorCLI par défaut.

Le backend natif ne nécessite aucun utilitaire, mais seulement un contrôleur
géré par `mpt3sas`. Il est expérimental : il est utilisé par l'auteur sur un LSI
SAS3008, mais n'a pas été validé sur un large éventail de contrôleurs et de
firmwares. Le backend StorCLI nécessite la commande `storcli`, disponible
notamment avec le plugin Unraid
[`storcli64`](https://forums.unraid.net/topic/192112-plugin-storcli64/).
Il n'existe aucun basculement automatique entre les deux backends.

Vérification rapide dans Unraid :

```sh
/etc/rc.d/rc.unraid-vsock-sensors status
/usr/local/sbin/unraid-vsock-sensors version
grep 'unraid-vsock-sensors' /var/log/syslog | tail -n 50
```

### 3. Installer le paquet Proxmox

Copier le `.deb` sur Proxmox puis, en tant que `root` :

```sh
apt update
apt install "proxmox-headers-$(uname -r)" \
  ./unraid-vsock-sensors-hwmon_X.Y.Z-1_amd64.deb
```

Le paquet installe le binaire, DKMS, les outils de compilation et le
méta-paquet `proxmox-default-headers`. Il compile `virt-temp` pour les noyaux
dont les en-têtes sont présents, puis active `unraid-vsock-hwmon.service`.

Le fichier `/etc/default/unraid-vsock-hwmon` n'est créé que s'il n'existe pas.
Une configuration issue d'une version précédente est donc conservée.

## Configuration Proxmox

`/etc/default/unraid-vsock-hwmon` contient :

```sh
UNRAID_VSOCK_CID=3
UNRAID_VSOCK_PORT=990
UNRAID_VSOCK_CACHE=/var/lib/unraid-vsock-sensors/hwmon-inventory.json
# UNRAID_VSOCK_RESTART_UNITS=coolercontrold.service
```

- `UNRAID_VSOCK_CID` désigne la VM Unraid ;
- `UNRAID_VSOCK_PORT` doit correspondre au port du plugin ;
- `UNRAID_VSOCK_CACHE` conserve la topologie des sondes entre les démarrages ;
- `UNRAID_VSOCK_RESTART_UNITS` contient éventuellement des unités systemd
  séparées par des virgules, par exemple :

```sh
UNRAID_VSOCK_RESTART_UNITS=coolercontrold.service,fan2go.service
```

Seules les unités déjà actives sont relancées, après le premier snapshot valide
et après une reconfiguration hwmon. La valeur est vide par défaut.

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

Le paquet et le module doivent être installés, le service actif, et `sensors`
doit afficher des périphériques tels que `unraid_disk1`,
`unraid_hdd_maximum` ou `unraid_sas3008`.

## Collecte et sondes publiées

### Disques

`disks.ini` reste la source d'autorité pour les disques assignés. Les unités de
`devs.ini` rejoignent le même snapshot ; ce fichier est fourni nativement par
Unraid et ne nécessite pas le plugin Unassigned Devices. Une unité présente
dans les deux fichiers est dédupliquée par son ID stable, avec priorité à son
entrée assignée. Le nom changeant `/dev/sdX` ne sert jamais d'identité.

Une sonde est créée pour chaque disque. Lorsqu'au moins deux disques
appartiennent à une même catégorie, un maximum `HDD`, `SATA SSD` ou `NVMe SSD`
est également publié. La clé USB `flash` est exclue ; les disques rotationnels,
y compris USB, appartiennent au groupe HDD.

La température provient du champ `temp` d'Unraid. Le `mtime` du rapport
`/var/local/emhttp/smart/<nom-logique>` pour un disque assigné, ou
`smart/<device>` pour un Unassigned Device, doit rester dans la fenêtre :

```text
poll_attributes + max(10 secondes, 20 % de poll_attributes)
```

Après chaque polling SMART, l'événement Unraid `poll_attributes` réveille le
daemon. Un watchdog de cinq secondes détecte les événements perdus, changements
d'inventaire, transitions de veille et expirations, sans accès SMART matériel.

Un disque en veille (`spundown="1"`) reste inventorié à `0 °C` sans exiger de
rapport frais. Après son réveil, la dernière mesure valide est conservée pendant
une période de grâce égale à `poll_attributes + 5s`. Sans nouvelle mesure, le
disque devient indisponible ; une mesure fraîche le rétablit automatiquement.

`poll_attributes` est lu dans `/var/local/emhttp/var.ini`. Une valeur absente,
négative ou non numérique entraîne un warning et un fallback interne de `30s`
pour le calcul de fraîcheur. `0` désactive valablement le polling automatique
d'Unraid. Une valeur supérieure à `60s` produit un warning sur le délai de
réaction thermique, sans modifier la configuration.

### Contrôleurs HBA

Le backend natif lit la température, l'adresse SAS et le modèle au moyen de
requêtes MPI CONFIG en lecture seule sur `/dev/mpt3ctl`. La température de IO
Unit Page 7 est une valeur signée sur 16 bits ; les valeurs Fahrenheit sont
converties en Celsius. StorCLI utilise la température ROC de sa sortie JSON.

Une collecte HBA possède un deadline de 15 secondes. Un `ioctl` synchrone peut
néanmoins rester bloqué ; le dernier snapshot valide expire alors séparément
après l'intervalle de collecte augmenté de 15 secondes. Tout résultat revenu
après son deadline est rejeté.

### Topologie et failsafe

Chaque sonde possède son propre périphérique hwmon et reste donc toujours
`temp1`. Son identité vient de l'ID Unraid pour un disque, puis de l'adresse SAS,
de l'adresse PCI ou du numéro de série pour un HBA. Les noms `hwmonX`, `/dev/sdX`
et les index locaux des contrôleurs ne sont pas considérés comme stables.

La topologie est enregistrée dans `UNRAID_VSOCK_CACHE` après un relevé valide et
restaurée au démarrage avec des températures failsafe. Un inventaire valide,
même vide, est autoritaire ; un snapshot en erreur conserve au contraire la
topologie précédente.

Un canal `virt_temp` non actualisé pendant dix secondes passe à `100 °C`. Ce
failsafe couvre une mesure indisponible, une panne de collecte, une perte VSOCK
ou l'arrêt du récepteur. Une nouvelle donnée valide rétablit la température.

## Contrat des fichiers Unraid

Les fichiers runtime sont des fichiers INI lus comme des données. Les champs
inconnus sont ignorés.

Pour `/var/local/emhttp/disks.ini` :

| Champ | Règle |
| --- | --- |
| section | Nom logique et nom du rapport `smart/<nom-logique>` ; `flash` est exclu. |
| `status` | Une valeur contenant `_NP` signifie qu'aucun disque physique n'est présent. Toute autre valeur conserve le disque, y compris dans un état dégradé. |
| `id` | Obligatoire pour un disque présent ; identité stable et clé de déduplication. |
| `device` | Obligatoire pour un disque présent ; `/dev/` est retiré. |
| `temp` | Optionnel ; absent, `*` ou invalide signifie que la mesure active a échoué. |
| `spundown` | Optionnel ; seule la valeur `1` indique la veille. |
| `rotational` | Optionnel ; seule la valeur `1` indique un disque rotationnel. |
| `transport` | Optionnel ; sert à distinguer SATA, NVMe et les autres SSD. |

Pour `/var/local/emhttp/devs.ini` :

| Champ | Règle |
| --- | --- |
| section | Nom affiché de l'unité. |
| `device` | Une section sans périphérique est ignorée ; la valeur localise `smart/<device>`. |
| `id` | Obligatoire lorsque `device` existe ; identité stable de la sonde. |
| `temp`, `spundown`, `rotational`, `transport` | Même sémantique que dans `disks.ini`. |

`/var/local/emhttp/var.ini` doit fournir `poll_attributes` sous forme d'un entier
en secondes. Un fichier illisible ou une valeur invalide utilise le fallback de
`30s` décrit plus haut.

## Mise à jour et désinstallation

Une mise à jour du plugin Unraid conserve
`/boot/config/plugins/unraid-vsock-sensors/unraid-vsock-sensors.cfg`. Sa
désinstallation arrête le service et supprime cette configuration ainsi que le
paquet conservé sur la clé USB.

Sur Proxmox, installer une nouvelle version avec :

```sh
apt install ./unraid-vsock-sensors-hwmon_X.Y.Z-N_amd64.deb
```

Le suffixe `-N` est la révision Debian. Pour supprimer le paquet en conservant
sa configuration, ou tout supprimer :

```sh
apt remove unraid-vsock-sensors-hwmon
apt purge unraid-vsock-sensors-hwmon
```

## Développement

Go 1.27.0 ou plus récent et ShellCheck sont nécessaires. Sous Debian ou Ubuntu :

```sh
sudo apt install shellcheck
```

Commandes principales :

```sh
make fmt                     # formate les fichiers Go
make tidy                    # synchronise go.mod et go.sum
make check                   # vérifie formatage, modules, Go, shell et PHP
make test-race               # exécute les tests Go avec le race detector
make fuzz-mpt3               # lance les 5 fuzzers MPT3, 30 s chacun
make build                   # crée bin/unraid-vsock-sensors
make artifacts               # crée les paquets .txz et .deb
make all                     # vérifie puis construit les artefacts locaux
```

`make fuzz-mpt3 FUZZTIME=2m` modifie la durée de chaque campagne. Le fuzzing
long n'appartient pas à `make check` ; les seeds sont néanmoins exécutés par
`go test ./...`.

Sans `VERSION`, la version provient de Git. Une version explicite ne comporte
pas le préfixe `v` :

```sh
make artifacts VERSION=1.4.2
make artifacts VERSION=1.4.3-rc.1
```

Une prerelease Debian utilise `~` (`1.4.3~rc.1-1`) afin de rester antérieure à
la finale. La construction d'artefacts ne modifie jamais le descripteur `.plg`
suivi par Git.

### Tests d'intégration Proxmox

La préparation du template et le fonctionnement du runner sont documentés dans
[`tests/vm/README.md`](tests/vm/README.md). Une fois l'environnement configuré :

```sh
make vm-template-sync    # synchronise le constructeur du template
make vm-template-rebuild # reconstruit le template Proxmox
make test-vm             # parcours complet dans un clone jetable
make test-vm-core        # module, hwmon et cache SMART QEMU
make test-vm-package     # paquet Debian et cycle DKMS
```

Les tests compilent et chargent `virt-temp` dans le clone, exercent les sondes
et le failsafe, puis vérifient installation, upgrade, échec DKMS, récupération,
suppression et purge. La VM est supprimée après succès et conservée après échec.
Les tests du transport VSOCK réel, d'Unraid et des contrôleurs physiques restent
à effectuer dans leurs environnements respectifs.

### Tester un paquet Unraid de développement

Sur une machine où le plugin est déjà installé :

```sh
make unraid-package
scp dist/<paquet-affiché>.txz root@NAS:/tmp/
```

Puis dans Unraid :

```sh
/etc/rc.d/rc.unraid-vsock-sensors stop
upgradepkg --install-new --reinstall /tmp/<paquet-affiché>.txz
/etc/rc.d/rc.unraid-vsock-sensors start
```

Le redémarrage du service est nécessaire pour exécuter le nouveau binaire.
Installer uniquement le `.txz` ne crée pas une installation persistante du
plugin ; une première installation ou un test de démarrage complet doit passer
par un descripteur `.plg`.

## Publier une release

Depuis un worktree propre :

```sh
make release VERSION=X.Y.Z
git diff -- unraid-plugin/unraid-vsock-sensors.plg
git add unraid-plugin/unraid-vsock-sensors.plg
git commit -m "Publie le descripteur Unraid X.Y.Z"
git tag -a vX.Y.Z -m "Release vX.Y.Z"
git push origin main vX.Y.Z
```

`make release` exige une version finale `X.Y.Z`, lance `check`, `test-race` et
les tests VM, construit les artefacts, puis actualise le `.plg` public. Cette
commande ne crée ni commit, ni tag et ne pousse rien.

Le `.plg` peut être actualisé séparément après la construction :

```sh
make update-plg VERSION=X.Y.Z
```

Publier dans la release GitHub :

- `dist/unraid-vsock-sensors-X.Y.Z-x86_64-1.txz` ;
- `dist/unraid-vsock-sensors-hwmon_X.Y.Z-1_amd64.deb`.

Une correction limitée au paquet Debian peut utiliser une nouvelle révision :

```sh
make hwmon-package VERSION=X.Y.Z DEBIAN_REVISION=2
```

## Sécurité du transport

AF_VSOCK n'est pas un mécanisme d'authentification général. L'agent Unraid se
connecte uniquement au CID hôte standard `2`. Le récepteur n'accepte que le CID
configuré et limite chaque snapshot à 1 Mio.

## Références techniques

Le backend HBA natif implémente l'ABI publique de `mpt3sas`, vérifiée contre le
commit Linux
[`8cbaf7b1ab4d`](https://github.com/torvalds/linux/commit/8cbaf7b1ab4dd9ced322b6ebf60b079cc3a3d8d2).
Les fichiers de référence sont
[`mpt3sas_ctl.h`](https://github.com/torvalds/linux/blob/8cbaf7b1ab4dd9ced322b6ebf60b079cc3a3d8d2/drivers/scsi/mpt3sas/mpt3sas_ctl.h),
[`mpt3sas_ctl.c`](https://github.com/torvalds/linux/blob/8cbaf7b1ab4dd9ced322b6ebf60b079cc3a3d8d2/drivers/scsi/mpt3sas/mpt3sas_ctl.c),
[`mpi2_cnfg.h`](https://github.com/torvalds/linux/blob/8cbaf7b1ab4dd9ced322b6ebf60b079cc3a3d8d2/drivers/scsi/mpt3sas/mpi/mpi2_cnfg.h)
et
[`mpt3sas_hwmon.c`](https://github.com/torvalds/linux/blob/8cbaf7b1ab4dd9ced322b6ebf60b079cc3a3d8d2/drivers/scsi/mpt3sas/mpt3sas_hwmon.c).

L'idée d'exposer sur l'hôte une sonde hwmon alimentée depuis une VM vient du
projet GPL-2.0
[`wxxsfxyzm/hdd-temp-monitor`](https://github.com/wxxsfxyzm/hdd-temp-monitor).

## Licence

Le programme Go, les scripts et l'interface Unraid sont sous
[`GPL-3.0-or-later`](LICENSES/GPL-3.0-or-later.txt). Le module noyau
[`virt-temp.c`](virt-temp/module/virt-temp.c) est sous
[`GPL-2.0-only`](LICENSES/GPL-2.0-only.txt). Les dépendances et attributions
figurent dans [`THIRD_PARTY_NOTICES.md`](THIRD_PARTY_NOTICES.md).

## Développement assisté par IA

Une part importante du code et de la documentation a été produite avec
l'assistance d'une IA. Les changements sont néanmoins relus, les chemins
critiques sont testés et le projet est utilisé en production par son auteur.
Tester les paquets dans son propre environnement reste recommandé.

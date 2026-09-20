# Intégration hwmon `virt-temp`

Ce document décrit le composant Proxmox de `unraid-vsock-sensors`.

Pour l'installation complète et la collecte côté Unraid, voir le
[README principal](../README.md).

## Composants

Le paquet Debian installe principalement :

- `/usr/bin/unraid-vsock-sensors` ;
- le module DKMS `virt-temp` ;
- `unraid-vsock-hwmon.service` ;
- `/etc/default/unraid-vsock-hwmon`.

Le module expose deux interfaces complémentaires :

```text
/sys/kernel/config/virt_temp/
├── disk/
└── hba/

/dev/virt-temp/
└── <ID encodé en hexadécimal>
```

Configfs pilote le cycle de vie et les métadonnées des sondes. Chaque sonde
créée obtient ensuite son propre périphérique caractère, utilisé uniquement pour
pousser sa température courante. Les consommateurs continuent à lire les
valeurs via le sous-système Linux `hwmon`.

## Interface kernel

Les familles `disk` et `hba` sont indépendantes.

Une sonde est créée en ajoutant un objet configfs sous la famille correspondante.
Le nom de l'objet est la partie de l'ID stable située après `disk:` ou `hba:`,
encodée en hexadécimal. Son attribut `label` configure le label hwmon.

Exemple conceptuel :

```sh
mkdir /sys/kernel/config/virt_temp/disk/73657269616c31
printf '%s\n' 'disk1' > /sys/kernel/config/virt_temp/disk/73657269616c31/label
printf '%s\n' '42000' > /dev/virt-temp/6469736b3a73657269616c31
```

Le fichier `/dev/virt-temp/<ID hexadécimal>` accepte uniquement un entier signé
en milli°C. L'identité et la famille ne sont pas transportées dans les données :
elles sont déjà portées par le périphérique ouvert. Il n'existe donc plus de
protocole `sample`/`configure`/`commit`, ni de session ou de staging côté noyau.

Supprimer l'objet configfs supprime immédiatement le périphérique hwmon et le
node `/dev` associé. Un descripteur `/dev` déjà ouvert reste mémoire-safe grâce
au refcount du `config_item`, mais ses écritures retournent `ENODEV` après la
suppression.

Contraintes principales :

- famille : `disk` ou `hba` ;
- ID complet : 1 à 84 octets, préfixé par `disk:` ou `hba:` ;
- label : 1 à 95 octets ;
- aucun NUL dans les ID ;
- aucune tabulation, retour à la ligne ou NUL dans les labels ;
- température : entier signé en milli°C, sans borne physique arbitraire.

## Topologie hwmon

Chaque sonde possède son propre périphérique hwmon :

```text
temp1_input
temp1_label
```

Son identité dépend de son ID stable, pas de `hwmonX`, `/dev/sdX` ou de sa
position dans l'inventaire.

Un changement sur une sonde ne décale donc jamais les canaux des autres. Une
modification de topologie crée ou supprime uniquement les objets configfs
concernés. Un changement de label réenregistre seulement le hwmon de cette
sonde.

## Cache et failsafe

Le récepteur conserve la topologie dans :

```text
/var/lib/unraid-vsock-sensors/hwmon-inventory.json
```

Les températures ne sont pas persistées.

Au démarrage, les périphériques peuvent ainsi être recréés immédiatement à la
température failsafe avant que la VM réponde.

Par défaut, une sonde non mise à jour pendant dix secondes passe à :

```text
100000
```

milli°C, soit `100 °C`.

Une nouvelle valeur valide rétablit immédiatement la température.

Le timeout peut être configuré entre 1 et 300 secondes, par exemple :

```sh
printf 'options virt-temp stale_timeout=15\n' \
  > /etc/modprobe.d/virt-temp.conf
```

Puis recharger le module.

## Récupération après erreur

La topologie côté noyau est reconstruite à partir de l'inventaire userspace. Si
le module est déchargé puis rechargé, les anciens chemins configfs et `/dev`
disparaissent. Une écriture qui rencontre `ENOENT` ou `ENODEV` déclenche alors
une réconciliation de la famille concernée.

Une création partiellement réussie reste récupérable : l'appel suivant
réutilise les objets déjà présents, réapplique leur label et recrée les éléments
manquants.

Les consommateurs qui ne suivent pas correctement les changements hwmon peuvent
être relancés automatiquement :

```sh
UNRAID_VSOCK_RESTART_UNITS=coolercontrold.service,fan2go.service
```

Le récepteur utilise `systemctl try-restart` et ne démarre jamais une unité
inactive.

## Installation

```sh
apt update
apt install "proxmox-headers-$(uname -r)" \
  ./unraid-vsock-sensors-hwmon_X.Y.Z-N_amd64.deb
```

Le paquet utilise DKMS et dépend de `proxmox-default-headers` afin de reconstruire
le module lors des mises à jour de noyau.

La configuration existante dans :

```text
/etc/default/unraid-vsock-hwmon
```

est conservée pendant les mises à jour.

## Construction

Depuis la racine du dépôt :

```sh
apt install debhelper rsync
make hwmon-package
```

La construction requiert aussi Go 1.27 ou plus récent dans le `PATH`. Le script
`virt-temp/package.sh` prépare une arborescence source temporaire, puis utilise
`dpkg-buildpackage` et debhelper 13. `dh_installsystemd` installe et active
l'unité sans arrêter le service avant une mise à jour. Les scripts de
maintenance gèrent explicitement DKMS : le `prerm`
généré par `dh_dkms` retire l'ancienne version dès le début d'une mise à jour,
ce qui empêcherait de conserver le module opérationnel si la compilation de la
nouvelle version échouait.

L'arborescence `debian/` permet aussi une construction directe avec
`dpkg-buildpackage -b -us -uc` lorsque les dépendances de construction sont
installées et que `debian/changelog` porte la version voulue. Sous Debian 13,
Go 1.27 doit être installé séparément ; le script de projet utilise alors ce
Go local et passe `-d` à `dpkg-buildpackage` pour ignorer le contrôle des
`Build-Depends` indisponibles dans la distribution.

Version explicite :

```sh
make hwmon-package VERSION=X.Y.Z
```

Nouvelle révision Debian :

```sh
make hwmon-package VERSION=X.Y.Z DEBIAN_REVISION=2
```

Le paquet est créé dans :

```text
dist/unraid-vsock-sensors-hwmon_X.Y.Z-N_amd64.deb
```

## Diagnostic

```sh
dpkg -s unraid-vsock-sensors-hwmon
dkms status -m virt-temp
systemctl status unraid-vsock-hwmon.service
journalctl -u unraid-vsock-hwmon.service -n 100 --no-pager
find /dev/virt-temp -maxdepth 1 -type c -ls
sensors
```

Si les headers du noyau courant manquent :

```sh
apt install "proxmox-headers-$(uname -r)"
apt --fix-broken install
```

Installation interrompue :

```sh
dpkg --configure -a
apt --fix-broken install
```

Lors d'une mise à jour, l'ancienne version DKMS installée et ses sources sont
conservées jusqu'à ce que la nouvelle version ait été construite et installée,
que le module ait été chargé et que le service fonctionne. Si la compilation
échoue, le paquet reste à réparer côté dpkg, mais le dernier module fonctionnel
reste disponible sur disque pour le prochain démarrage. Cela ne restaure pas
les fichiers userspace de l'ancien paquet. Après correction de la cause de
l'échec, terminer l'installation avec `apt --fix-broken install`.

Un `apt remove` du paquet half-configured retire toutes les versions DKMS
`virt-temp` enregistrées (la version cassée et l'ancienne version conservée)
ainsi que leurs sources de secours, tout en conservant la configuration
`/etc/default/unraid-vsock-hwmon` et le cache de topologie.

## Désinstallation

Conserver la configuration :

```sh
apt remove unraid-vsock-sensors-hwmon
```

Tout supprimer :

```sh
apt purge unraid-vsock-sensors-hwmon
```

## Tests

Le vrai module, DKMS, systemd, configfs, les nodes `/dev`, hwmon, le failsafe et
la récupération après reload sont testés dans une VM Proxmox dédiée.

Voir [`../tests/vm/README.md`](../tests/vm/README.md).

## Licence

Le programme userspace est sous `GPL-3.0-or-later`.

Le module noyau `virt-temp` est sous `GPL-2.0-only`.

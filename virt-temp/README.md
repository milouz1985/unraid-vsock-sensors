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

Le module crée :

```text
/dev/virt-temp
```

Le récepteur VSOCK alimente ce périphérique et expose ensuite les températures
via le sous-système Linux `hwmon`.

## Protocole `/dev/virt-temp`

Les familles `disk` et `hba` sont indépendantes.

Le protocole textuel utilise :

```text
sample<TAB><ID><TAB><température en milli°C><TAB><label>
configure<TAB><famille>
commit<TAB><famille>
```

`configure` remplace la topologie d'une famille.

`commit` met uniquement à jour les sondes déjà configurées.

Une session fermée sans opération finale ne modifie rien.

Contraintes principales :

- famille : `disk` ou `hba` ;
- ID : 1 à 84 octets, préfixé par `disk:` ou `hba:` ;
- label : 1 à 95 octets ;
- aucune tabulation ni retour à la ligne dans les ID ou labels ;
- maximum 1 024 enregistrements par session ;
- température : entier signé en milli°C, sans borne physique arbitraire.

## Topologie hwmon

Chaque sonde possède son propre périphérique hwmon :

```text
temp1_input
temp1_label
```

Son identité dépend de son ID stable, pas de `hwmonX`, `/dev/sdX` ou de sa
position dans l'inventaire.

Un changement sur une sonde ne décale donc jamais les canaux des autres.

Une modification d'inventaire ou de label déclenche un nouveau `configure`.

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

Si une nouvelle topologie ne peut pas être enregistrée, le module tente de
restaurer la précédente.

Si le module est déchargé puis rechargé alors que le récepteur tourne encore,
un ancien `commit` retourne `ESTALE`. Le récepteur répond alors par un nouveau
`configure`.

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
make hwmon-package
```

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
ls -l /dev/virt-temp
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

Le vrai module, DKMS, systemd, hwmon, le failsafe et la récupération après
`ESTALE` sont testés dans une VM Proxmox dédiée.

Voir [`../tests/vm/README.md`](../tests/vm/README.md).

## Licence

Le programme userspace est sous `GPL-3.0-or-later`.

Le module noyau `virt-temp` est sous `GPL-2.0-only`.
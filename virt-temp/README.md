# Intégration hwmon virt-temp

Cette intégration publie les températures des disques internes et des HBA d'Unraid
sous forme de sondes Linux `hwmon` natives sur l'hôte Proxmox. Le module crée
deux périphériques : `unraid_storage` regroupe les disques et leurs maximums,
et `unraid_hba` regroupe les contrôleurs. Chaque sonde occupe un canal `tempN`.
`unraid-vsock-sensors hwmon` leur transmet les mesures récupérées par AF_VSOCK
via `/dev/virt-temp`.

Chaque sonde repasse à 100 °C lorsqu'elle n'a reçu aucune mise à jour depuis
10 secondes. L'arrêt de l'agent ou la perte de la connexion VSOCK déclenche
ainsi une valeur de sécurité au lieu de conserver indéfiniment une ancienne
température.

Les disques USB ne sont pas publiés. Les maximums HDD, SATA SSD et NVMe sont
créés lorsqu'au moins deux disques appartiennent au groupe. Le premier
instantané non vide reçu après le démarrage de l'agent configure l'inventaire
de chaque périphérique. Un état initial vide reste en attente afin de ne pas
figer un démarrage incomplet d'Unraid ou de StorCLI. Seul le mode HBA
explicitement `disabled` configure une famille vide. L'inventaire et l'ordre de
ses canaux restent ensuite fixes jusqu'au prochain redémarrage de l'agent.

Une mise à jour ne rafraîchit que les identifiants connus. Si une sonde attendue
disparaît, son canal n'est pas supprimé : son watchdog atteint 100 °C. Le canal
maximum de son groupe n'est pas rafraîchi non plus, afin qu'une courbe utilisant
uniquement ce maximum atteigne également le failsafe. Le journal précise qu'il
faut vérifier la disparition puis redémarrer `unraid-vsock-hwmon.service` si
elle est volontaire. Une nouvelle sonde est signalée mais n'est exposée qu'après
ce redémarrage. Celui-ci constitue donc l'acceptation explicite de la nouvelle
topologie et reconstruit les deux inventaires.

Une famille dont la collecte échoue n'est pas mise à jour ; tous ses canaux
finissent donc au failsafe. Un disque endormi reste présent avec une température
de 0 °C, conformément au cache `disks.ini` d'Unraid.

Une session accepte au maximum 1 024 sondes distinctes avant son opération
finale `configure` ou `commit`.
Cette borne protège les allocations de mémoire noyau contrôlées depuis
l'espace utilisateur ; elle ne représente pas une limite matérielle des HBA.

L'identité d'un HBA utilise en priorité son numéro de série, puis son adresse
SAS, son adresse PCI et enfin son numéro de contrôleur StorCLI.

## Construire le paquet sur la machine de développement

Go et `dpkg-deb` sont nécessaires uniquement sur la machine de développement.
Le script cross-compile le binaire Linux amd64 et crée le paquet Debian dans
`dist/` :

```sh
./virt-temp/package.sh
```

Une release taguée `vX.Y.Z` produit un module DKMS `virt-temp/X.Y.Z`, tandis
qu'une branche de développement conserve son suffixe `-dev`.

## Installer sur Proxmox

Installer les en-têtes du noyau Proxmox courant en même temps que le paquet :

```sh
sudo apt install "proxmox-headers-$(uname -r)" \
  ./unraid-vsock-sensors-hwmon_X.Y.Z-1_amd64.deb
```

`apt` installe les dépendances DKMS, compile et charge le module, puis active le
service systemd. Une configuration existante dans
`/etc/default/unraid-vsock-hwmon` est préservée. Le paquet migre également une
installation réalisée avec l'ancien tarball. Lors d'une mise à jour, installer
simplement le nouveau `.deb` avec la même commande.

## Désinstaller de Proxmox

Retirer le paquet tout en conservant sa configuration :

```sh
sudo apt remove unraid-vsock-sensors-hwmon
```

Utiliser `apt purge` à la place pour supprimer également
`/etc/default/unraid-vsock-hwmon`.

## Publier manuellement la température

```sh
sudo unraid-vsock-sensors hwmon --cid 42 --port 19090 --interval 1s
```

Le paquet active automatiquement le service. Pour changer le CID, modifier sa
configuration puis le redémarrer :

```sh
sudo editor /etc/default/unraid-vsock-hwmon
sudo systemctl restart unraid-vsock-hwmon.service
```

Vérifier les sondes natives et l'agent :

```sh
sensors unraid_storage-* unraid_hba-*
systemctl status unraid-vsock-hwmon.service
```

Après dix secondes sans mise à jour réussie, chaque `temp1_input` existant
retourne `100000` milli-degrés Celsius. Le délai peut être modifié au chargement
du module, par exemple avec `modprobe virt-temp stale_timeout=15`. Les valeurs
acceptées vont de 1 à 300 secondes.

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
instantané valide reçu après le démarrage de l'agent configure l'inventaire de
chaque périphérique. Cet inventaire et l'ordre de ses canaux restent ensuite
fixes jusqu'au prochain redémarrage de l'agent.

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

Go est nécessaire uniquement sur la machine de développement. Le script
cross-compile le binaire Linux et crée dans `dist/` une archive accompagnée de
sa somme SHA-256 :

```sh
./virt-temp/package.sh
```

Une release taguée `v0.3.0` produit un module DKMS `virt-temp/0.3.0`, tandis
qu'une branche de développement conserve son suffixe `-dev`.

## Installer sur Proxmox

Seuls DKMS, les outils de compilation C et les en-têtes du noyau en cours sont
nécessaires sur Proxmox ; Go et Git ne le sont pas :

```sh
sudo apt install dkms build-essential \
  "proxmox-headers-$(uname -r)" lm-sensors
archive="$(find . -maxdepth 1 -name 'unraid-vsock-sensors-hwmon-*-linux-amd64.tar.gz' -print -quit)"
sha256sum -c "$archive.sha256"
tar -xzf "$archive"
cd "${archive%.tar.gz}"
./install.sh
```

L'installateur lit la version incluse dans le paquet, installe les sources
DKMS correspondantes, charge le module et active le service systemd. Une
configuration existante dans `/etc/default/unraid-vsock-hwmon` est préservée.
Lors d'une mise à jour, la nouvelle version est chargée et vérifiée avant que
l'ancienne version du module soit retirée de DKMS.

## Désinstaller de Proxmox

L'installateur pose également une commande de désinstallation durable :

```sh
sudo uninstall-unraid-vsock-hwmon
```

Elle arrête le service, décharge le module, retire son inscription DKMS et
supprime les fichiers installés. La configuration
`/etc/default/unraid-vsock-hwmon` est volontairement conservée.

## Publier manuellement la température

```sh
sudo unraid-vsock-sensors hwmon --cid 42 --port 19090 --interval 1s
```

Pour un fonctionnement continu, installer l'unité systemd fournie et remplacer
ses valeurs par défaut si nécessaire :

```sh
sudo install -m 0644 virt-temp/unraid-vsock-hwmon.service /etc/systemd/system/
printf 'UNRAID_VSOCK_CID=42\nUNRAID_VSOCK_PORT=19090\n' | \
  sudo tee /etc/default/unraid-vsock-hwmon
sudo systemctl daemon-reload
sudo systemctl enable --now unraid-vsock-hwmon.service
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

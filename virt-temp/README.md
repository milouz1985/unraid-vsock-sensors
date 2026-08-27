# Expérimentation hwmon virt-temp

Ce prototype publie les températures des disques internes et des HBA d'Unraid
sous forme de sondes Linux `hwmon` natives sur l'hôte Proxmox. Comme
`drivetemp`, le module noyau crée dynamiquement un périphérique hwmon par
sonde, contenant chacun un unique canal `temp1` ;
`unraid-vsock-sensors hwmon` lui transmet les instantanés récupérés par
AF_VSOCK via `/dev/virt-temp`.

Chaque sonde repasse à 100 °C lorsqu'elle n'a reçu aucune mise à jour depuis
10 secondes. L'arrêt de l'agent ou la perte de la connexion VSOCK déclenche
ainsi une valeur de sécurité au lieu de conserver indéfiniment une ancienne
température.

Les disques USB ne sont pas publiés. Les maximums HDD, SATA SSD et NVMe sont
créés lorsqu'au moins deux disques appartiennent au groupe. Les périphériques
suivent dynamiquement les disques et HBA présents à chaque instantané réussi :
un périphérique disparaît avec sa sonde et retrouve la même identité lorsqu'il
réapparaît.

Comme `drivetemp`, le module crée et supprime chaque périphérique hwmon
indépendamment. Le `commit` termine un inventaire complet afin d'identifier les
sondes disparues, mais ne constitue pas une transaction globale : l'échec
exceptionnel de création d'un périphérique n'annule pas les mises à jour des
autres sondes. L'erreur est renvoyée à l'agent et la sonde concernée est
réessayée à l'instantané suivant.

Une session accepte au maximum 1 024 sondes distinctes avant son `commit`.
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
sensors | sed -n '/virt_temp/,+3p'
systemctl status unraid-vsock-hwmon.service
```

Après dix secondes sans mise à jour réussie, chaque `temp1_input` existant
retourne `100000` milli-degrés Celsius. Le délai peut être modifié au chargement
du module, par exemple avec `modprobe virt-temp stale_timeout=15`. Les valeurs
acceptées vont de 1 à 300 secondes.

# Expérimentation hwmon virt-temp

Ce prototype publie la température maximale des HDD internes d'Unraid sous la
forme d'une sonde Linux `hwmon` native sur l'hôte Proxmox. Le module noyau se
contente de stocker et d'exposer une valeur ; `unraid-vsock-sensors hwmon` la
récupère par AF_VSOCK.

La sonde démarre à 100 °C et repasse à 100 °C lorsqu'elle n'a reçu aucune mise
à jour depuis 10 secondes. L'arrêt de l'agent ou la perte de la connexion VSOCK
déclenche ainsi une valeur de sécurité au lieu de conserver indéfiniment une
ancienne température.

Lorsque le chemin est détecté automatiquement, l'agent retrouve le périphérique
si son numéro `hwmonN` change après un déchargement et rechargement du module.

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

Vérifier séparément la sonde native et l'agent :

```sh
sensors virt_temp-virtual-0
systemctl status unraid-vsock-hwmon.service
```

Après dix secondes sans mise à jour réussie, la lecture de `temp1_input`
retourne `100000` milli-degrés Celsius. Le délai peut être modifié au chargement
du module, par exemple avec `modprobe virt-temp stale_timeout=15`.

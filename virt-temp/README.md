# Expérimentation hwmon virt-temp

Ce prototype publie la température maximale des HDD internes d'Unraid sous la
forme d'une sonde Linux `hwmon` native sur l'hôte Proxmox. Le module noyau se
contente de stocker et d'exposer une valeur ; `unraid-vsock-sensors hwmon` la
récupère par AF_VSOCK.

La sonde démarre à 100 °C et repasse à 100 °C lorsqu'elle n'a reçu aucune mise
à jour depuis 10 secondes. L'arrêt de l'agent ou la perte de la connexion VSOCK
déclenche ainsi une valeur de sécurité au lieu de conserver indéfiniment une
ancienne température.

## Compiler et charger le module

```sh
sudo cp -r virt-temp/virt-temp-0.1.0 /usr/src/
sudo dkms add -m virt-temp -v 0.1.0
sudo dkms build -m virt-temp -v 0.1.0
sudo dkms install -m virt-temp -v 0.1.0
sudo modprobe virt-temp
echo virt-temp | sudo tee /etc/modules-load.d/virt-temp.conf
sensors
```

## Publier la température

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

## Configurer l'auto-complétion de l'éditeur

Après avoir installé les en-têtes du noyau en cours d'exécution sur la machine
de développement, générer `compile_commands.json` :

```sh
make -C virt-temp/virt-temp-0.1.0 compile_commands
```

Cette base contient des chemins propres au noyau et à la machine ; elle est
donc ignorée par Git. Les configurations du dépôt indiquent à clangd et à
l'extension C/C++ de VS Code de la lire à la racine du projet. Il faut la
régénérer après chaque changement ou mise à jour du noyau.

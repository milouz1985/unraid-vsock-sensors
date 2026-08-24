# Plugin CoolerControl natif

Ce service `device` gRPC expose les températures fournies par
`unraid-vsock-sensors` comme un appareil **Unraid Storage** dans CoolerControl.
Il fonctionne sur l'hôte Proxmox et contacte directement la VM Unraid avec
AF_VSOCK.

## Sondes

- Groupes de disques : `hdd`, `ssd` et `nvme` (maximum de chaque famille),
  affichés uniquement lorsque la famille contient au moins deux disques.
- Une sonde par disque et par contrôleur HBA découvert au démarrage de
  CoolerControl.
- Les HBA ne sont pas agrégés, afin que chaque radiateur puisse piloter son
  propre ventilateur avec `hba0`, `hba1`, etc.
- Un disque en spindown est affiché à `0 °C`.
- Une erreur de lecture des disques rend le périphérique temporairement
  indisponible. Une erreur StorCLI masque seulement les mesures HBA.

Le service gRPC n'écoute pas sur TCP. Il utilise exclusivement un socket Unix
local accessible uniquement par son propriétaire ; TLS n'est donc ni nécessaire
ni attendu par le client plugin de CoolerControl.

Les ajouts ou suppressions de disques nécessitent un redémarrage de
`coolercontrold` pour renouveler la liste des sondes.

## Créer et installer le paquet

Go 1.25 ou plus récent est nécessaire sur la machine de compilation :

```sh
make plugin-package
```

L'archive et sa somme SHA-256 sont créées dans `dist/`. Copier l'archive sur
Proxmox, puis :

```sh
tar -xzf unraid-vsock-sensors-cc-0.1.0-linux-amd64.tar.gz
cd unraid-vsock-sensors-cc-0.1.0-linux-amd64
./install.sh --cid=3 --port=990
sudo systemctl restart coolercontrold
```

La version d'un package officiel provient du tag Git courant. Par exemple :

```sh
git tag -a v0.1.0 -m "Release v0.1.0"
make plugin-package
```

En dehors d'un tag exact, le build reçoit automatiquement une version de
développement contenant le nombre de commits et le hash Git. `VERSION=0.1.0`
permet de fournir explicitement une version lors d'un build sans dépôt Git.

Cette installation ne nécessite ni Git ni Go sur Proxmox.

Le plugin est installé dans
`/var/lib/coolercontrol/plugins/unraid-vsock-sensors-cc`. Une réinstallation
sans option conserve sa configuration. Le CID et le port peuvent ensuite être
modifiés dans **Plugins → Unraid VSOCK Sensors**.

Pour diagnostiquer le service :

```sh
journalctl -u cc-plugin-unraid-vsock-sensors-cc
```

## Développement du protocole

Les fichiers `.proto` officiels sont documentés dans
[`proto/UPSTREAM.md`](proto/UPSTREAM.md). Le code Go généré est inclus ; sa
régénération nécessite `protoc`, `protoc-gen-go` et `protoc-gen-go-grpc`, puis
`./generate.sh`.

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
local créé avec les permissions `0600`; TLS n'est donc ni nécessaire ni attendu
par le client plugin de CoolerControl.

Les ajouts ou suppressions de disques nécessitent un redémarrage de
`coolercontrold` pour renouveler la liste des sondes.

## Installer depuis les sources

Prérequis : CoolerControl avec support des plugins et Go 1.25 ou plus récent.
Les fichiers Go générés depuis le protocole gRPC sont inclus dans le dépôt :
`protoc` n'est pas nécessaire pour compiler ou installer le plugin.

Les fichiers `.proto` sont une copie non modifiée de la spécification officielle
CoolerControl. Leur dépôt, commit et licence sont consignés dans
[`proto/UPSTREAM.md`](proto/UPSTREAM.md). Pour régénérer les fichiers Go après
une mise à jour de cette copie, installer `protoc`, `protoc-gen-go` et
`protoc-gen-go-grpc`, puis exécuter `./generate.sh`.

Depuis la racine du dépôt :

```sh
coolercontrol-plugin/install.sh --cid=42 --port=19090
sudo systemctl restart coolercontrold
```

Le plugin est installé par défaut dans
`/var/lib/coolercontrol/plugins/unraid-vsock-sensors-cc`. Les variables
`UNRAID_VSOCK_CID`, `UNRAID_VSOCK_PORT` et `CC_PLUGINS_DIR` permettent aussi de
modifier ces valeurs.

Après installation, le CID et le port peuvent être modifiés dans
**Plugins → Unraid VSOCK Sensors** dans l'interface CoolerControl. Le formulaire
écrit le `config.json` du plugin et redémarre le daemon pour appliquer la
nouvelle connexion. La configuration peut aussi être éditée directement dans :

```text
/var/lib/coolercontrol/plugins/unraid-vsock-sensors-cc/config.json
```

Pour diagnostiquer le service :

```sh
journalctl -u cc-plugin-unraid-vsock-sensors-cc
```

## Installer le paquet précompilé

La machine de compilation peut produire une archive autonome Linux amd64 :

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

Cette installation ne nécessite ni Git ni Go sur Proxmox.

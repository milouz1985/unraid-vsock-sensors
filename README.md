# unraid-fan-control

Expose les températures de disques déjà mises en cache par Unraid à l'hôte
Proxmox via `AF_VSOCK`. Aucun appel SMART n'est effectué, donc l'outil ne
réveille pas les disques en veille.

## Fonctionnement

- Dans la VM Unraid, `serve` lit `/var/local/emhttp/disks.ini` à chaque requête.
- Sur Proxmox, `get` retourne une température seule, directement exploitable
  par un capteur de type commande.
- `hdd`, `ssd` et `nvme` retournent la température maximale du groupe.
- Un nom Unraid (`disk1`, nom de pool) ou un device (`sdb`, `nvme0n1`) permet
  d'interroger un disque indépendamment.
- `hba` retourne la température ROC maximale rapportée par StorCLI ; `hba0`,
  `hba1`, etc. permettent de sélectionner chaque contrôleur.
- Une température absente ou `*` (HDD en veille) est ignorée. La commande
  échoue si aucune température du groupe n'est disponible.

## Compiler

```sh
go build -trimpath -ldflags='-s -w' -o unraid-fan-control .
```

Le même binaire Linux amd64 peut être copié dans la VM et sur l'hôte. Go 1.23
ou plus récent est nécessaire uniquement pour compiler.

## Ajouter vsock à la VM Proxmox

Choisir un CID unique (exemple : `42`) et ajouter le périphérique QEMU à la VM :

```text
args: -device vhost-vsock-pci,guest-cid=42
```

Si la VM possède déjà une ligne `args:`, y ajouter seulement l'option ci-dessus.
Après redémarrage, vérifier `lsmod | grep vsock` dans les deux systèmes.

## Exécuter dans Unraid

```sh
unraid-fan-control serve --port 19090
```

Le serveur exécute la commande fixe
`storcli /cALL show temperature J nolog`. Son résultat est conservé 30 secondes
par défaut, même si plusieurs clients interrogent le serveur.
`--storcli-cache 1m` ajuste le cache.

Le lancement persistant pourra être emballé dans un plugin Unraid ; pour un
premier essai, le script de démarrage `/boot/config/go` suffit.

## Interroger depuis Proxmox

```sh
unraid-fan-control get --cid 42 hdd
unraid-fan-control get --cid 42 nvme
unraid-fan-control get --cid 42 disk1
unraid-fan-control get --cid 42 nvme0n1
unraid-fan-control get --cid 42 hba
unraid-fan-control get --cid 42 hba0
unraid-fan-control get --cid 42 --json
```

Les commandes autres que `--json` écrivent uniquement un nombre en degrés
Celsius. Elles conviennent donc à une source `cmd` de fan2go ou CoolerControl.

## Fraîcheur et sécurité

La fraîcheur des disques dépend de `Tunable (poll_attributes)` dans les réglages disque
d'Unraid. Avec 30 secondes, une commande exécutée plus souvent renverra
simplement la même valeur mise en cache. L'outil ne lance volontairement jamais
`smartctl`.

La température HBA ne vient pas d'Unraid : elle correspond au champ
`ROC temperature(Degree Celsius)` de StorCLI. Une sonde absente, une commande
en erreur ou dépassant dix secondes rend uniquement la famille HBA
indisponible ; les températures des disques restent utilisables.

Le transport vsock n'est pas un mécanisme d'authentification. Le serveur accepte
uniquement `GET`, ne reçoit aucun chemin ni commande, limite les requêtes à 1 Kio
et ne renvoie que le contenu structuré attendu.

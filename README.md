# unraid-vsock-sensors

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
- Pour un HDD en veille (`temp="*"`), le serveur renvoie `0 °C`, qui signifie
  que la sonde est inactive. Le spindown ne provoque donc pas une fausse alerte
  à `100 °C` dans CoolerControl et aucune ancienne mesure ne reste figée.
- Les slots Unraid non assignés (`DISK_NP`) et la clé de démarrage `flash` ne
  sont pas exposés comme des sondes.
- Une vraie erreur de lecture ou une température invalide fait toujours échouer
  la commande, afin que CoolerControl puisse la traiter comme une panne.

## Compiler

```sh
make build
```

Le binaire statique est créé dans `bin/unraid-vsock-sensors`.

Le même binaire Linux amd64 peut être copié dans la VM et sur l'hôte. Go 1.25
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
unraid-vsock-sensors serve --port 19090
```

Le serveur exécute en arrière-plan la commande fixe
`storcli /cALL show temperature J nolog`, immédiatement au démarrage puis toutes
les 30 secondes par défaut. Les requêtes utilisent uniquement le dernier état
en mémoire et n'attendent donc jamais StorCLI. `--storcli-cache 1m` ajuste
l'intervalle de rafraîchissement.

Le lancement persistant pourra être emballé dans un plugin Unraid ; pour un
premier essai, le script de démarrage `/boot/config/go` suffit.

## Interroger depuis Proxmox

```sh
unraid-vsock-sensors get --cid 42 hdd
unraid-vsock-sensors get --cid 42 nvme
unraid-vsock-sensors get --cid 42 disk1
unraid-vsock-sensors get --cid 42 nvme0n1
unraid-vsock-sensors get --cid 42 hba
unraid-vsock-sensors get --cid 42 hba0
unraid-vsock-sensors get --cid 42 --json
```

`--json` renvoie toujours la réponse structurée complète, y compris les champs
`error` et `hba_error`, afin de permettre le diagnostic d'une famille de sondes
sans masquer les mesures encore disponibles dans l'autre.

Les commandes autres que `--json` écrivent uniquement un nombre en degrés
Celsius. Elles conviennent donc à une source `cmd` de fan2go ou CoolerControl.

## Plugin CoolerControl natif

Un service gRPC natif est disponible dans `coolercontrol-plugin`. Contrairement
à une collection de sondes `cmd`, il effectue une seule requête vsock par cycle
et présente automatiquement les groupes, les disques et les HBA comme un
appareil **Unraid Storage**. Consultez
[`coolercontrol-plugin/README.md`](coolercontrol-plugin/README.md) pour
l'installation.

## Fraîcheur et sécurité

La fraîcheur des disques dépend de `Tunable (poll_attributes)` dans les réglages
disque d'Unraid. Avec 30 secondes, une commande exécutée plus souvent renverra
simplement la même valeur mise en cache. L'outil ne lance volontairement jamais
`smartctl`.

La température HBA ne vient pas d'Unraid : elle correspond au champ
`ROC temperature(Degree Celsius)` de StorCLI. En cas d'erreur, la dernière
mesure valide reste disponible pendant deux tentatives supplémentaires, puis
est supprimée au troisième échec consécutif. Un succès remet ce compteur à
zéro. Une sonde absente ou une commande en erreur ne concerne que la famille
HBA ; les températures des disques restent utilisables.

Le transport vsock n'est pas un mécanisme d'authentification. Le serveur accepte
uniquement les connexions provenant du CID hôte standard `2`. Il accepte
uniquement `GET`, ne reçoit aucun chemin ni commande, limite les requêtes à
1 Kio et ne renvoie que le contenu structuré attendu.

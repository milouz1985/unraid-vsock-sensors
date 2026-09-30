# Intégration hwmon `virt-temp`

Ce document décrit le composant Proxmox de `unraid-vsock-sensors`.

Pour l'installation complète et la collecte côté Unraid, voir le
[README principal](../README.md).

La construction et la release sont documentées dans
[`CONTRIBUTING.md`](../CONTRIBUTING.md), et les tests réels dans
[`tests/vm/README.md`](../tests/vm/README.md).

## Composants

Le paquet Debian installe principalement :

- `/usr/bin/unraid-vsock-sensors` ;
- le module DKMS `virt-temp` ;
- `unraid-vsock-hwmon.service` ;
- `unraid-vsock-hwmon-topology.path` ;
- `unraid-vsock-hwmon-topology.service` ;
- `unraid-vsock-hwmon-restart@.service` ;
- `/etc/default/unraid-vsock-hwmon`.

Le module expose deux interfaces complémentaires :

```text
/sys/kernel/config/virt_temp/
├── disk/
└── hba/

/dev/virt-temp/
└── <ID encodé en hexadécimal>
```

Configfs pilote le cycle de vie et les métadonnées des sondes. Chaque sonde
créée obtient ensuite son propre périphérique caractère, utilisé uniquement pour
pousser sa température courante. Les consommateurs continuent à lire les
valeurs via le sous-système Linux `hwmon`.

Quand `unraid-vsock-hwmon` fonctionne, il est l'unique writer supporté de
`/sys/kernel/config/virt_temp`. Ce modèle single-writer est un contrat
architectural, pas un verrou empêchant `root` de modifier configfs : le module
n'ajoute ni token d'ownership ni détection périodique des modifications
extérieures.

## Interface kernel

Les familles `disk` et `hba` sont indépendantes.

Une sonde est créée en ajoutant un objet configfs sous la famille correspondante.
Le nom de l'objet est la partie de l'ID stable située après `disk:` ou `hba:`,
encodée en hexadécimal. Son attribut `label` configure le label hwmon.

Exemple manuel, réservé au développement, au diagnostic ou aux tests avec
`unraid-vsock-hwmon` arrêté :

```sh
mkdir /sys/kernel/config/virt_temp/disk/73657269616c31
printf '%s\n' '42000' > /dev/virt-temp/6469736b3a73657269616c31
printf '%s\n' 'disk1' > /sys/kernel/config/virt_temp/disk/73657269616c31/label
```

Le fichier `/dev/virt-temp/<ID hexadécimal>` accepte uniquement un entier signé
en milli°C. L'identité et la famille ne sont pas transportées dans les données :
elles sont déjà portées par le périphérique ouvert. Il n'existe donc plus de
protocole `sample`/`configure`/`commit`, ni de session ou de staging côté noyau.

Supprimer l'objet configfs supprime immédiatement le périphérique hwmon et le
node `/dev` associé. Un descripteur `/dev` déjà ouvert reste mémoire-safe grâce
au refcount du `config_item`, mais ses écritures retournent `ENODEV` après la
suppression.

Contraintes principales :

- famille : `disk` ou `hba` ;
- ID complet : 1 à 84 octets, préfixé par `disk:` ou `hba:` ;
- label : 1 à 95 octets ;
- aucun NUL dans les ID ;
- aucune tabulation, retour à la ligne ou NUL dans les labels ;
- température : entier signé en milli°C, sans borne physique arbitraire.

## Topologie hwmon

Chaque sonde possède son propre périphérique hwmon :

```text
temp1_input
temp1_label
```

Son identité dépend de son ID stable, pas de `hwmonX`, `/dev/sdX` ou de sa
position dans l'inventaire.

Un changement sur une sonde ne décale donc jamais les canaux des autres. Une
modification de topologie crée ou supprime uniquement les objets configfs
concernés. Un changement de label réenregistre seulement le hwmon de cette
sonde.

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

La topologie côté noyau est reconstruite à partir de l'inventaire userspace. Si
le module est déchargé puis rechargé, les anciens chemins configfs et `/dev`
disparaissent. Une écriture qui rencontre `ENOENT` ou `ENODEV` déclenche alors
une réconciliation de la famille concernée.

Une création partiellement réussie reste récupérable : l'appel suivant
réutilise les objets déjà présents, réapplique leur label et recrée les éléments
manquants.

## Notification des changements de topologie

Après une réconciliation complète qui ajoute, retire ou renomme une sonde, le
récepteur modifie un fichier runtime vide :

```text
/run/unraid-vsock-sensors/topology-changed
```

L'événement est également émis au premier snapshot reçu de la VM lorsqu'une
topologie a été restaurée depuis le cache, même si aucune sonde n'est ajoutée,
retirée ou renommée. Il reste différé tant qu'une famille nécessite encore une
réconciliation.

Ce fichier ne contient ni nom d'unité, ni commande, ni donnée de sonde.
`unraid-vsock-hwmon-topology.path` transforme sa modification en activation de
`unraid-vsock-hwmon-topology.service`. Ce oneshot vide constitue le point
d'abonnement public :

```text
receiver → topology-changed → topology.path → topology.service → abonnés
```

Le récepteur ne connaît aucun consommateur. Il ne scanne pas les unités
installées et ne communique pas avec systemd.

Le fichier d'événement est conservé pendant les arrêts et redémarrages du
receiver afin que sa suppression ne soit jamais interprétée comme un changement
de topologie. Il disparaît naturellement avec `/run` au redémarrage de l'hôte,
ou lors de la purge du paquet après l'arrêt de l'unité `.path`.

Pour redémarrer uniquement s'il est déjà actif un service fictif
`foo.service`, utiliser son nom simple, sans suffixe de type, comme instance :

```sh
systemctl enable unraid-vsock-hwmon-restart@foo.service
```

L'instance `unraid-vsock-hwmon-restart@foo.service` appelle
`systemctl try-restart --no-block -- foo.service`. Ce template vise les services
classiques non instanciés. Une unité qui nécessite un nom plus complexe ou une
autre réaction peut fournir son propre oneshot avec :

```ini
[Install]
WantedBy=unraid-vsock-hwmon-topology.service
```

L'administrateur ou le logiciel concerné choisit ainsi librement un reload,
rescan, refresh ou restart. Le paquet ne crée aucun abonnement en fonction des
logiciels détectés.

## Confinement systemd

Le service reste lancé en `root` parce que l'interface actuelle de `virt-temp`
réserve à root l'administration complète de configfs et l'écriture des
miscdevices. Son processus principal ne conserve néanmoins que
`CAP_NET_BIND_SERVICE`, nécessaire au port VSOCK privilégié `990`.

L'unité limite ses sockets à `AF_VSOCK`. Le receiver ne peut donc ouvrir ni
socket D-Bus, ni socket privée systemd, et n'invoque jamais `systemctl`. Dans
`/run`, ses écritures sont limitées au `RuntimeDirectory` que systemd lui
attribue ; le code n'y modifie que son fichier d'événement. Les abonnements et
symlinks `.wants` restent définis par root indépendamment du receiver. Un
receiver compromis peut déclencher répétitivement les abonnés déjà autorisés,
mais ne peut pas en choisir ou en créer de nouveaux.

`ProtectSystem=strict` rend le reste du système de fichiers non modifiable, les
répertoires personnels sont masqués et les écritures persistantes sont limitées
au `StateDirectory`. Les interfaces kernel nécessaires restent accessibles sous
`/sys/kernel/config/virt_temp` et `/dev/virt-temp`, avec un `/tmp` privé. Le
`modprobe` exécuté avant le daemon reste explicitement privilégié afin que le
démarrage à froid continue à charger le module.

Le cache par défaut se trouve dans le répertoire d'état autorisé. Si
`UNRAID_VSOCK_CACHE` désigne un autre répertoire, celui-ci doit également être
autorisé dans un drop-in :

```ini
[Service]
ReadWritePaths=/chemin/du/cache
```

Puis appliquer le changement :

```sh
systemctl daemon-reload
systemctl restart unraid-vsock-hwmon.service
```

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

`UNRAID_VSOCK_RESTART_UNITS` n'est plus pris en charge. Une ligne existante est
conservée dans ce fichier mais reste sans effet : le receiver ne contrôle plus
les unités consommatrices. Après une mise à jour, activer explicitement chaque
abonnement nécessaire, par exemple :

```sh
systemctl enable unraid-vsock-hwmon-restart@coolercontrold.service
```

CoolerControl n'est ici qu'un exemple, pas un consommateur géré par UVSS. Le
template utilise `try-restart` : lors d'un événement, une unité inactive reste
inactive.

## Diagnostic

```sh
dpkg -s unraid-vsock-sensors-hwmon
dkms status -m virt-temp
systemctl status unraid-vsock-hwmon.service
systemctl status unraid-vsock-hwmon-topology.path
journalctl -u unraid-vsock-hwmon.service -n 100 --no-pager
find /dev/virt-temp -maxdepth 1 -type c -ls
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

Lors d'une mise à jour, l'ancienne version DKMS installée et ses sources sont
conservées jusqu'à ce que la nouvelle version ait été construite et installée,
que le module ait été chargé et que le service fonctionne. Si la compilation
échoue, le paquet reste à réparer côté dpkg, mais le dernier module fonctionnel
reste disponible sur disque pour le prochain démarrage. Cela ne restaure pas
les fichiers userspace de l'ancien paquet. Après correction de la cause de
l'échec, terminer l'installation avec `apt --fix-broken install`.

Le downgrade en place d'une version 3.x vers une version 2.x antérieure à
l'interface configfs n'est pas supporté. Les anciens scripts de maintenance ne
savent pas supprimer les objets configfs créés par la version 3.x ; tenter ce
downgrade peut donc laisser le paquet dans un état half-configured.

Un `apt remove` du paquet half-configured retire toutes les versions DKMS
`virt-temp` enregistrées (la version cassée et l'ancienne version conservée)
ainsi que leurs sources de secours, tout en conservant la configuration
`/etc/default/unraid-vsock-hwmon` et le cache de topologie.

## Désinstallation

Un processus extérieur qui conserve ouvert le FD d'une sonde retient le module.
Dans ce cas, la tentative de suppression du paquet retire les objets configfs
mais échoue proprement au déchargement de `virt_temp`. Si le service était actif,
le `prerm` tente de le relancer afin qu'il restaure la topologie depuis le cache ;
l'ancien FD retourne néanmoins `ENODEV`. Après fermeture du FD, relancer la
suppression permet de terminer le retrait. Les scripts du paquet ne tuent pas le
processus extérieur pour forcer l'opération.

Conserver la configuration :

```sh
apt remove unraid-vsock-sensors-hwmon
```

Les abonnements créés avec `unraid-vsock-hwmon-restart@.service` appartiennent à
l'administrateur et sont conservés. Les désactiver explicitement avant la
désinstallation s'ils ne sont plus nécessaires.

Purger la configuration et le cache :

```sh
apt purge unraid-vsock-sensors-hwmon
```

La purge retire la configuration et le cache. Elle ne retire spécialement ni
les abonnements au template UVSS, ni les unités tierces directement abonnées à
`unraid-vsock-hwmon-topology.service` : ces abonnements explicites restent sous
la responsabilité de l'administrateur.

## Licence

Le programme userspace est sous `GPL-3.0-or-later`.

Le module noyau `virt-temp` est sous `GPL-2.0-only`.

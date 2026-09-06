# Intégration hwmon `virt-temp`

Ce document décrit le composant Proxmox de `unraid-vsock-sensors`. Pour la
procédure complète, depuis la configuration VSOCK jusqu'au plugin Unraid,
consulter le [README principal](../README.md).

## Composants installés

Le paquet Debian `unraid-vsock-sensors-hwmon` installe :

- `/usr/bin/unraid-vsock-sensors`, le récepteur VSOCK et agent hwmon ;
- `/usr/src/virt-temp-X.Y.Z`, les sources du module DKMS ;
- `/usr/lib/systemd/system/unraid-vsock-hwmon.service` ;
- `/usr/lib/modules-load.d/virt-temp.conf` ;
- un modèle de configuration dans
  `/usr/share/unraid-vsock-sensors-hwmon/`.

Au premier démarrage, `/etc/default/unraid-vsock-hwmon` est créé seulement s'il
n'existe pas. Une configuration existante n'est jamais remplacée.
L'inventaire persistant est stocké par défaut dans
`/var/lib/unraid-vsock-sensors/hwmon-inventory.json`.

L'agent Unraid ouvre une connexion VSOCK persistante vers ce récepteur. Il
pousse séparément les familles disque et HBA après leurs collectes, puis envoie
un heartbeat léger chaque seconde. Le récepteur refuse les connexions qui ne
viennent pas du CID configuré, maintient `/dev/virt-temp` à jour et expose le
dernier snapshot sur `/run/unraid-vsock-sensors/sensors.sock` pour la commande
locale `get`.

## Fonctionnement du pilote

Le module crée `/dev/virt-temp`. Le récepteur y envoie séparément les familles
`disk` et `hba` :

1. `configure` crée l'inventaire et les canaux d'une famille ;
2. `commit` actualise uniquement les identifiants déjà configurés ;
3. une fermeture sans opération finale ne modifie rien.

Le protocole textuel utilise un discriminant explicite dans son premier champ :

```text
sample<TAB><ID stable><TAB><température en milli°C><TAB><label>
configure<TAB><famille>
commit<TAB><famille>
```

Les tabulations et retours à la ligne sont interdits dans les ID et labels.
Chaque ligne est transmise par une écriture distincte sur la même session.
Un ID doit contenir entre 1 et 63 octets et commencer par le nom de sa famille
suivi de `:`. Un label doit contenir entre 1 et 95 octets. Une température doit
être un entier compris entre `0` et `150000` milli°C inclus. Les seules familles
acceptées sont `disk` et `hba`, et les seules opérations finales sont
`configure` et `commit`.

Chaque session accepte au maximum 1 024 enregistrements. Cette limite borne les
allocations contrôlées depuis l'espace utilisateur ; elle ne correspond pas à
une limite matérielle.

Les inventaires `disk` et `hba` possèdent chacun leur propre mutex. Deux
producteurs peuvent ainsi actualiser des familles différentes sans se bloquer.
Au sein d'une famille, le mutex sérialise les `commit` avec un éventuel
`configure`. Les lectures sysfs n'en ont pas besoin : la désinscription hwmon
attend la fin des lectures en cours avant que l'ancien inventaire soit libéré.

Le pilote expose au maximum deux périphériques hwmon :

- `unraid_storage`, avec les disques internes et leurs maximums de groupe ;
- `unraid_hba`, avec les contrôleurs HBA transmis par la VM.

Chaque sonde correspond à un canal `tempN_input` accompagné de
`tempN_label`. Les ID stables restent internes au protocole ; sysfs expose le
label configuré au démarrage. Pour un HBA, ce label utilise le modèle et
l'adresse PCI lorsqu'ils sont disponibles. Les indices locaux IOC et StorCLI
ne font pas partie de l'identité publiée.

## Inventaire persistant et failsafe

Après un premier relevé valide, le récepteur met en cache les ID, labels et
groupes, mais jamais les températures. Au démarrage suivant, ce cache recrée
les canaux à `100 °C` avant que la VM réponde. Un logiciel de ventilation peut
donc les découvrir dès le boot de Proxmox.

Un snapshot valide dont les ID diffèrent remplace automatiquement la famille
concernée et le cache. Une erreur globale de lecture ne constitue pas une
nouvelle topologie : les anciens canaux restent alors en place et atteignent le
failsafe. Une température de disque indisponible n'interrompt pas les autres
mises à jour : ce disque et le maximum de sa catégorie reçoivent explicitement
la température failsafe, tandis que les autres canaux restent actualisés.
L'agent Unraid collecte les températures SMART en arrière-plan et masque une
erreur transitoire pendant l'intervalle SMART configuré augmenté de cinq
secondes, sans déclarer la sonde en panne avant le prochain relevé configuré.
Un changement de label seul n'affecte pas l'identité.

Si l'enregistrement d'une nouvelle topologie échoue, le pilote réenregistre
l'inventaire précédent au lieu de laisser disparaître les sondes. L'erreur est
retournée au récepteur, qui retente la nouvelle configuration, tandis que les
anciens canaux non actualisés atteignent naturellement le failsafe.

Si le module `virt_temp` est déchargé puis rechargé sans redémarrer le
récepteur, le premier `commit` retourne `ESTALE` parce que le noyau a perdu son
inventaire.
Le récepteur répond par une unique opération `configure`, restaure les canaux et
notifie les consommateurs comme lors de tout changement de topologie.

Une erreur HBA invalide le relevé complet. L'inventaire précédent reste en
place sans être actualisé et atteint donc le failsafe. StorCLI tente auparavant
une redécouverte unique lorsque la topologie ou l'ensemble des contrôleurs a
changé.

Les logiciels qui n'observent pas les ajouts hwmon à chaud peuvent être
relancés une première fois lorsque la VM répond, même si l'inventaire restauré
est inchangé, puis après chaque reconfiguration. La liste est optionnelle et
générique :

```sh
UNRAID_VSOCK_RESTART_UNITS=coolercontrold.service
# ou
UNRAID_VSOCK_RESTART_UNITS=coolercontrold.service,fan2go.service
```

Le récepteur utilise `systemctl try-restart` : une unité absente ou inactive n'est
pas démarrée. La valeur reste vide par défaut.

Le récepteur suit un TTL distinct pour les familles disque et HBA. Chaque état
transporte la durée de validité restante de sa collecte, que Proxmox mémorise
jusqu'au prochain état de cette famille. Un collecteur bloqué expire donc même
si la connexion reste active. Une erreur explicite ramène son échéance à trois
secondes ; trois secondes sans heartbeat font expirer les deux familles à
`100000` millidegrés Celsius.

Chaque canal applique en plus le même failsafe après 10 secondes sans mise à
jour. Ce second délai, géré dans le module, protège encore le système si le
récepteur lui-même s'arrête. Il est configurable entre 1 et 300 secondes. Par
exemple, pour utiliser 15 secondes de manière persistante :

```sh
printf 'options virt-temp stale_timeout=15\n' \
  > /etc/modprobe.d/virt-temp.conf
systemctl stop unraid-vsock-hwmon.service
modprobe -r virt_temp
modprobe virt_temp
systemctl start unraid-vsock-hwmon.service
```

Décharger le module supprime momentanément les sondes hwmon. Arrêter au préalable
les logiciels qui les utilisent si nécessaire.

## Construire le paquet Debian

Depuis la racine du dépôt :

```sh
make hwmon-package
```

Pour une version de release explicite :

```sh
make hwmon-package VERSION=X.Y.Z
```

Pour republier uniquement une correction du packaging :

```sh
make hwmon-package VERSION=X.Y.Z DEBIAN_REVISION=2
```

Le résultat est :

```text
dist/unraid-vsock-sensors-hwmon_X.Y.Z-N_amd64.deb
```

La version Debian `X.Y.Z-N` contient la version applicative `X.Y.Z` et la
révision de packaging `N`. Un build Git postérieur à une release est converti
en snapshot Debian avec `+dev` : `1.4.3+dev.N.gHASH-1` est postérieur à
`1.4.3-1`, mais reste antérieur à `1.4.4-1` et à `1.5.0-1`.

## Installer et mettre à jour

Sur Proxmox, en tant que `root` :

```sh
apt update
apt install "proxmox-headers-$(uname -r)" \
  ./unraid-vsock-sensors-hwmon_X.Y.Z-N_amd64.deb
```

Lors d'une mise à jour, le `prerm` retire l'enregistrement DKMS de la version
installée avant que `dpkg` supprime ses sources versionnées. Le `postinst`
enregistre ensuite les nouvelles sources, compile et installe le module pour
chaque noyau déjà présent dont les en-têtes sont disponibles, le signe lorsque
DKMS est configuré pour le faire, charge `virt_temp` et démarre le service.
L'unité exécute également `modprobe virt_temp` avant chaque démarrage : elle ne
peut donc plus rester active sans `/dev/virt-temp` après un reboot.

Le paquet dépend de `proxmox-default-headers`. Ce méta-paquet installe les
headers de chaque nouveau noyau Proxmox par défaut et permet à DKMS de
recompiler automatiquement `virt-temp`. La commande d'installation demande en
plus les headers de `$(uname -r)` pour couvrir le noyau actuellement démarré,
qui peut être plus ancien après une mise à jour effectuée avant le reboot.

Une mise à jour utilise la même commande avec le nouveau `.deb`. Le paquet
migre automatiquement les fichiers posés par l'ancien installateur tarball.

## Diagnostiquer

```sh
dpkg --audit
dpkg -s unraid-vsock-sensors-hwmon
dkms status -m virt-temp
systemctl status unraid-vsock-hwmon.service
journalctl -u unraid-vsock-hwmon.service -n 100 --no-pager
ls -l /dev/virt-temp
sensors
```

Si l'installation a été interrompue :

```sh
dpkg --configure -a
apt --fix-broken install
```

Si les headers du noyau actif manquent :

```sh
apt install "proxmox-headers-$(uname -r)"
apt --fix-broken install
```

## Désinstaller

Conserver `/etc/default/unraid-vsock-hwmon` :

```sh
apt remove unraid-vsock-sensors-hwmon
```

Supprimer également la configuration :

```sh
apt purge unraid-vsock-sensors-hwmon
```

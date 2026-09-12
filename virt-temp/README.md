# Intégration hwmon `virt-temp`

Ce document décrit le composant Proxmox de `unraid-vsock-sensors`. Pour la
procédure complète, depuis la configuration VSOCK jusqu'au plugin Unraid,
consulter le [README principal](../README.md). La collecte des températures
côté Unraid, la fraîcheur du cache SMART et l'architecture globale y sont
documentées afin de conserver une seule source de vérité.

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
pousse chaque seconde son dernier snapshot complet, qui sert aussi de heartbeat.
Le récepteur refuse les connexions qui ne viennent pas du CID configuré et
maintient `/dev/virt-temp` à jour.

## Fonctionnement du pilote

Le module crée `/dev/virt-temp`. Le récepteur y envoie séparément les familles
`disk` et `hba` :

1. `configure` crée un périphérique hwmon par sonde de la famille ;
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
Un ID doit contenir entre 1 et 84 octets et commencer par le nom de sa famille
suivi de `:`. Un label doit contenir entre 1 et 95 octets. Une température doit
être un entier signé en milli°C représentable par le noyau ; aucune plage de
plausibilité matérielle n'est imposée. Les seules familles acceptées sont `disk`
et `hba`, et les seules opérations finales sont `configure` et `commit`.
`configure` enregistre le label ; `commit` identifie les sondes uniquement par
leur ID et ignore le label transmis.

Chaque session accepte au maximum 1 024 enregistrements. Cette limite borne les
allocations contrôlées depuis l'espace utilisateur ; elle ne correspond pas à
une limite matérielle.

Les inventaires `disk` et `hba` possèdent chacun leur propre mutex. Deux
producteurs peuvent ainsi actualiser des familles différentes sans se bloquer.
Au sein d'une famille, le mutex sérialise les `commit` avec un éventuel
`configure`. Les lectures sysfs n'en ont pas besoin : la désinscription hwmon
attend la fin des lectures en cours avant que l'ancien inventaire soit libéré.

Le pilote expose un périphérique hwmon indépendant par sonde. Son nom lisible
est dérivé du label, par exemple `unraid_disk1`, `unraid_hdd_maximum` ou
`unraid_sas3008`. Son unique mesure est donc toujours `temp1_input`, accompagnée
du label complet dans `temp1_label`. Le parent platform encode l'ID stable en
hexadécimal : l'identité du périphérique ne dépend ni du nom hwmon, ni de
`hwmonX`, ni de l'ordre des autres sondes. Pour un HBA, le label utilise le
modèle et l'adresse PCI lorsqu'ils sont disponibles. Les indices locaux IOC et
StorCLI ne font pas partie de l'identité publiée.

Ce choix évite d'associer durablement une sonde à une position `tempN` dans un
périphérique agrégé. Une modification de topologie pourrait autrement décaler
les canaux et faire lire à un consommateur la température d'un autre disque.
Réserver les anciens canaux empêcherait ce décalage, mais conserverait des
sondes fantômes à `100 °C` après un retrait planifié. Avec un périphérique par
ID, chaque sonde reste `temp1`, tandis qu'un ID retiré disparaît sans modifier
l'identité des autres périphériques.

Ce modèle remplace les anciens périphériques agrégés `unraid_storage` et
`unraid_hba`. La première mise à niveau nécessite donc de sélectionner les
nouvelles sources dans CoolerControl ou d'adapter les `platform` fan2go.

## Inventaire persistant et failsafe

Après un premier relevé valide, le récepteur met en cache les ID, labels et
groupes, mais jamais les températures. Au démarrage suivant, ce cache recrée
les périphériques à `100 °C` avant que la VM réponde. Un logiciel de ventilation
peut donc les découvrir dès le boot de Proxmox.

Chaque ID stable possède son propre périphérique et reste toujours `temp1`.
Lorsqu'un snapshot modifie l'inventaire ou un label, `configure` recrée tous les
périphériques de la famille. Leurs noms platform et leurs identités restent
stables, mais leurs numéros dynamiques `hwmonX` peuvent changer. Un ID absent du
nouvel inventaire est retiré du cache et aucune autre sonde ne récupère son
identité. Un ancien périphérique restauré depuis le cache reste temporairement
au failsafe jusqu'au premier snapshot valide, qui le retire si le matériel a
réellement été supprimé.

Un snapshot signalant une erreur globale ne constitue pas une nouvelle
topologie : les anciens périphériques restent alors en place et atteignent le
failsafe. Une sonde configurée mais omise d'un `commit` conserve sa dernière
valeur jusqu'à l'expiration de `stale_timeout`, puis retourne `100 °C`, sans
interrompre les autres mises à jour. Une nouvelle configuration initialise
directement les sondes indisponibles au failsafe. Les règles qui déterminent
cette indisponibilité côté Unraid sont décrites dans le README principal. Un
changement de label seul n'affecte pas l'identité, mais reconfigure la famille
afin d'actualiser `temp1_label`, le nom hwmon et le cache.

Si l'enregistrement d'une nouvelle topologie échoue, le pilote tente de
réenregistrer l'inventaire précédent au lieu de laisser disparaître les sondes.
L'erreur est retournée au récepteur, qui retente la nouvelle configuration,
tandis que les anciens périphériques non actualisés atteignent naturellement le
failsafe. Si cette restauration échoue également, les `commit` suivants
retournent `ESTALE` afin de forcer une nouvelle configuration.

Si le module `virt_temp` est déchargé puis rechargé sans redémarrer le
récepteur, le premier `commit` retourne `ESTALE` parce que le noyau a perdu son
inventaire.
Le récepteur répond par une unique opération `configure`, restaure les
périphériques et notifie les consommateurs comme lors de tout changement de
topologie.

Les logiciels qui n'observent pas les ajouts hwmon à chaud peuvent être
relancés une première fois lorsque la VM répond, même si l'inventaire restauré
est inchangé, puis après chaque reconfiguration. La liste est optionnelle et
générique :

```sh
UNRAID_VSOCK_RESTART_UNITS=coolercontrold.service
# ou
UNRAID_VSOCK_RESTART_UNITS=coolercontrold.service,fan2go.service
```

Les motifs systemd ne sont pas acceptés et le récepteur ne peut pas se désigner
lui-même dans cette liste.

Le récepteur utilise `systemctl try-restart` : une unité absente ou inactive n'est
pas démarrée. Si `systemctl` ne parvient pas à mettre la demande en file
d'attente, une nouvelle tentative est programmée 30 secondes après chaque
échec. La valeur reste vide par défaut.

Chaque canal applique un failsafe de `100000` millidegrés Celsius après
10 secondes sans mise à jour. Ce délai, géré dans le module, couvre aussi une
perte du flux VSOCK ou l'arrêt du récepteur. Il est configurable entre 1 et
300 secondes. Le délai de lecture VSOCK de trois secondes sert uniquement à
détecter une connexion interrompue et à permettre sa reconnexion ; il ne
remplace pas ce failsafe thermique. Par exemple, pour utiliser 15 secondes de
manière persistante :

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
révision de packaging `N`. Une prerelease `1.4.3-rc.1` devient
`1.4.3~rc.1-1`, donc reste antérieure à la finale `1.4.3-1`. Un build Git
postérieur à une version utilise `+dev` : `1.4.3+dev.N.gHASH-1` est postérieur
à `1.4.3-1`, mais reste antérieur à `1.4.4-1` et à `1.5.0-1`. Après un tag RC,
un snapshot tel que `1.4.3-rc.1-dev.N.gHASH` devient
`1.4.3~rc.1+dev.N.gHASH-1`.

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

## Licence

Le programme userspace est distribué sous `GPL-3.0-or-later`. Le module noyau
`virt-temp` est un programme séparé distribué sous `GPL-2.0-only`. Les licences
et notices tierces complètes sont installées dans
`/usr/share/doc/unraid-vsock-sensors-hwmon/`.

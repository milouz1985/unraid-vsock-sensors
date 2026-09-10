# Tests avec le vrai module Linux

Le builder prépare un template Debian 13 qui démarre le **noyau Proxmox
cible**, avec ses headers. Depuis le poste de développement, le lanceur pilote
Proxmox par SSH, clone une VM dédiée et lui envoie directement le working tree
avec `rsync`, y compris les modifications non commitées et les nouveaux
fichiers non ignorés. La compilation du module, son chargement, les tests
hwmon/SMART et le cycle de vie du paquet DKMS se font tous dans la VM.

`build-template.sh` dépend de `common.sh`, placé dans le même répertoire, et
s'exécute directement sur Proxmox pour la préparation initiale du template.
Le runner n'exige aucune copie du dépôt sur le nœud Proxmox : seules les
commandes `qm` et `pvesm` y sont exécutées par SSH. Les scripts du dépôt
utilisent des fins de ligne LF, imposées par `.gitattributes`.

## Création du template Proxmox

Le builder s'exécute en root sur Proxmox, mais le dépôt complet n'a pas besoin
d'y être cloné. Depuis le poste de développement, copier uniquement les
scripts de construction, leur configuration et la clé **publique** dans un
répertoire temporaire dédié :

```sh
cp tests/vm/template.env.example tests/vm/template.env
# Éditer template.env avant la copie : stockage, bridge, réseau et VMID.
make vm-template-sync
```

La synchronisation utilise `PVE_HOST`, `PVE_SSH_USER` et `PVE_TEMPLATE_DIR`
dans `template.env`. La commande manuelle équivalente est :

```sh
ssh root@pve01.lan.home \
    'install -d -m 700 /root/uvss-template-builder'

rsync -av \
    tests/vm/build-template.sh \
    tests/vm/common.sh \
    tests/vm/go-version \
    tests/vm/template.env \
    root@pve01.lan.home:/root/uvss-template-builder/
```

La configuration copiée doit notamment contenir :

```sh
VMID=9000
STORAGE=zfs-pve
BRIDGE=vmbr0
CI_USER=uvss-test
IPCONFIG0="ip=dhcp,ip6=auto"
NAMESERVER="192.168.50.1"
SEARCHDOMAIN="lan.home"
```

La clé privée `~/.ssh/id_ed25519` reste exclusivement sur le poste de
développement. La synchronisation du builder ne copie aucune clé.

Le nœud doit disposer d'un stockage acceptant les disques VM, d'un bridge et
d'un accès aux téléchargements Debian, Proxmox et Go. Le template et les
clones ont besoin du réseau pour Cloud-Init et les modules Go.

L'hôte fournit la version cible via `uname -r` et sa clé publique de dépôt
`/usr/share/keyrings/proxmox-archive-keyring.gpg`. Il n'a besoin ni des headers
ni des outils de compilation du module. Le noyau et les headers sont installés
uniquement dans l'image de la VM, via le dépôt Proxmox signé.

Sur Proxmox, vérifier d'abord la transaction proposée pour les outils
libguestfs :

```sh
apt update
apt -s install --no-install-recommends libguestfs-tools
```

Si elle propose de supprimer `proxmox-ve` ou des composants Proxmox,
corriger les dépôts avant de continuer. Sinon, installer le paquet puis lancer
la première construction :

```sh
apt install --no-install-recommends libguestfs-tools
cd /root/uvss-template-builder
bash build-template.sh
```

Ces commandes peuvent aussi être lancées depuis le poste :

```sh
ssh -t root@pve01.lan.home \
    'apt install --no-install-recommends libguestfs-tools'
ssh -t root@pve01.lan.home \
    'cd /root/uvss-template-builder && bash build-template.sh'
```

Le builder utilise notamment ses variables `VMID`, `STORAGE`, `BRIDGE` et
`CI_USER`. `template.env` est un fichier shell local de confiance, ignoré par
Git. Sa section runner contient notamment :

```sh
PVE_HOST=pve01.lan.home
PVE_SSH_USER=root
PVE_TEMPLATE_DIR=/root/uvss-template-builder
TEMPLATE_VMID=9000
TEST_VMID=9900
PVE_STORAGE=zfs-pve
GUEST_USER=uvss-test
GUEST_SSH_KEY="${HOME}/.ssh/id_ed25519"
GUEST_SSH_PUBLIC_KEY="${GUEST_SSH_KEY}.pub"
GUEST_SSH_IDENTITY_AGENT="${SSH_AUTH_SOCK:-}"
```

Le poste doit disposer de `ssh`, `ssh-add`, `ssh-keygen`, `rsync`, `python3`,
`git` et `sha256sum`.
La connexion SSH au nœud Proxmox doit fonctionner sans interaction, tout
comme l'utilisation de `GUEST_SSH_KEY` sans saisie interactive.
Si la clé privée est protégée par une passphrase, la charger dans `ssh-agent`
avant le test :

```sh
eval "$(ssh-agent -s)" # seulement si aucun agent n'est déjà disponible
ssh-add ~/.ssh/id_ed25519
```

Le runner vérifie avant de créer la VM que les clés privée et publique
correspondent. Si la clé privée est chiffrée, il exige également que l'agent
désigné par `GUEST_SSH_IDENTITY_AGENT` — ou par `SSH_AUTH_SOCK` — puisse
l'utiliser. Il transmet explicitement cet agent à SSH, même si la configuration
globale contient une autre directive `IdentityAgent`.

Si la clé publique du poste n'est pas encore autorisée sur Proxmox, la mise en
place initiale peut se faire avec :

```sh
ssh-copy-id -i ~/.ssh/id_ed25519.pub root@pve01.lan.home
```

Après le démarrage de chaque clone, le runner attend la fin de Cloud-Init via
QEMU Guest Agent puis ajoute `GUEST_SSH_PUBLIC_KEY` aux clés autorisées de
`GUEST_USER`. Cette clé vaut `${GUEST_SSH_KEY}.pub` par défaut : chaque poste
de développement peut donc accéder à son clone avec sa propre clé, même si le
template a été construit depuis une autre machine. Seule la clé publique
transite par Proxmox ; la clé privée reste sur le poste qui lance le test.
Le template ne contient donc aucune clé utilisateur préinstallée et n'a pas
besoin d'être reconstruit lorsqu'un autre poste utilise une autre paire de
clés. Le runner vérifie avant l'injection que le clone ne contient pas déjà de
fichier `authorized_keys` non vide.
Les valeurs de `template.env` prennent priorité sur les valeurs par défaut des
scripts. Ne pas écraser un fichier déjà configuré lors d'une mise à jour.
La version de Go du template possède une seule source de vérité :
`tests/vm/go-version`.

`PVE_KERNEL_RELEASE` vide (ou absent) sélectionne le noyau courant de l'hôte,
localement pendant le build et par SSH pendant chaque test. Pour cibler une
autre version, renseigner la valeur exacte d'un `uname -r` Proxmox dans ce
fichier.
`PVE_REPO_COMPONENT` vaut `pve-no-subscription` par défaut ; `pve-test` est
possible si le noyau cible vient de ce dépôt. `PVE_KEYRING_FILE` permet
d'indiquer le chemin de la clé publique du dépôt sur l'hôte.

Le builder installe `proxmox-kernel-${PVE_KERNEL_RELEASE}-signed` et
`proxmox-headers-${PVE_KERNEL_RELEASE}`. Si cette version n'est plus disponible
dans le dépôt sélectionné, la construction échoue sans choisir un autre noyau.
Il n'installe pas l'hyperviseur complet dans la VM.

Le builder télécharge l'image Debian generic et Go, vérifie leurs sommes
depuis les serveurs officiels puis agrandit la partition ext4 avant la
personnalisation hors ligne. Il installe QEMU Guest Agent, le noyau Proxmox
cible et ses headers, Go, PHP et les outils de compilation. Cloud-Init
ne sert ensuite qu'à l'identité et au réseau de chaque instance ; les mises
à jour automatiques de paquets au premier boot sont désactivées.

GRUB est configuré avec `GRUB_TOP_LEVEL` pour démarrer le noyau cible même
si une autre version est installée. Le noyau Debian peut rester comme secours,
mais un démarrage dessus fait échouer la validation. Le builder enregistre
la cible dans `/etc/uvss-test-kernel` puis vérifie que `uname -r` correspond
exactement à cette cible lors du boot. Il vérifie aussi l'agent, Cloud-Init,
les headers, les symboles hwmon et les modules `drivetemp` et `vsock_loopback`.
Il écrit aussi `/etc/uvss-test-image-version`. Le builder, le lanceur et les
scripts invités partagent la constante de version dans `common.sh` ; une
évolution incompatible du template impose ainsi sa reconstruction.
Le builder et le lanceur acceptent le code `2` de Cloud-Init uniquement si
son état JSON est `done`, sans erreur fatale, et si tous les avertissements
correspondent exactement à la dépréciation connue de la forme chaîne de
`user`, annoncée depuis Cloud-Init 22.2 et prévue pour suppression en 27.2.
Seule la catégorie `DEPRECATED` et ce message sont tolérés. Ils restent
affichés. Toute autre catégorie, tout autre message, erreur ou timeout bloque
le lancement. Un code `2` provenant d'une autre commande reste un échec.
L'identité de la VM est nettoyée avant conversion en template. Les fichiers
de travail sont supprimés ; une VM en échec est conservée pour diagnostic.
L'image Debian utilise `latest` et APT utilise les dépôts courants : les
reconstructions ne sont pas identiques bit à bit. `IMAGE_BASE_URL` permet
de sélectionner une image datée ; cela ne fige pas les paquets APT.

Pour reconstruire un template existant, renvoyer d'abord les fichiers du
builder avec la commande `rsync` précédente, puis exécuter :

```sh
ssh -t root@pve01.lan.home \
    'cd /root/uvss-template-builder && bash build-template.sh --replace'
```

Cette option détruit le VMID configuré. Elle n'accepte qu'une VM portant
le tag `uvss-test-template`. Le lancement normal refuse un VMID occupé. Le
builder démarre une fois la VM, vérifie le noyau, les headers, Cloud-Init et
`/etc/uvss-test-image-version`, nettoie son identité, puis la convertit en
template. Une sortie `Template 9000 is ready` confirme la réussite.

## Exécution

```sh
make test-vm
make test-vm-core
make test-vm-package
# Conserver aussi une VM dont les tests réussissent :
bash tests/vm/run.sh --keep
# Équivalent avec Make :
make test-vm TEST_VM_KEEP=1
```

Dans une VM conservée, les phases de démarrage et Cloud-Init peuvent être
analysées directement avec :

```sh
systemd-analyze time
systemd-analyze critical-chain
systemd-analyze blame
sudo cloud-init analyze blame
sudo cloud-init analyze show
```

`make test-vm` exécute le parcours complet.
`make test-vm-core` couvre uniquement le module, hwmon et SMART ;
`make test-vm-package` couvre uniquement le paquet Debian et DKMS.

Le lanceur refuse un `TEST_VMID` déjà utilisé et exige le tag
`uvss-test-template` sur le template. Il conserve un verrou partagé sur ce
dernier pendant tout le test ; le builder prend un verrou exclusif, ce qui
interdit une reconstruction simultanée. Ces verrous sont détenus par une
session SSH persistante dans `/run/lock` sur Proxmox. Le verrou exclusif du
clone se trouve au même endroit ; aucun verrou local ne prétend protéger les
ressources de l'hyperviseur. Avant chaque opération sensible — validation du
template, clone, configuration, démarrage, tests, arrêt et destruction — le
runner vérifie que le processus SSH détenant les verrous existe toujours. La
perte de cette session interrompt le parcours et conserve le clone éventuel
pour diagnostic.

Le runner crée ensuite un clone lié du template, attend QEMU Guest Agent et la
fin de Cloud-Init, injecte la clé publique du poste, récupère son IPv4 avec
`network-get-interfaces`, puis attend SSH.
Les fichiers listés par Git sont envoyés directement du poste au clone par
`rsync`. Le transfert n'utilise plus QGA, base64 ou une archive intermédiaire
et ne copie jamais le dépôt sur `PVE_HOST`. Il n'inclut ni `.git`, ni les
fichiers locaux ignorés. Les fichiers suivis mais supprimés localement sont
aussi omis.
Les fichiers ignorés nécessaires aux tests doivent être explicitement suivis.
Un manifeste SHA256 est vérifié dans la VM après le transfert.

Avant le démarrage, deux volumes SATA de `TEST_DISK_SIZE_GIB` Gio sont ajoutés
avec les numéros de série `UVSSDISK1` et `UVSSDISK2`. Leurs noms `/dev/sdX`
ne sont pas supposés stables : le test parcourt les disques entiers et les
identifie avec le numéro de série exposé par sysfs. Il vérifie ensuite
`ROTA=1`, génère un `disks.ini`, un `disk.cfg` et des rapports SMART en cache,
puis exerce le collecteur sans aucune commande SMART matérielle. Le scénario
couvre aussi l'expiration du cache, le failsafe hwmon et la récupération.

Avant les tests, le lanceur vérifie par SSH le noyau démarré dans le clone et
le marqueur du template contre `PVE_KERNEL_RELEASE`. Après une mise à jour du
noyau de l'hôte, reconstruire le template ou fixer explicitement la cible
précédente. Un ancien template Debian est refusé : il doit être reconstruit.

Dans le parcours principal, `guest-tests.sh` lance :

1. la compilation de `virt-temp.ko` avec les headers du noyau actif ;
2. `go test -tags=integration -count=1 -run '^TestVM' .` ;
3. dans le parcours paquet, `package-tests.sh` construit quatre versions du
   `.deb`, puis teste installation, mise à jour, échec volontaire d'une
   compilation DKMS, récupération par la version suivante, remove et purge
   avec le vrai DKMS et systemd.

Le test charge le vrai module et vérifie la configuration disque/HBA, les
valeurs et labels sysfs, les écritures sans commit, la propagation d'une
erreur réelle d'écriture, le retrait de sondes, le failsafe et sa récupération.
Il décharge et recharge aussi le module, constate le vrai `ESTALE` puis
vérifie la reconfiguration par le code Go de production. Ce scénario supprime
la nécessité de simuler le comportement du noyau et l'erreur `ESTALE`.
L'injection interne via `publishHWMonFamilyWithWriter()` reste utilisée par les
tests unitaires ciblés sur les erreurs du publisher.
Le test vérifie également `/sys/class/hwmon/hwmonN/name`. Après un rechargement
du module, il restaure un cache réel et exige la présence immédiate des sondes
à `100000` milli°C.

La suite d'intégration exige root, une VM QEMU marquée par le builder et
`UVSS_VM_TEST=1`. Elle refuse un module déjà chargé et ne saute pas
silencieusement les tests si les prérequis manquent. Ne pas la lancer sur
un hôte de production. Elle retire le module à la fin du test hwmon. Le test
du paquet installe le module via DKMS, vérifie le service et la version installée
pour le noyau courant, puis contrôle la conservation de la configuration lors
des mises à jour et retraits, et sa suppression lors de la purge. Le paquet
volontairement cassé contient une directive `#error` ajoutée après sa
construction. Le test exige que son installation échoue, journalise l'état de
`dpkg`, de DKMS, du module et du service, puis vérifie que le paquet reste
`half-configured`, que l'ancienne version DKMS a été désenregistrée et que le
module précédemment chargé reste actif. Il installe ensuite une version valide
plus récente et exige que celle-ci répare complètement l'installation et
retire l'enregistrement DKMS cassé. Le service
écoute sur VSOCK avec `vsock_loopback` dans la VM ; ce contrôle de démarrage
n'envoie pas de snapshots et ne prétend pas tester le transport entre deux
machines.

Après succès, le clone est arrêté puis supprimé à distance. Après un échec,
avec `--keep` ou avec `TEST_VM_KEEP=1`, il reste disponible. Le journal complet
est récupéré directement par `rsync` dans
`dist/vm-tests-<VMID>.<suffixe>.log` sur le poste avant toute suppression du
clone. Il reste aussi accessible dans une VM conservée :

```sh
ssh root@pve01.lan.home qm terminal 9900
ssh -i ~/.ssh/id_ed25519 uvss-test@ADRESSE_IP \
    sudo cat /var/tmp/uvss-tests.log
# Après diagnostic, supprimer explicitement le clone conservé :
ssh root@pve01.lan.home qm shutdown 9900 --timeout 120
ssh root@pve01.lan.home qm destroy 9900 --purge
```

Adapter `9900` à `TEST_VMID`. `TEST_TIMEOUT` borne les commandes de test
dans la VM. Un timeout ou une commande SSH en échec fait échouer le parcours.
Les opérations concurrentes des scripts sur le même VMID sont verrouillées.

## Portée

Le module est compilé, chargé et testé sous le noyau PVE cible dans la VM,
avec le même numéro de version que celui demandé. Le paquet est testé avec
ses vraies dépendances, notamment `proxmox-default-headers`, qui peut ajouter
d'autres headers au template ; le boot reste fixé à la cible explicite.
Le test vérifie DKMS pour le noyau courant, pas un cycle de reboot entre
deux noyaux différents ni le chargement sous Secure Boot.

La VM ne dispose pas d'Unraid ni de matériel SMART/HBA physique. Les tests de
transport AF_VSOCK hôte/invité, des températures SMART/HBA physiques et du
cycle de vie du plugin Unraid restent à réaliser dans leurs environnements
respectifs. Un disque QEMU ne garantit pas les fonctions SMART d'un disque
physique. Aucun module UVSS n'est compilé, installé ou chargé sur l'hôte.

Les tests unitaires du protocole, de la logique métier et des erreurs
matérielles restent utiles et continuent de tourner localement avec `make check`.
Le détecteur de courses s'exécute séparément avec `make test-race` ou
`make all` ; ces contrôles ne sont pas rejoués dans la VM.
Chaque journal indique le commit Git, l'état du working tree, le SHA256 de
son manifeste, le noyau PVE, la version du template, la version de Go et le
parcours exécuté. `make check` lance ShellCheck sur tous les scripts Bash et
POSIX ; l'outil doit donc être installé sur la machine de développement et dans
la CI.
Le runner affiche également la durée de chaque phase sous la forme `TIMING` :
clone, ajout des disques, démarrage, disponibilité QGA/IP/SSH, Cloud-Init,
validation du template, transfert des sources, tests, téléchargement du journal
et suppression de la VM. Le journal invité détaille aussi la compilation du
module, le téléchargement des modules Go, les tests d'intégration et chaque
étape du cycle de vie du paquet.

Références : [personnalisation libguestfs](https://libguestfs.org/virt-customize.1.html),
[agrandissement de l'image](https://libguestfs.org/virt-resize.1.html),
[commandes Proxmox qm](https://pve.proxmox.com/pve-docs/qm.1.html),
[dépôts Proxmox](https://github.com/proxmox/pve-docs/blob/master/pve-package-repos.adoc),
[sélection du noyau GRUB](https://www.gnu.org/software/grub/manual/grub/html_node/Simple-configuration.html).

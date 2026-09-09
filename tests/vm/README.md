# Tests avec le vrai module Linux

Le builder prépare un template Debian 13 qui démarre le **noyau Proxmox
cible**, avec ses headers. Le lanceur clone une VM dédiée et y exécute les
sources actuelles du dépôt, y compris les modifications non commitées et les
nouveaux fichiers non ignorés. La compilation du module, son chargement,
les tests hwmon et le cycle de vie du paquet DKMS se font tous dans la VM.

`build-template.sh` dépend de `common.sh`, placé dans le même répertoire.
Pour utiliser le lanceur `run.sh`, copier ou cloner le dépôt complet sur le
nœud Proxmox. Les scripts du dépôt utilisent des fins de ligne LF, imposées
par `.gitattributes` ; un copier-coller via un éditeur externe peut les altérer.

## Préparation sur Proxmox

Exécuter les commandes depuis une copie de ce dépôt sur le nœud Proxmox,
en root. Le nœud doit disposer d'un stockage acceptant les disques VM,
d'un bridge et d'un accès aux téléchargements Debian et Go. Le template
et les clones ont besoin du réseau pour Cloud-Init et les modules Go.
Le lanceur utilise QEMU Guest Agent ; aucune connexion SSH à la VM n'est
nécessaire pour les tests.

L'hôte fournit la version cible via `uname -r` et sa clé publique de dépôt
`/usr/share/keyrings/proxmox-archive-keyring.gpg`. Il n'a besoin ni des headers
ni des outils de compilation du module. Le noyau et les headers sont installés
uniquement dans l'image de la VM, via le dépôt Proxmox signé.

Vérifier d'abord la transaction proposée pour les outils libguestfs :

```sh
apt update
apt -s install --no-install-recommends libguestfs-tools
```

Si elle propose de supprimer `proxmox-ve` ou des composants Proxmox,
corriger les dépôts avant de continuer. Sinon :

```sh
apt install --no-install-recommends libguestfs-tools
cp tests/vm/template.env.example tests/vm/template.env
# Éditer template.env : stockage, bridge, VMID libres et clé publique SSH.
make vm-template
```

`template.env` est un fichier shell local de confiance, ignoré par Git.
Il configure notamment `VMID` (template), `TEST_VMID` (clone), `STORAGE`
et `SSH_PUBLIC_KEY_FILE`. Fournir uniquement une clé **publique** à ce
dernier emplacement. Les valeurs de l'exemple ne décrivent aucun serveur
particulier.

Les valeurs locales prennent priorité sur les valeurs par défaut du builder.
Ne pas écraser un `template.env` déjà configuré lors d'une mise à jour.
La version de Go du template possède une seule source de vérité :
`tests/vm/go-version`.

`PVE_KERNEL_RELEASE` vide (ou absent) sélectionne le noyau courant de l'hôte,
au moment du build **et de chaque test**. Pour cibler une autre version,
renseigner la valeur exacte d'un `uname -r` Proxmox dans ce fichier.
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
correspondent à la dépréciation connue `'user' of type string is deprecated`.
Seule la catégorie `DEPRECATED` et ce message sont tolérés. Ils restent
affichés. Toute autre catégorie, tout autre message, erreur ou timeout bloque
le lancement. Un code `2` provenant d'une autre commande reste un échec.
L'identité de la VM est nettoyée avant conversion en template. Les fichiers
de travail sont supprimés ; une VM en échec est conservée pour diagnostic.
L'image Debian utilise `latest` et APT utilise les dépôts courants : les
reconstructions ne sont pas identiques bit à bit. `IMAGE_BASE_URL` permet
de sélectionner une image datée ; cela ne fige pas les paquets APT.

Pour reconstruire un template existant :

```sh
bash tests/vm/build-template.sh --replace
```

Cette option détruit le VMID configuré. Elle n'accepte qu'une VM portant
le tag `uvss-test-template`. Le lancement normal refuse un VMID occupé.

## Exécution

```sh
make test-vm
make test-vm-package
make test-vm-all
# Conserver aussi une VM dont les tests réussissent :
bash tests/vm/run.sh --keep
```

`make test-vm` couvre le module, hwmon et SMART. `make test-vm-package` couvre
le paquet Debian et DKMS. `make test-vm-all` enchaîne les deux dans un clone.

Le lanceur refuse un `TEST_VMID` déjà utilisé et exige le tag
`uvss-test-template` sur le template. Il conserve un verrou partagé sur ce
dernier pendant tout le test ; le builder prend un verrou exclusif, ce qui
interdit une reconstruction simultanée. Il crée ensuite un clone complet,
attend l'agent et Cloud-Init, puis transfère une archive des fichiers listés
par Git par blocs via l'agent. Il n'inclut ni `.git`, ni les fichiers locaux
ignorés. Les fichiers suivis mais supprimés localement sont aussi omis.
Les fichiers ignorés nécessaires aux tests doivent être explicitement suivis.
Le SHA256 de l'archive est calculé sur l'hôte puis vérifié dans la VM avant
son extraction.

Avant le démarrage, deux volumes SATA de `TEST_DISK_SIZE_GIB` Gio sont ajoutés
avec les numéros de série `UVSSDISK1` et `UVSSDISK2`. Ils doivent apparaître
comme `/dev/sdb` et `/dev/sdc`, avec `ROTA=1`. Un shim minimal installé dans
la VM associe `disk1` et `disk2` à ces périphériques puis transmet les options
au vrai `smartctl`. Le test vérifie SMART, le JSON normalisé, la température
QEMU de 31 °C et le vrai `diskCollector` alimenté par un `disks.ini`.

Avant le transfert, le lanceur vérifie le noyau démarré dans le clone et le
marqueur du template contre `PVE_KERNEL_RELEASE`. Après une mise à jour du
noyau de l'hôte, reconstruire le template ou fixer explicitement la cible
précédente. Un ancien template Debian est refusé : il doit être reconstruit.

Dans le parcours principal, `guest-tests.sh` lance :

1. `go vet`, les contrôles de scripts et `go test -race ./...` ;
2. la compilation de `virt-temp.ko` avec les headers du noyau actif ;
3. `go test -tags=integration -count=1 -run '^TestVM' .` ;
4. dans le parcours paquet, `package-tests.sh` construit deux versions du `.deb`, installation,
   mise à jour, remove, réinstallation et purge, avec le vrai DKMS et systemd.

Le test charge le vrai module et vérifie la configuration disque/HBA, les
valeurs et labels sysfs, les écritures sans commit, la propagation d'une
erreur réelle d'écriture, le retrait de sondes, le failsafe et sa récupération.
Il décharge et recharge aussi le module, constate le vrai `ESTALE` puis
vérifie la reconfiguration par le code Go de production. Ce scénario remplace
le writer injecté et les deux tests qui simulaient ses erreurs.
Le test vérifie également `/sys/class/hwmon/hwmonN/name`. Après un rechargement
du module, il restaure un cache réel et exige la présence immédiate des sondes
à `100000` milli°C.

La suite d'intégration exige root, une VM QEMU marquée par le builder et
`UVSS_VM_TEST=1`. Elle refuse un module déjà chargé et ne saute pas
silencieusement les tests si les prérequis manquent. Ne pas la lancer sur
un hôte de production. Elle retire le module à la fin du test hwmon. Le test
du paquet le réinstalle via DKMS, vérifie le service et la version installée pour le noyau courant,
puis contrôle la conservation de la configuration lors des mises à jour et
retraits, et sa suppression lors de la purge. Le service écoute sur VSOCK
avec `vsock_loopback` dans la VM ; ce contrôle de démarrage n'envoie pas de
snapshots et ne prétend pas tester le transport entre deux machines.

Après succès, le clone est arrêté puis supprimé. Après un échec ou avec
`--keep`, il reste disponible. Le journal complet est récupéré par blocs dans
`dist/vm-tests-<VMID>.<suffixe>.log` sur le nœud avant toute suppression du
clone. Il reste aussi accessible dans une VM conservée :

```sh
qm guest exec 9900 -- cat /var/tmp/uvss-tests.log
qm terminal 9900
# Après diagnostic, supprimer explicitement le clone conservé :
qm shutdown 9900 --timeout 120
qm destroy 9900 --purge
```

Adapter `9900` à `TEST_VMID`. `TEST_TIMEOUT` borne les commandes de test
dans la VM. Un timeout ou un résultat de l'agent sans code de sortie est un
échec. Les opérations concurrentes des scripts sur le même VMID sont verrouillées.

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
matérielles restent utiles et continuent de tourner avec `make check`.
Chaque journal indique le commit Git, l'état du working tree, le SHA256 de
l'archive, le noyau PVE, la version du template, la version de Go et le
parcours exécuté. `make lint-shell` lance ShellCheck sur tous les scripts Bash
lorsque l'outil est installé sur la machine de développement ou dans la CI.

Références : [personnalisation libguestfs](https://libguestfs.org/virt-customize.1.html),
[agrandissement de l'image](https://libguestfs.org/virt-resize.1.html),
[commandes Proxmox qm](https://pve.proxmox.com/pve-docs/qm.1.html),
[dépôts Proxmox](https://github.com/proxmox/pve-docs/blob/master/pve-package-repos.adoc),
[sélection du noyau GRUB](https://www.gnu.org/software/grub/manual/grub/html_node/Simple-configuration.html).

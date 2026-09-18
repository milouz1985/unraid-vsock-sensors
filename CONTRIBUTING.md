# Contribuer à unraid-vsock-sensors

Ce document décrit l'environnement de développement, les vérifications à
effectuer et le processus de publication de `unraid-vsock-sensors`.

Pour l'installation et l'utilisation du projet, voir [`README.md`](README.md).

## Prérequis

Le projet nécessite :

- Go 1.27.0 ou plus récent ;
- GNU Make ;
- Bash ;
- PHP CLI ;
- ShellCheck ;
- Git ;
- `dpkg-buildpackage`, debhelper 13 et `rsync` pour construire le paquet Proxmox.

Sous Debian ou Ubuntu :

```sh
sudo apt install make shellcheck php-cli git debhelper rsync
```

Le module `virt-temp` nécessite également les headers Linux correspondant au
noyau utilisé pour sa compilation.

## Vérifications courantes

Les principales commandes sont :

```sh
make fmt
make tidy
make check
make test-race
make fuzz-mpt3
make build
make artifacts
```

`make check` exécute notamment :

- vérification `gofmt` ;
- vérification de `go.mod` et `go.sum` ;
- `go vet`;
- tests Go ;
- syntaxe Bash et POSIX shell ;
- lint PHP ;
- tests des scripts du plugin ;
- ShellCheck.

Le race detector est exécuté séparément :

```sh
make test-race
```

## Fuzzing MPT3

Les parsers MPT3 disposent de plusieurs cibles de fuzzing.

Lancer les campagnes par défaut :

```sh
make fuzz-mpt3
```

Modifier leur durée :

```sh
make fuzz-mpt3 FUZZTIME=2m
```

Les campagnes longues ne font pas partie de `make check`, mais les seeds des
fuzzers sont exécutées par `go test ./...`.

## Tests d'intégration Proxmox

Les tests VM utilisent un vrai noyau Proxmox, le vrai module `virt_temp`, DKMS,
systemd et de vrais block devices QEMU.

Commandes principales :

```sh
make vm-template-sync
make vm-template-rebuild
make test-vm
make test-vm-core
make test-vm-package
```

Le fonctionnement du runner et la préparation du template sont documentés dans
[`tests/vm/README.md`](tests/vm/README.md).

Les tests VM doivent être privilégiés lorsqu'ils permettent de vérifier une
véritable frontière système, par exemple :

- module noyau ;
- `/dev/virt-temp` ;
- hwmon/sysfs ;
- DKMS ;
- systemd ;
- erreurs réelles telles que `ESTALE`.

Les tests unitaires restent préférables pour les cas qui nécessiteraient sinon
de simuler du matériel dans la VM, notamment :

- parsers ;
- topologies sysfs USB particulières ;
- StorCLI ;
- timeouts ioctl ;
- erreurs HBA ;
- logique temporelle ;
- fuzzing MPT3.

## Principes d'architecture

Le projet cherche à conserver une séparation claire entre les responsabilités.

### Côté Unraid

L'agent :

- consomme les champs `temp` et `spundown` déjà maintenus par `emhttpd` ;
- lorsque le heartbeat `poll_attributes` est sain, Unraid est l'autorité pour
  ces champs ; UVSS ne connaît pas l'âge physique de la mesure emhttpd ;
- en cas de heartbeat `poll_attributes` absent, lance temporairement
  `smartctl_type` avec protection standby et des délais bornés ;
- ne doit pas réveiller les disques ;
- publie `Temp=0` comme sentinelle synthétique en état `standby` ou `waking`,
  sans confondre ce cas avec une vraie mesure à `0 °C` ; UVSS ne conserve pas
  lui-même sa mesure pré-standby pour le réveil. La wake grace exige une
  transition standby vers actif observée sans erreur d'inventaire intermédiaire ;
- publie les snapshots via AF_VSOCK.

Le backend StorCLI conserve l'identité des contrôleurs tant que la lecture des
températures réussit avec le même ensemble d'index. Un remplacement à chaud
avec index inchangés peut conserver une identité périmée jusqu'au redémarrage
du daemon. `CommandContext(...).Output()` peut aussi attendre un enfant qui
garde ses pipes ouverts après l'arrêt du processus principal ; ce cas a été
reproduit avec un faux StorCLI, pas avec le binaire réel sur Unraid. Le snapshot
HBA expire indépendamment pour protéger le failsafe hwmon. Si le cas est
confirmé sur le vrai chemin StorCLI, traiter localement groupe de processus,
annulation et attente bornée.

Les températures des disques proviennent de :

```text
/var/local/emhttp/disks.ini
/var/local/emhttp/devs.ini
```

La collecte HBA reste séparée de la collecte disque.

### Côté Proxmox

Le récepteur :

- valide les snapshots ;
- maintient la topologie hwmon ;
- pilote `/dev/virt-temp` ;
- persiste uniquement la topologie, jamais les températures.

Le module kernel doit rester simple. La logique métier et la découverte
matérielle appartiennent autant que possible à l'espace utilisateur.

### Failsafe

En cas d'incertitude thermique, le comportement doit rester conservateur.

Quelques règles importantes :

- une erreur d'inventaire ne doit pas publier un inventaire partiel ;
- une topologie précédente valide doit être conservée lors d'une erreur ;
- une détection de bus disque impossible doit inclure le disque en mode `Auto` ;
- une sonde qui n'est plus rafraîchie passe au failsafe kernel ;
- les valeurs physiques ne doivent pas être arbitrairement bornées dans les
  couches qui ne sont pas responsables de leur plausibilité.

Éviter d'ajouter des abstractions génériques lorsque les collecteurs ont des
contraintes réellement différentes.

## Formatage et conventions

Le code Go doit être formaté avec :

```sh
make fmt
```

Les fichiers texte du dépôt utilisent LF.

Les scripts doivent passer ShellCheck lorsqu'il s'applique.

Préférer des erreurs contenant suffisamment de contexte pour permettre un
diagnostic sans journalisation supplémentaire.

Les erreurs répétitives de collecte doivent éviter de produire du spam dans les
logs lorsqu'un mécanisme de transition ou de type `sticky error` existe déjà.

## Tests lors d'une modification

Une modification locale classique devrait au minimum passer :

```sh
make check
make test-race
```

Une modification touchant l'une des zones suivantes devrait également passer
les tests VM concernés :

- `virt-temp`;
- publication hwmon ;
- cache de topologie ;
- paquet Debian ;
- DKMS ;
- service systemd ;
- intégration avec de vrais block devices Linux.

Avant une release complète :

```sh
make test-vm
```

## Construction des artefacts

Construire tous les artefacts :

```sh
make artifacts
```

Avec une version explicite :

```sh
make artifacts VERSION=2.0.0
```

Une prerelease est également acceptée :

```sh
make artifacts VERSION=2.0.0-rc.1
```

Les artefacts sont créés dans :

```text
dist/
```

Le projet cible Linux amd64.

## Versionnement

Les versions suivent SemVer.

Exemples :

```text
2.0.0
2.0.0-rc.1
```

Le préfixe `v` n'est pas fourni à `make`.

Le paquet Debian convertit une prerelease SemVer afin de préserver l'ordre
Debian, par exemple :

```text
2.0.0-rc.1
→ 2.0.0~rc.1-1
```

Une correction limitée au paquet Debian peut augmenter uniquement sa révision :

```sh
make hwmon-package VERSION=2.0.0 DEBIAN_REVISION=2
```

## Tester un paquet Unraid de développement

Sur une installation où le plugin existe déjà :

```sh
make unraid-package
scp dist/<paquet>.txz root@NAS:/tmp/
```

Puis sur Unraid :

```sh
/etc/rc.d/rc.unraid-vsock-sensors stop
upgradepkg --install-new --reinstall /tmp/<paquet>.txz
/etc/rc.d/rc.unraid-vsock-sensors start
```

Installer uniquement le `.txz` ne crée pas une installation persistante du
plugin. Une première installation complète doit passer par un descripteur
`.plg`.

## Préparer une release

Depuis un worktree propre :

```sh
make release VERSION=X.Y.Z
```

Cette commande :

- valide la version ;
- exécute `make check` ;
- exécute `make test-race` ;
- exécute les tests VM ;
- construit les artefacts ;
- actualise le descripteur `.plg`.

Elle ne crée ni commit, ni tag et ne pousse rien.

Vérifier ensuite le changement du `.plg` :

```sh
git diff -- unraid-plugin/unraid-vsock-sensors.plg
```

Puis :

```sh
git add unraid-plugin/unraid-vsock-sensors.plg
git commit -m "Publie le descripteur Unraid X.Y.Z"
git tag -a vX.Y.Z -m "Release vX.Y.Z"
```

## Publication GitHub

Le remote `github` correspond au dépôt public distribuant le plugin.

`origin` correspond au dépôt Gitea de développement.

Publier d'abord le tag :

```sh
git push github vX.Y.Z
```

Créer ensuite la GitHub Release et y téléverser :

```text
dist/unraid-vsock-sensors-X.Y.Z-x86_64-1.txz
dist/unraid-vsock-sensors-hwmon_X.Y.Z-1_amd64.deb
```

Vérifier que les assets sont accessibles :

```sh
curl -fIL https://github.com/milouz1985/unraid-vsock-sensors/releases/download/vX.Y.Z/unraid-vsock-sensors-X.Y.Z-x86_64-1.txz

curl -fIL https://github.com/milouz1985/unraid-vsock-sensors/releases/download/vX.Y.Z/unraid-vsock-sensors-hwmon_X.Y.Z-1_amd64.deb
```

Ne pousser `main` sur GitHub qu'après cette vérification : le `.plg` publié sur
`main` référence directement le `.txz` de la release.

Enfin :

```sh
git push github main
git push origin main vX.Y.Z
```

## Commits

Les commits doivent rester ciblés et décrire l'intention du changement.

Exemples :

```text
fix(disks): rejeter les IDs dupliqués dans chaque source Unraid
refactor(hba): fixer l'intervalle selon le backend
refactor(unraid): simplifier le bootstrap de la page
docs: alléger et dédupliquer les README
```

Éviter de mélanger refactor, changement fonctionnel et documentation sans lien
dans un même commit.

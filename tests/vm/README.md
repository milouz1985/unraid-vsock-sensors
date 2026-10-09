# Tests d'intégration Proxmox

Cette suite teste `unraid-vsock-sensors` dans une VM Debian 13 démarrant un vrai
noyau Proxmox.

Elle couvre notamment :

- le module `virt_temp` ;
- le vrai sysfs hwmon ;
- le collecteur disque avec des block devices QEMU ;
- le failsafe et la reconstruction après reload du module ;
- AF_VSOCK guest → host via un guest KVM imbriqué ;
- DKMS ;
- systemd ;
- l'installation, la mise à jour, la suppression et la purge du paquet Debian.

Aucun module UVSS n'est chargé directement sur l'hôte Proxmox.

## Architecture

```text
poste de développement
→ SSH vers Proxmox
→ clone lié du template
→ rsync du working tree
→ compilation et tests dans la VM
```

Le clone est supprimé après succès et conservé après échec pour diagnostic.

## Prérequis

Sur le poste de développement :

- SSH ;
- `rsync` ;
- `git` ;
- `python3` ;
- une clé SSH utilisable sans interaction.

Sur Proxmox :

- un stockage VM ;
- un bridge réseau ;
- accès aux dépôts Debian et Proxmox ;
- `libguestfs-tools` pour construire le template.

## Configuration

Créer :

```sh
cp tests/vm/template.env.example tests/vm/template.env
```

Puis adapter notamment :

```sh
PVE_HOST=pve01.lan.home
PVE_SSH_USER=root
PVE_TEMPLATE_DIR=/root/uvss-template-builder

TEMPLATE_VMID=9000
TEST_VMID=9900
PVE_STORAGE=zfs-pve

BRIDGE=vmbr0
GUEST_USER=uvss-test
GUEST_SSH_KEY="${HOME}/.ssh/id_ed25519"
```

`template.env` est local et ignoré par Git.

La version de Go du template est définie dans :

```text
tests/vm/go-version
```

## Préparer le template

Synchroniser le builder :

```sh
make vm-template-sync
```

Construire ou reconstruire le template :

```sh
make vm-template-rebuild
```

Le builder crée une Debian generic avec :

- QEMU Guest Agent ;
- Cloud-Init ;
- le noyau Proxmox cible ;
- ses headers ;
- Go ;
- PHP ;
- les outils de compilation ;
- `qemu-system-x86`, `busybox-static`, `cpio`, `kmod`, `zstd` et `xz-utils`
  pour le guest d’acceptance imbriqué.

Le contrat d’image est en version 3 : reconstruire les templates plus anciens.
Ces dépendances sont installées dans l’image Debian ; aucune installation réseau
n’est effectuée par la suite VSOCK. La virtualisation imbriquée doit être exposée
au clone (`--cpu host`), avec `/dev/kvm` utilisable. Aucun fallback TCG n’est permis.

Le boot est explicitement fixé sur le noyau Proxmox demandé.

Un template existant n'est remplacé que s'il porte le tag :

```text
uvss-test-template
```

afin d'éviter de détruire une VM étrangère.

Si `PVE_KERNEL_RELEASE` n'est pas défini, le noyau actuellement démarré sur
l'hôte est utilisé comme cible.

## Exécuter les tests

Suite complète (core + kernel-stress + package) :

```sh
make test-vm
```

Module, hwmon et collecteur disque :

```sh
make test-vm-core
```

Stress du module `virt-temp` (lifecycle, concurrence, unload/reload) :

```sh
make test-vm-kernel-stress
```

Acceptance AF_VSOCK réelle :

```sh
make test-vm-vsock
```

Paquet Debian et DKMS :

```sh
make test-vm-package
```

Mode long pour le stress (plus d'itérations) :

```sh
UVSS_STRESS_ITERS=2000 UVSS_RELOAD_CYCLES=100 make test-vm-kernel-stress
```

Le runner valide ces deux paramètres comme des entiers strictement positifs
(au plus sept chiffres) et les transmet explicitement au scénario
`kernel-stress` dans la VM, y compris pendant `make test-vm`. Sans paramètre,
le guest conserve les valeurs par défaut de 500 itérations et 30 cycles.
Les lignes `Concurrent stress (N iterations)` et `Unload/reload (N cycles)`
du journal guest confirment les valeurs effectivement utilisées.

Le scénario installe debhelper et `rsync` dans le clone VM avant de construire
les paquets. Il vérifie ensuite les scripts de maintenance générés, notamment
la conservation de l'ancienne version DKMS lors d'une mise à jour ratée.

Conserver le clone même après succès :

```sh
make test-vm TEST_VM_KEEP=1
```

## Fonctionnement du runner

Le runner :

1. vérifie le template ;
2. crée un clone lié ;
3. ajoute deux disques SATA QEMU ;
4. démarre la VM ;
5. attend QEMU Guest Agent, Cloud-Init et SSH ;
6. transfère le working tree par `rsync` ;
7. vérifie son manifeste SHA256 ;
8. exécute les tests ; pour la suite paquet, redémarre la VM après l'upgrade
   échoué pour vérifier le module conservé, puis après le second upgrade cassé
   pour vérifier la suppression directe du paquet half-configured, puis une
   dernière fois après réparation ;
9. récupère le journal ;
10. supprime le clone après succès.

Des verrous côté Proxmox empêchent une reconstruction du template ou
l'utilisation concurrente du même clone pendant les tests.

Si la session qui détient les verrous disparaît, le runner échoue et conserve
la VM pour diagnostic.

## Disques de test

Deux volumes SATA sont ajoutés avec les numéros de série :

```text
UVSSDISK1
UVSSDISK2
```

Les tests ne supposent jamais que `/dev/sdX` est stable.

Les disques sont retrouvés via :

```text
/dev/disk/by-id/ata-QEMU_HARDDISK_<serial>
```

Le runner vérifie également leur topologie sysfs puis génère un environnement
Unraid minimal avec :

- `disks.ini` ;
- `var.ini` ;
- les champs `temp` et `spundown` de `disks.ini`, sans cache SMART séparé.

Aucune commande SMART matérielle n'est exécutée.

## Suite `core`

Le test compile et charge le vrai module `virt_temp`, puis exécute :

```sh
go test -tags=integration -count=1 -run '^TestVM' .
```

Il vérifie notamment :

- création des périphériques hwmon ;
- valeurs et labels ;
- températures signées atypiques jusqu'au vrai hwmon ;
- ajout et retrait de sondes ;
- failsafe et récupération ;
- erreur d'écriture réelle via `/dev/full` ;
- reload du module ;
- suppression/recréation réelle de la topologie configfs ;
- sécurité d’un descripteur `/dev/virt-temp/*` encore ouvert pendant un `rmdir` ;
- reconfiguration par le code de production ;
- restauration du cache ;
- collecteur disque avec vrais block devices QEMU.

## Suite `kernel-stress`

Le test `tests/vm/virt-temp-stress-test.sh` exerce le vrai module `virt_temp`
construit depuis le working tree et chargé via `insmod`. Le service
`unraid-vsock-hwmon` est arrêté pendant le test et restauré après. Tous les
reloads utilisent le `.ko` exact du checkout.

Scénarios (dans l'ordre) :

1. **Nominal** : `mkdir` configfs → `/dev` apparaît → write température →
   write label → lecture hwmon (`temp1_input` + `temp1_label`) → `rmdir` →
   disparition.
2. **Open-FD** : FD `/dev/virt-temp/*` ouvert, `rmdir` configfs, write via
   l'ancien FD → doit retourner **ENODEV** exactement (vérifié par Python).
3. **Multi-FD** : 8 FDs simultanés, `rmdir`, chaque FD write → **ENODEV**
   (8/8), puis fermeture propre.
4. **Unload refcount** : sensor créé, FD ouvert, `rmdir` (le seul refcount
   restant est le FD), `rmmod` → doit échouer. Close FD, `rmmod` → succès.
   Rechargement via `insmod`.
5. **Concurrence** : 3 workers en parallèle pendant 500 itérations :
   - Worker A : lifecycle strict (create → label → write → publish → remove) ;
   - Worker B : lecture hwmon ciblé sur le sensor publié par A ;
   - Worker C : écriture miscdevice via Python (errno exact).
   Preuve d'activité : `hwmon_reads > 0`, `misc_attempts > 0`.
   La première sonde attend cette activité des deux observateurs avant son
   retrait, avec une limite de cinq secondes, pour rendre les runs courts
   vérifiables. Les sondes suivantes suivent le lifecycle concurrent normal.
   Erreurs acceptées : ENOENT/ENODEV (disparition concurrente).
6. **Unload/reload** : 30 cycles `rmmod` / `insmod` avec vérification de
   l'apparition/disparition de `/sys/kernel/config/virt_temp`.
7. **dmesg** : recherche des patterns `BUG:`, `WARNING:`, `KASAN:`, `KCSAN:`,
   `UBSAN:`, `use-after-free`, `general protection fault`, `kernel BUG`,
   `Oops:`, `refcount_t:`, `hung task`, `lockdep` depuis le début du test.

Limites :

- Pas de KASAN/KCSAN sur le kernel PVE standard. Un dmesg propre ne prouve
  pas l'absence absolue d'UAF ; c'est une amélioration future.
- Le test couvre la concurrence userspace (FDs, configfs, hwmon,
  miscdevice) mais pas la préemption/migration multi-cœur au niveau kernel.

## Suite `package`

Elle construit plusieurs versions du `.deb` et vérifie :

- installation ;
- DKMS ;
- chargement du module ;
- démarrage systemd ;
- politique `SystemCallFilter` du fragment installé et de ses drop-ins,
  expansion récursive des groupes locaux, absence de syscall bloqué hors des
  groupes et contribution effective de chaque groupe ;
- mutations temporaires de cette politique : retrait de `@mount` et ajout
  d'un syscall extérieur aux groupes, tous deux détectés, puis restauration
  du filtre et des drop-ins sans redémarrer le service ;
- mise à jour ;
- conservation de la configuration ;
- échec volontaire d'une compilation DKMS ;
- ancienne version DKMS encore installée sur les noyaux ciblés, avec ses
  sources et son fichier module, après cet échec ;
- redémarrage réel depuis ce module conservé, avant toute réparation dpkg ;
- réparation directe du paquet half-configured par l'installation d'une version
  valide (sans `apt remove` intermédiaire) : la version conservée et la version
  cassée sont retirées, `saved_sources` est nettoyé ;
- second upgrade volontairement cassé, pour tester la désinstallation d'un
  upgrade échoué ;
- `remove` direct du paquet half-configured, qui doit retirer toutes les
  versions DKMS et leurs sources de secours tout en conservant la configuration
  et le cache ;
- réinstallation d'une version valide plus récente que la version cassée
  (un downgrade serait refusé par `apt-get`) ;
- second redémarrage réel depuis le module réparé ;
- échec récupérable de `remove` lorsqu'un processus extérieur conserve ouvert
  le FD d'une sonde supprimée, avec `ENODEV` sur cet ancien FD et restauration
  du service précédemment actif ;
- conservation d'un abonnement restart créé par l'administrateur après
  `remove` et `purge` ;
- `remove` ;
- `purge`.

Le package volontairement cassé contient une directive `#error`. Son échec doit
laisser le paquet half-configured sans retirer l'ancienne version DKMS. Après
reboot, le module doit se charger depuis le disque et le service doit rester
actif. La réparation d'un upgrade échoué est testée par l'installation directe
d'une version valide ; la désinstallation d'un upgrade échoué est testée par un
`remove` direct d'un second paquet cassé. Un `remove` direct d'un paquet
half-configured doit laisser la machine sans module chargé ni version DKMS
restante.

Le démarrage du service est vérifié avec `vsock_loopback`. Ce test ne représente
pas un vrai transport AF_VSOCK entre une VM Unraid et son hôte.

## Suite `vsock-e2e`

La VM PVE disposable est l’hôte VSOCK. Elle lance un mini guest QEMU/KVM,
sans disque ni réseau, avec le même `/boot/vmlinuz-$(uname -r)` et un initramfs
construit pour le test :

```text
guest KVM imbriqué (sender Go statique)
→ virtio-vsock → vhost-vsock dans la VM disposable
→ receiver hwmon de production
→ configfs virt_temp → /dev/virt-temp → hwmon
```

Le script vérifie `/dev/kvm`, le device QEMU `vhost-vsock-pci`, le module
`vhost_vsock` et `/dev/vhost-vsock`. Il construit le receiver et `virt-temp.ko`
depuis le working tree. Le port provient de l’unité systemd et `--cid 42` filtre
le CID du guest imbriqué ; le receiver utilise son bind VSOCK de production.
Aucun module ni receiver n’est lancé sur l’hyperviseur physique.

L’initramfs contient BusyBox statique, `/init`, le sender, et les modules dérivés
par `modprobe --show-depends` pour `virtio_pci` et `vmw_vsock_virtio_transport`.
Les modules compressés sont décompressés avant inclusion. Le sender réutilise
`sensors.WriteFrame` pour les frames valides et `vsock.Host` comme destination.

Le canal série bidirectionnel du coprocess QEMU synchronise `READY`, `VALID1`,
`INVALID`, `VALID2` et `QUIT`, avec des deadlines. Chaque envoi ouvre une nouvelle
connexion AF_VSOCK. Le test vérifie le sensor `disk:vsock-e2e`, label `VSOCK E2E`,
à `42000` puis `43000` milli°C, les chemins configfs et miscdevice indépendants,
l’unicité et l’identité du hwmon. `INVALID` envoie du JSON incorrect : l’erreur
protocol doit apparaître dans le log du receiver, qui reste vivant et accepte
`VALID2`.

Le cleanup attend QEMU et le receiver, retire uniquement le sensor possédé,
vérifie les leftovers et décharge les modules chargés par le test. Les nouvelles
lignes de dmesg et la console interne sont vérifiées pour les erreurs kernel.
La console complète et le log receiver sont inclus dans le journal récupéré ;
les fichiers séparés `/var/tmp/uvss-vsock-{console,receiver,dmesg}.log` restent
également disponibles dans une VM conservée après échec.

Cette suite valide le transport virtio réel et le receiver. Elle n’exécute pas
un OS Unraid, ne teste pas un HBA physique et n’utilise pas `vsock_loopback` pour
ses connexions. Le test de démarrage du paquet conserve sa portée distincte.

## Diagnostic

Les journaux sont récupérés dans :

```text
dist/vm-tests-<VMID>.<suffixe>.log
```

Pour une VM conservée :

```sh
ssh root@pve01.lan.home qm terminal 9900
```

ou :

```sh
ssh -i ~/.ssh/id_ed25519 uvss-test@ADRESSE_IP \
  sudo cat /var/tmp/uvss-tests.log
```

Après diagnostic :

```sh
ssh root@pve01.lan.home qm shutdown 9900 --timeout 120
ssh root@pve01.lan.home qm destroy 9900 --purge
```

Adapter `9900` à `TEST_VMID`.

Pour analyser le boot :

```sh
systemd-analyze time
systemd-analyze critical-chain
systemd-analyze blame
sudo cloud-init analyze blame
```

## Portée

Les tests utilisent réellement :

- le noyau Proxmox ;
- ses headers ;
- le module `virt_temp` ;
- hwmon/sysfs ;
- DKMS ;
- systemd ;
- des block devices QEMU ;
- AF_VSOCK virtio guest → host, avec un guest KVM imbriqué.

Ils ne couvrent pas :

- Unraid lui-même ;
- un vrai disque SMART ;
- un HBA physique ;
- Secure Boot ;
- un cycle de reboot entre plusieurs noyaux.

Les tests unitaires restent nécessaires pour les parsers, les topologies USB,
StorCLI, les timeouts ioctl, le protocole et le fuzzing MPT3.

Le race detector est exécuté séparément :

```sh
make test-race
```

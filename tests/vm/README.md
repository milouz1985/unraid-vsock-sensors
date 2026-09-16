# Tests d'intégration Proxmox

Cette suite teste `unraid-vsock-sensors` dans une VM Debian 13 démarrant un vrai
noyau Proxmox.

Elle couvre notamment :

- le module `virt_temp` ;
- le vrai sysfs hwmon ;
- le collecteur disque avec des block devices QEMU ;
- le failsafe et `ESTALE` ;
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
- les outils de compilation.

Le boot est explicitement fixé sur le noyau Proxmox demandé.

Un template existant n'est remplacé que s'il porte le tag :

```text
uvss-test-template
```

afin d'éviter de détruire une VM étrangère.

Si `PVE_KERNEL_RELEASE` n'est pas défini, le noyau actuellement démarré sur
l'hôte est utilisé comme cible.

## Exécuter les tests

Suite complète :

```sh
make test-vm
```

Module, hwmon et collecteur disque :

```sh
make test-vm-core
```

Paquet Debian et DKMS :

```sh
make test-vm-package
```

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
- vrai `ESTALE` ;
- reconfiguration par le code de production ;
- restauration du cache ;
- collecteur disque avec vrais block devices QEMU.

## Suite `package`

Elle construit plusieurs versions du `.deb` et vérifie :

- installation ;
- DKMS ;
- chargement du module ;
- démarrage systemd ;
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
- des block devices QEMU.

Ils ne couvrent pas :

- Unraid lui-même ;
- un vrai disque SMART ;
- un HBA physique ;
- le transport AF_VSOCK guest → host réel ;
- Secure Boot ;
- un cycle de reboot entre plusieurs noyaux.

Les tests unitaires restent nécessaires pour les parsers, les topologies USB,
StorCLI, les timeouts ioctl, le protocole et le fuzzing MPT3.

Le race detector est exécuté séparément :

```sh
make test-race
```

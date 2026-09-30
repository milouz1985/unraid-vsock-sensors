# Intégration hwmon `virt-temp`

Ce document décrit l'interface et le cycle de vie du module noyau `virt-temp`.

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

## Licence

Le programme userspace est sous `GPL-3.0-or-later`.

Le module noyau `virt-temp` est sous `GPL-2.0-only`.

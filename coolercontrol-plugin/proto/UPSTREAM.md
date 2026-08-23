# Origine des fichiers protobuf

Les fichiers sous `coolercontrol/` sont une copie non modifiée de la
spécification gRPC officielle de CoolerControl :

- dépôt : `https://gitlab.com/coolercontrol/cc-plugins`
- commit : `f7db90a36539bd936ae845ff4dea3d69b7e7434c`
- date du commit : `2026-04-20`
- chemin source : `proto/coolercontrol/`

Ils sont distribués selon la licence GPL-3.0 du dépôt source, reproduite dans
le fichier `LICENSE` de ce répertoire. Les options Go nécessaires à la
génération sont fournies extérieurement par `../generate.sh`, afin de conserver
les fichiers upstream à l'identique.


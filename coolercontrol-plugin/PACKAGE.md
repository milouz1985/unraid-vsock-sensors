# Installation sur Proxmox

Placez l'archive et son fichier `.sha256` dans le même répertoire, puis adaptez
le CID et le port AF_VSOCK à votre VM Unraid :

```sh
archive="$(find . -maxdepth 1 -name 'unraid-vsock-sensors-cc-*-linux-amd64.tar.gz' -print -quit)"
sha256sum -c "$archive.sha256"
tar -xzf "$archive"
cd "${archive%.tar.gz}"
sudo ./install.sh --cid=42 --port=19090
sudo systemctl restart coolercontrold
```

La configuration peut ensuite être modifiée dans CoolerControl sous
**Plugins → Unraid VSOCK Sensors**.

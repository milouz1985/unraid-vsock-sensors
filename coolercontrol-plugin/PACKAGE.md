# Installation sur Proxmox

Placez l'archive et son fichier `.sha256` dans le même répertoire, puis adaptez
le CID et le port AF_VSOCK à votre VM Unraid :

```sh
sha256sum -c unraid-vsock-sensors-cc-0.1.0-linux-amd64.tar.gz.sha256
tar -xzf unraid-vsock-sensors-cc-0.1.0-linux-amd64.tar.gz
cd unraid-vsock-sensors-cc-0.1.0-linux-amd64
sudo ./install.sh --cid=42 --port=19090
sudo systemctl restart coolercontrold
```

La configuration peut ensuite être modifiée dans CoolerControl sous
**Plugins → Unraid VSOCK Sensors**.

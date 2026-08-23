#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
module="unraid-vsock-sensors/coolercontrol-plugin"
models_package="$module/gen/models"
service_package="$module/gen/device_service"
output_dir="$(mktemp -d)"
trap 'rm -rf "$output_dir"' EXIT

cd "$script_dir"
proto_files=(
    proto/coolercontrol/models/v1/*.proto
    proto/coolercontrol/device_service/v1/*.proto
)
go_options=(--go_opt="module=$module")
grpc_options=(--go-grpc_opt="module=$module")

for path in "${proto_files[@]}"; do
    relative="${path#proto/}"
    case "$relative" in
        coolercontrol/models/*) package="$models_package" ;;
        coolercontrol/device_service/*) package="$service_package" ;;
        *) echo "Fichier protobuf inattendu : $relative" >&2; exit 1 ;;
    esac
    go_options+=(--go_opt="M$relative=$package")
    grpc_options+=(--go-grpc_opt="M$relative=$package")
done

protoc -I proto --go_out="$output_dir" "${go_options[@]}" "${proto_files[@]}"
protoc -I proto --go-grpc_out="$output_dir" "${grpc_options[@]}" \
    proto/coolercontrol/device_service/v1/device_service.proto

rm -rf "$script_dir/gen"
mv "$output_dir/gen" "$script_dir/gen"


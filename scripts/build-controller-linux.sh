#!/usr/bin/env bash
set -Eeuo pipefail

readonly method_ctx="scripts.build-controller-linux"
readonly script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
readonly repo_root="$(cd -- "${script_dir}/.." && pwd -P)"
readonly output_dir="${repo_root}/data/build"
readonly output_path="${output_dir}/controller-linux"

mkdir -p "${output_dir}"
"${script_dir}/docker-go.sh" \
  go build -trimpath -o /workspace/data/build/controller-linux ./cmd/controller
chmod 0755 "${output_path}"

printf 'ИНФО метод=%s сообщение=%s путь=%s\n' \
  "${method_ctx}" \
  "controller собран в ограниченном контейнере" \
  "${output_path}"

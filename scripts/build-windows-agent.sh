#!/usr/bin/env bash
set -Eeuo pipefail

readonly script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
readonly repo_root="$(cd -- "${script_dir}/.." && pwd -P)"
readonly method_ctx="scripts.build-windows-agent"
readonly output_dir="${repo_root}/data/build"
readonly output_path="${output_dir}/windows-agent.exe"

mkdir -p "${output_dir}"

"${script_dir}/docker-go.sh" env \
  CGO_ENABLED=0 GOOS=windows GOARCH=amd64 \
  go build -trimpath -o /workspace/data/build/windows-agent.exe ./cmd/windows-agent

printf 'ИНФО метод=%s сообщение=%s путь=%s\n' \
  "${method_ctx}" \
  "Windows Agent собран в ограниченном контейнере" \
  "${output_path}"
printf 'ИНФО метод=%s сообщение=%s\n' \
  "${method_ctx}" \
  "бинарник не запущен и не заменил установленную копию"

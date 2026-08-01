#!/usr/bin/env bash
set -Eeuo pipefail

readonly method_ctx="scripts.docker-build-safe"
readonly builder_name="${ARENA_BUILDER_NAME:-arena-bounded}"
readonly memory_limit="${ARENA_BUILD_MEMORY:-1024m}"
readonly memory_bytes="${ARENA_BUILD_MEMORY_BYTES:-1073741824}"
readonly cpu_quota="${ARENA_BUILD_CPU_QUOTA:-100000}"
readonly cpu_period="${ARENA_BUILD_CPU_PERIOD:-100000}"
readonly builder_container="buildx_buildkit_${builder_name}0"

fail() {
  printf 'ОШИБКА метод=%s сообщение=%s\n' "${method_ctx}" "$*" >&2
  exit 1
}

stop_builder() {
  docker buildx stop "${builder_name}" >/dev/null 2>&1 || true
}
trap stop_builder EXIT
trap 'status=$?; printf "ОШИБКА метод=%s сообщение=%s код=%d\\n" \
  "${method_ctx}" "сборка образа в ограниченном BuildKit завершилась ошибкой" "${status}" >&2; exit "${status}"' ERR

command -v docker >/dev/null 2>&1 || fail "не найдена команда docker"
(($# >= 1)) || fail "не переданы параметры команды docker buildx build"

if ! docker buildx inspect "${builder_name}" >/dev/null 2>&1; then
  docker buildx create \
    --name "${builder_name}" \
    --driver docker-container \
    --driver-opt "memory=${memory_limit}" \
    --driver-opt "memory-swap=${memory_limit}" \
    --driver-opt "cpu-quota=${cpu_quota}" \
    --driver-opt "cpu-period=${cpu_period}" \
    --driver-opt default-load=true >/dev/null
fi
docker buildx inspect --bootstrap "${builder_name}" >/dev/null

actual_memory="$(docker inspect --format '{{.HostConfig.Memory}}' "${builder_container}")"
actual_swap="$(docker inspect --format '{{.HostConfig.MemorySwap}}' "${builder_container}")"
actual_quota="$(docker inspect --format '{{.HostConfig.CpuQuota}}' "${builder_container}")"
actual_period="$(docker inspect --format '{{.HostConfig.CpuPeriod}}' "${builder_container}")"
[[ "${actual_memory}" == "${memory_bytes}" ]] ||
  fail "BuildKit имеет неверный лимит памяти ${actual_memory}, ожидалось ${memory_bytes}"
[[ "${actual_swap}" == "${memory_bytes}" ]] ||
  fail "BuildKit имеет неверный лимит памяти с подкачкой ${actual_swap}, ожидалось ${memory_bytes}"
[[ "${actual_quota}" == "${cpu_quota}" && "${actual_period}" == "${cpu_period}" ]] ||
  fail "BuildKit имеет неверный лимит процессора ${actual_quota}/${actual_period}"

docker buildx build \
  --builder "${builder_name}" \
  --load \
  --progress plain \
  "$@"

printf 'ИНФО метод=%s сообщение=%s память_байт=%s cpu_quota=%s cpu_period=%s\n' \
  "${method_ctx}" \
  "образ собран в ограниченном BuildKit" \
  "${actual_memory}" \
  "${actual_quota}" \
  "${actual_period}"

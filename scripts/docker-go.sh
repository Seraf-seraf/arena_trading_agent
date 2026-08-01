#!/usr/bin/env bash
set -Eeuo pipefail

# Resource-bounded Go workspace used for builds and tests. The module cache is
# mounted read-only; all compiler output stays in the disposable container.
readonly method_ctx="scripts.docker-go"
readonly repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)"
readonly arena_user_home="$(getent passwd "$(id -u)" | cut -d: -f6)"
readonly go_mod_cache="${ARENA_GO_MOD_CACHE:-${arena_user_home}/go/pkg/mod}"
readonly memory_limit="${ARENA_DOCKER_MEMORY:-1024m}"
readonly cpu_limit="${ARENA_DOCKER_CPUS:-1}"
readonly pids_limit="${ARENA_DOCKER_PIDS:-128}"
readonly go_image="${ARENA_GO_IMAGE:-golang:1.25.4-bookworm}"
readonly go_max_procs="${ARENA_GO_MAX_PROCS:-1}"
readonly go_parallelism="${ARENA_GO_PARALLELISM:-1}"

fail() {
	printf 'ОШИБКА метод=%s сообщение=%s\n' "${method_ctx}" "$*" >&2
	exit 1
}

trap 'status=$?; printf "ОШИБКА метод=%s сообщение=%s код=%d\\n" \
  "${method_ctx}" "команда Go в ограниченном контейнере завершилась ошибкой" "${status}" >&2; exit "${status}"' ERR

command -v docker >/dev/null 2>&1 ||
	fail "не найдена команда docker"
[[ -d "${go_mod_cache}" ]] ||
	fail "не найден read-only кэш модулей Go ${go_mod_cache}; задайте ARENA_GO_MOD_CACHE"

if (($# == 0)); then
	set -- go test "-p=${go_parallelism}" ./...
fi

printf 'ИНФО метод=%s сообщение=%s память=%s процессоры=%s pids=%s подкачка=%s\n' \
	"${method_ctx}" \
	"команда Go запускается в ограниченном контейнере" \
	"${memory_limit}" \
	"${cpu_limit}" \
	"${pids_limit}" \
	"отключена"

docker run --rm \
	--name "arena-go-$RANDOM-$$" \
	--memory="$memory_limit" \
	--memory-swap="$memory_limit" \
	--cpus="$cpu_limit" \
	--pids-limit="$pids_limit" \
	--network=none \
	--user "$(id -u):$(id -g)" \
	-e GOCACHE=/tmp/go-build \
	-e "GOMAXPROCS=${go_max_procs}" \
	-e "GOFLAGS=-p=${go_parallelism}" \
	-v "$repo_root:/workspace" \
	-v "$go_mod_cache:/go/pkg/mod:ro" \
	-w /workspace \
	"$go_image" "$@"

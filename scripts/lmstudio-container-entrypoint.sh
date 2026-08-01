#!/usr/bin/env bash
set -Eeuo pipefail

readonly method_ctx="scripts.lmstudio-container-entrypoint"
readonly lm_home="${ARENA_LMSTUDIO_HOME:-${HOME}/.lmstudio}"
readonly llmster="${lm_home}/llmster/0.0.20-1/llmster"
readonly lms_cli="${lm_home}/bin/lms"
readonly server_port="${ARENA_LM_PORT:-1234}"

fail() {
  printf 'ОШИБКА метод=%s сообщение=%s\n' "${method_ctx}" "$*" >&2
  exit 1
}

[[ -x "${llmster}" ]] || fail "не найден исполняемый llmster: ${llmster}"
[[ -x "${lms_cli}" ]] || fail "не найден исполняемый lms: ${lms_cli}"

"${llmster}" --docker-compatible-platforms cpu-only &
daemon_pid="$!"

stop_daemon() {
  kill -TERM "${daemon_pid}" >/dev/null 2>&1 || true
  wait "${daemon_pid}" >/dev/null 2>&1 || true
}
trap stop_daemon EXIT INT TERM

ready=false
for _ in $(seq 1 60); do
  if "${lms_cli}" daemon status >/dev/null 2>&1; then
    ready=true
    break
  fi
  sleep 1
done
[[ "${ready}" == "true" ]] || fail "служба LM Studio не перешла в готовое состояние"

"${lms_cli}" server start --port "${server_port}" --bind 0.0.0.0

ready=false
for _ in $(seq 1 60); do
  if curl --silent --show-error --fail --max-time 2 \
    "http://127.0.0.1:${server_port}/api/v1/models" >/dev/null 2>&1; then
    ready=true
    break
  fi
  sleep 1
done
[[ "${ready}" == "true" ]] || fail "REST-сервер LM Studio не ответил на проверку готовности"

printf 'ИНФО метод=%s сообщение=%s\n' \
  "${method_ctx}" \
  "LM Studio запущена без предварительной загрузки модели"
wait "${daemon_pid}"

#!/usr/bin/env bash
set -Eeuo pipefail

readonly method_ctx="scripts.stop-wsl-safe"
readonly script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
readonly repo_root="$(cd -- "${script_dir}/.." && pwd -P)"
readonly compose_file="${repo_root}/compose.safe.yml"
readonly controller_mode_url="http://127.0.0.1:8787/api/v1/mode"

fail() {
  local context="$1"
  shift
  printf 'ОШИБКА метод=%s сообщение=%s\n' "${context}" "$*" >&2
  exit 1
}

command -v curl >/dev/null 2>&1 ||
  fail "${method_ctx}" "не найдена команда curl"
command -v docker >/dev/null 2>&1 ||
  fail "${method_ctx}" "не найдена команда docker"
docker compose version >/dev/null 2>&1 ||
  fail "${method_ctx}" "Docker Compose недоступен"

pause_failed=false
# Без --fail: любой HTTP-ответ требует обязательной попытки PUT PAUSED.
if curl --silent --show-error --max-time 2 \
  "${controller_mode_url}" >/dev/null 2>&1; then
  if ! curl --silent --show-error --fail --max-time 8 \
    --request PUT \
    --header 'Content-Type: application/json' \
    --data '{"mode":"PAUSED"}' \
    "${controller_mode_url}" >/dev/null; then
    printf 'ОШИБКА метод=%s сообщение=%s\n' \
      "${method_ctx}.pause_controller" \
      "controller не принял обязательный переход в PAUSED" >&2
    pause_failed=true
  elif ! mode_json="$(curl --silent --show-error --fail --max-time 5 \
    "${controller_mode_url}")"; then
    printf 'ОШИБКА метод=%s сообщение=%s\n' \
      "${method_ctx}.pause_controller" \
      "не удалось подтвердить режим PAUSED" >&2
    pause_failed=true
  elif ! grep -Eq '"mode"[[:space:]]*:[[:space:]]*"PAUSED"' <<<"${mode_json}"; then
    printf 'ОШИБКА метод=%s сообщение=%s ответ=%s\n' \
      "${method_ctx}.pause_controller" \
      "controller вернул режим, отличный от PAUSED" \
      "${mode_json}" >&2
    pause_failed=true
  else
    printf 'ИНФО метод=%s сообщение=%s\n' \
      "${method_ctx}.pause_controller" \
      "controller подтвердил режим PAUSED"
  fi
else
  printf 'ИНФО метод=%s сообщение=%s\n' \
    "${method_ctx}.pause_controller" \
    "controller по HTTP недоступен; безопасность обеспечит остановка контейнера"
fi

docker compose --file "${compose_file}" down --timeout 20 ||
  fail "${method_ctx}" "не удалось остановить Docker Compose стек"
if ! remaining="$(docker compose --file "${compose_file}" ps --all --quiet)"; then
  fail "${method_ctx}" "не удалось подтвердить остановку Docker Compose стека"
fi
[[ -z "${remaining}" ]] ||
  fail "${method_ctx}" "после остановки остались контейнеры проекта: ${remaining//$'\n'/,}"
[[ "${pause_failed}" == "false" ]] ||
  fail "${method_ctx}" "стек остановлен, но обязательный переход controller в PAUSED завершился ошибкой"

printf 'ИНФО метод=%s сообщение=%s\n' \
  "${method_ctx}" \
  "контейнеры остановлены; база SQLite и записи сеансов сохранены"

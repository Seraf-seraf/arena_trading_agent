#!/usr/bin/env bash
set -Eeuo pipefail

readonly method_ctx="scripts.start-wsl-safe"
readonly script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
readonly repo_root="$(cd -- "${script_dir}/.." && pwd -P)"
readonly compose_file="${repo_root}/compose.safe.yml"
readonly controller_url="http://127.0.0.1:8787"
readonly controller_mode_url="${controller_url}/api/v1/mode"
readonly lmstudio_url="http://127.0.0.1:1234"
readonly model_subdir="models/lmstudio-community/Qwen3.5-0.8B-GGUF"

fail() {
  local context="$1"
  shift
  printf 'ОШИБКА метод=%s сообщение=%s\n' "${context}" "$*" >&2
  exit 1
}

require_command() {
  local function_ctx="${method_ctx}.require_command"
  command -v "$1" >/dev/null 2>&1 ||
    fail "${function_ctx}" "не найдена команда $1"
}

wait_for_url() {
  local function_ctx="${method_ctx}.wait_for_url"
  local name="$1"
  local url="$2"
  local attempts="${3:-60}"
  local attempt
  for ((attempt = 1; attempt <= attempts; attempt++)); do
    if curl --silent --show-error --fail --max-time 2 "${url}" >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  fail "${function_ctx}" "${name} не ответил на ${url}"
}

configure_lm_paths() {
  local function_ctx="${method_ctx}.configure_lm_paths"
  local arena_user_home
  local lmstudio_home
  local model_dir

  if ! arena_user_home="$(getent passwd "$(id -u)" | cut -d: -f6)"; then
    fail "${function_ctx}" "не удалось прочитать домашний каталог пользователя"
  fi
  [[ -n "${arena_user_home}" && "${arena_user_home}" == /* ]] ||
    fail "${function_ctx}" "не удалось определить абсолютный домашний каталог пользователя"
  lmstudio_home="${ARENA_LMSTUDIO_HOME:-${arena_user_home}/.lmstudio}"
  model_dir="${ARENA_LM_MODEL_DIR:-${lmstudio_home}/${model_subdir}}"
  [[ "${lmstudio_home}" == /* ]] ||
    fail "${function_ctx}" "ARENA_LMSTUDIO_HOME должен быть абсолютным путём"
  [[ "${model_dir}" == /* ]] ||
    fail "${function_ctx}" "ARENA_LM_MODEL_DIR должен быть абсолютным путём"

  export ARENA_LMSTUDIO_HOME="${lmstudio_home}"
  export ARENA_LM_MODEL_DIR="${model_dir}"
  printf 'ИНФО метод=%s сообщение=%s lmstudio_home=%s каталог_модели=%s\n' \
    "${function_ctx}" \
    "каталоги LM Studio согласованы" \
    "${ARENA_LMSTUDIO_HOME}" \
    "${ARENA_LM_MODEL_DIR}"
}

pause_existing_controller() {
  local function_ctx="${method_ctx}.pause_existing_controller"
  local mode_json

  # Без --fail: любой HTTP-ответ означает, что на адресе есть controller и
  # обязательный PUT PAUSED нельзя выдать за отсутствие процесса.
  if ! curl --silent --show-error --max-time 2 \
    "${controller_mode_url}" >/dev/null 2>&1; then
    printf 'ИНФО метод=%s сообщение=%s\n' \
      "${function_ctx}" \
      "работающий controller по HTTP не обнаружен; безопасность обеспечит остановка прежнего стека"
    return 0
  fi
  if ! curl --silent --show-error --fail --max-time 8 \
    --request PUT \
    --header 'Content-Type: application/json' \
    --data '{"mode":"PAUSED"}' \
    "${controller_mode_url}" >/dev/null; then
    printf 'ОШИБКА метод=%s сообщение=%s\n' \
      "${function_ctx}" \
      "работающий controller не принял обязательный переход в PAUSED" >&2
    return 1
  fi
  if ! mode_json="$(curl --silent --show-error --fail --max-time 5 \
    "${controller_mode_url}")"; then
    printf 'ОШИБКА метод=%s сообщение=%s\n' \
      "${function_ctx}" \
      "не удалось прочитать режим controller после перехода в PAUSED" >&2
    return 1
  fi
  if ! grep -Eq '"mode"[[:space:]]*:[[:space:]]*"PAUSED"' <<<"${mode_json}"; then
    printf 'ОШИБКА метод=%s сообщение=%s ответ=%s\n' \
      "${function_ctx}" \
      "controller не подтвердил обязательный режим PAUSED" \
      "${mode_json}" >&2
    return 1
  fi
  printf 'ИНФО метод=%s сообщение=%s\n' \
    "${function_ctx}" \
    "работающий controller подтвердил режим PAUSED"
}

stop_existing_compose_stack() {
  local function_ctx="${method_ctx}.stop_existing_compose_stack"
  local remaining

  if ! docker compose --file "${compose_file}" down --timeout 20; then
    fail "${function_ctx}" "не удалось остановить прежний Docker Compose стек"
  fi
  if ! remaining="$(docker compose --file "${compose_file}" ps --all --quiet)"; then
    fail "${function_ctx}" "не удалось подтвердить остановку прежнего Docker Compose стека"
  fi
  [[ -z "${remaining}" ]] ||
    fail "${function_ctx}" "после docker compose down остались контейнеры проекта: ${remaining//$'\n'/,}"
  printf 'ИНФО метод=%s сообщение=%s\n' \
    "${function_ctx}" \
    "прежний Docker Compose стек полностью остановлен до проверки модели и сборки"
}

validate_geometry_environment() {
  local function_ctx="${method_ctx}.validate_geometry_environment"
  local names=(
    ARENA_EXPECTED_WIDTH
    ARENA_EXPECTED_HEIGHT
    ARENA_EXPECTED_DPI
  )
  local configured=0
  local name
  local value

  for name in "${names[@]}"; do
    value="${!name:-}"
    if [[ -n "${value}" ]]; then
      ((configured += 1))
      [[ "${value}" =~ ^[1-9][0-9]*$ ]] ||
        fail "${function_ctx}" "${name} должен быть положительным целым числом"
    fi
  done
  if ((configured != 0 && configured != ${#names[@]})); then
    fail "${function_ctx}" \
      "ARENA_EXPECTED_WIDTH, ARENA_EXPECTED_HEIGHT и ARENA_EXPECTED_DPI задаются только вместе"
  fi
  if ((configured == 0)); then
    printf 'ИНФО метод=%s сообщение=%s\n' \
      "${function_ctx}" \
      "ожидаемая геометрия не задана; SCAN и TRADE останутся заблокированы предварительной проверкой"
    return
  fi
  printf 'ИНФО метод=%s сообщение=%s ширина=%s высота=%s dpi_процент=%s\n' \
    "${function_ctx}" \
    "ожидаемая геометрия задана; запуск всё равно останется в PAUSED" \
    "${ARENA_EXPECTED_WIDTH}" \
    "${ARENA_EXPECTED_HEIGHT}" \
    "${ARENA_EXPECTED_DPI}"
}

stop_legacy_pid() {
  local function_ctx="${method_ctx}.stop_legacy_pid"
  local pid_file="$1"
  local expected="$2"
  local attempt
  local pid
  [[ -f "${pid_file}" ]] || return 0
  read -r pid <"${pid_file}" || true
  [[ "${pid:-}" =~ ^[0-9]+$ ]] || return 0
  [[ -r "/proc/${pid}/cmdline" ]] || return 0
  if tr '\0' ' ' <"/proc/${pid}/cmdline" | grep -Fq "${expected}"; then
    printf 'ИНФО метод=%s сообщение=%s pid=%s\n' \
      "${function_ctx}" \
      "останавливается подтверждённый устаревший процесс" \
      "${pid}"
    kill -TERM "${pid}" >/dev/null 2>&1 || true
    for ((attempt = 1; attempt <= 20; attempt++)); do
      kill -0 "${pid}" >/dev/null 2>&1 || break
      sleep 0.25
    done
    if kill -0 "${pid}" >/dev/null 2>&1; then
      fail "${function_ctx}" "подтверждённый устаревший процесс ${pid} не завершился"
    fi
  fi
}

require_command curl
require_command cut
require_command docker
require_command getent
require_command grep
require_command id
require_command python3
require_command sleep
require_command timeout
require_command tr
docker compose version >/dev/null 2>&1 ||
  fail "${method_ctx}" "Docker Compose недоступен"
configure_lm_paths

pause_failed=false
pause_existing_controller || pause_failed=true
stop_existing_compose_stack
stop_legacy_pid "${repo_root}/data/run/controller.pid" "${repo_root}/data/run/controller"
stop_legacy_pid "${repo_root}/data/run/ocr.pid" "uvicorn app.main:app"
[[ "${pause_failed}" == "false" ]] ||
  fail "${method_ctx}" "прежний стек остановлен, но обязательный переход controller в PAUSED завершился ошибкой; устраните причину и повторите запуск"

validate_geometry_environment

mkdir -p "${repo_root}/data/build" "${repo_root}/data/frames" "${repo_root}/data/run"
"${script_dir}/verify-qwen-model.sh"

# Освобождаем порт 1234 от старой локальной службы. Игра и Windows Agent этим не
# затрагиваются.
if command -v lms >/dev/null 2>&1; then
  if ! timeout 15s lms server stop >/dev/null 2>&1; then
    printf 'ПРЕДУПРЕЖДЕНИЕ метод=%s сообщение=%s\n' \
      "${method_ctx}.stop_host_lmstudio" \
      "локальный REST-сервер LM Studio не подтвердил остановку; запуск продолжится только после проверки контейнерного порта"
  fi
  if ! timeout 15s lms daemon down >/dev/null 2>&1; then
    printf 'ПРЕДУПРЕЖДЕНИЕ метод=%s сообщение=%s\n' \
      "${method_ctx}.stop_host_lmstudio" \
      "локальная служба LM Studio не подтвердила остановку"
  fi
fi

"${script_dir}/build-controller-linux.sh"

"${script_dir}/docker-build-safe.sh" \
  --tag arena-controller:0.2.0 \
  --file "${repo_root}/deploy/Dockerfile.controller" \
  "${repo_root}"
"${script_dir}/docker-build-safe.sh" \
  --tag arena-lmstudio:0.8b \
  --file "${repo_root}/deploy/Dockerfile.lmstudio" \
  "${repo_root}"
"${script_dir}/docker-build-safe.sh" \
  --tag arena-ocr:0.1.0 \
  --file "${repo_root}/services/ocr/Dockerfile" \
  "${repo_root}/services/ocr"
if ! docker compose --file "${compose_file}" up --detach --no-build; then
  fail "${method_ctx}.start_compose_stack" "не удалось запустить Docker Compose стек"
fi

wait_for_url "LM Studio" "${lmstudio_url}/api/v1/models" 90
"${script_dir}/verify-qwen-model.sh" --api-only "${lmstudio_url}"
wait_for_url "OCR" "http://127.0.0.1:8788/healthz" 60
wait_for_url "controller" "${controller_url}/healthz" 60

if ! curl --silent --show-error --fail --max-time 5 \
  --request PUT \
  --header 'Content-Type: application/json' \
  --data '{"mode":"PAUSED"}' \
  "${controller_mode_url}" >/dev/null; then
  fail "${method_ctx}.confirm_started_paused" \
    "запущенный controller не принял обязательный переход в PAUSED"
fi
if ! mode_json="$(curl --silent --show-error --fail --max-time 5 \
  "${controller_mode_url}")"; then
  fail "${method_ctx}.confirm_started_paused" \
    "не удалось прочитать режим запущенного controller"
fi
grep -Eq '"mode"[[:space:]]*:[[:space:]]*"PAUSED"' <<<"${mode_json}" ||
  fail "${method_ctx}.confirm_started_paused" "controller не подтвердил режим PAUSED"

printf 'ИНФО метод=%s сообщение=%s\n' \
  "${method_ctx}" \
  "изолированный WSL-стек запущен"
printf 'ИНФО метод=%s панель_управления=%s/\n' \
  "${method_ctx}" \
  "${controller_url}"
printf 'ИНФО метод=%s режим=PAUSED\n' "${method_ctx}"
printf 'ИНФО метод=%s модель_LM=%s сообщение=%s\n' \
  "${method_ctx}" \
  "qwen3.5-0.8b" \
  "модель загружается только по запросу"
docker compose --file "${compose_file}" ps ||
  fail "${method_ctx}" "не удалось вывести итоговое состояние Docker Compose"

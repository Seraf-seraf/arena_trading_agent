#!/usr/bin/env bash
set -Eeuo pipefail

readonly method_ctx="scripts.status-safe"
readonly script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
readonly repo_root="$(cd -- "${script_dir}/.." && pwd -P)"
readonly compose_file="${repo_root}/compose.safe.yml"
readonly controller_url="http://127.0.0.1:8787"
readonly lmstudio_url="http://127.0.0.1:1234"
declare -Ar expected_memory_bytes=(
  [controller]="402653184"
  [ocr]="805306368"
  [lmstudio]="1610612736"
)
declare -Ar expected_nano_cpus=(
  [controller]="1000000000"
  [ocr]="1000000000"
  [lmstudio]="1500000000"
)
declare -Ar expected_pids=(
  [controller]="64"
  [ocr]="96"
  [lmstudio]="160"
)

result=0

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

check_url() {
  local function_ctx="${method_ctx}.check_url"
  local name="$1"
  local url="$2"
  local response
  if response="$(curl --silent --show-error --fail --max-time 5 "${url}" 2>/dev/null)"; then
    printf 'ИНФО метод=%s сервис=%s состояние=ДОСТУПЕН ответ=%s\n' \
      "${function_ctx}" "${name}" "${response}"
  else
    printf 'ОШИБКА метод=%s сервис=%s состояние=НЕДОСТУПЕН адрес=%s\n' \
      "${function_ctx}" "${name}" "${url}" >&2
    result=1
  fi
}

check_container_limits() {
  local function_ctx="${method_ctx}.check_container_limits"
  local service="$1"
  local container
  local values
  local memory
  local memory_swap
  local nano_cpus
  local pids
  local expected_memory
  local expected_cpu
  local expected_pid

  expected_memory="${expected_memory_bytes[$service]:-}"
  expected_cpu="${expected_nano_cpus[$service]:-}"
  expected_pid="${expected_pids[$service]:-}"
  if [[ -z "${expected_memory}" || -z "${expected_cpu}" || -z "${expected_pid}" ]]; then
    printf 'ОШИБКА метод=%s сервис=%s сообщение=%s\n' \
      "${function_ctx}" \
      "${service}" \
      "для сервиса не заданы ожидаемые ресурсные ограничения" >&2
    result=1
    return
  fi

  if ! container="$(docker compose --file "${compose_file}" ps --all --quiet "${service}")"; then
    printf 'ОШИБКА метод=%s сервис=%s сообщение=%s\n' \
      "${function_ctx}" \
      "${service}" \
      "не удалось определить контейнер сервиса" >&2
    result=1
    return
  fi
  if [[ -z "${container}" ]]; then
    printf 'ОШИБКА метод=%s сервис=%s сообщение=%s\n' \
      "${function_ctx}" \
      "${service}" \
      "контейнер не создан" >&2
    result=1
    return
  fi
  if [[ "${container}" == *$'\n'* ]]; then
    printf 'ОШИБКА метод=%s сервис=%s сообщение=%s контейнеры=%s\n' \
      "${function_ctx}" \
      "${service}" \
      "обнаружено несколько контейнеров сервиса вместо одного" \
      "${container//$'\n'/,}" >&2
    result=1
    return
  fi
  if ! values="$(docker inspect \
    --format '{{.HostConfig.Memory}} {{.HostConfig.MemorySwap}} {{.HostConfig.NanoCpus}} {{.HostConfig.PidsLimit}}' \
    "${container}" 2>/dev/null)"; then
    printf 'ОШИБКА метод=%s сервис=%s контейнер=%s сообщение=%s\n' \
      "${function_ctx}" \
      "${service}" \
      "${container}" \
      "контейнер не найден" >&2
    result=1
    return
  fi
  read -r memory memory_swap nano_cpus pids <<<"${values}"
  if [[ "${memory}" != "${expected_memory}" ]] ||
    [[ "${memory_swap}" != "${expected_memory}" ]] ||
    [[ "${nano_cpus}" != "${expected_cpu}" ]] ||
    [[ "${pids}" != "${expected_pid}" ]]; then
    printf 'ОШИБКА метод=%s сервис=%s контейнер=%s сообщение=%s память_байт=%s ожидалась_память_байт=%s memory_swap_байт=%s nano_cpus=%s ожидалось_nano_cpus=%s pids=%s ожидалось_pids=%s\n' \
      "${function_ctx}" \
      "${service}" \
      "${container}" \
      "ресурсные ограничения не совпадают с безопасным профилем или разрешают подкачку" \
      "${memory}" \
      "${expected_memory}" \
      "${memory_swap}" \
      "${nano_cpus}" \
      "${expected_cpu}" \
      "${pids}" \
      "${expected_pid}" >&2
    result=1
    return
  fi
  printf 'ИНФО метод=%s сервис=%s контейнер=%s сообщение=%s память_байт=%s подкачка=%s nano_cpus=%s pids=%s\n' \
    "${function_ctx}" \
    "${service}" \
    "${container}" \
    "ресурсные ограничения подтверждены" \
    "${memory}" \
    "отключена" \
    "${nano_cpus}" \
    "${pids}"
}

controller_argument() {
  local function_ctx="${method_ctx}.controller_argument"
  local requested="$1"
  local container
  local previous=""
  local argument

  container="$(docker compose --file "${compose_file}" ps --all --quiet controller)"
  if [[ -z "${container}" ]]; then
    printf 'ОШИБКА метод=%s сообщение=%s\n' \
      "${function_ctx}" \
      "контейнер controller не создан" >&2
    return 1
  fi
  while IFS= read -r argument; do
    if [[ "${previous}" == "${requested}" ]]; then
      printf '%s' "${argument}"
      return 0
    fi
    previous="${argument}"
  done < <(docker inspect --format '{{range .Config.Cmd}}{{println .}}{{end}}' "${container}")
  printf 'ОШИБКА метод=%s сообщение=%s аргумент=%s\n' \
    "${function_ctx}" \
    "в команде controller не найден обязательный аргумент" \
    "${requested}" >&2
  return 1
}

check_controller_geometry() {
  local function_ctx="${method_ctx}.check_controller_geometry"
  local width
  local height
  local dpi

  if ! width="$(controller_argument -expected-width)" ||
    ! height="$(controller_argument -expected-height)" ||
    ! dpi="$(controller_argument -expected-dpi)"; then
    result=1
    return
  fi
  if [[ "${width}" =~ ^[1-9][0-9]*$ ]] &&
    [[ "${height}" =~ ^[1-9][0-9]*$ ]] &&
    [[ "${dpi}" =~ ^[1-9][0-9]*$ ]]; then
    printf 'ИНФО метод=%s сообщение=%s ширина=%s высота=%s dpi_процент=%s\n' \
      "${function_ctx}" \
      "ожидаемая геометрия controller настроена" \
      "${width}" \
      "${height}" \
      "${dpi}"
    return
  fi
  printf 'ИНФО метод=%s сообщение=%s ширина=%s высота=%s dpi_процент=%s\n' \
    "${function_ctx}" \
    "ожидаемая геометрия не настроена; SCAN и TRADE должны оставаться заблокированы" \
    "${width}" \
    "${height}" \
    "${dpi}"
}

require_command curl
require_command docker
require_command python3
docker compose version >/dev/null 2>&1 ||
  fail "${method_ctx}" "Docker Compose недоступен"

if ! compose_output="$(docker compose --file "${compose_file}" ps \
  --format 'table {{.Name}}\t{{.State}}\t{{.Status}}')"; then
  fail "${method_ctx}" "не удалось прочитать состояние Docker Compose"
fi
while IFS= read -r compose_status; do
  [[ -n "${compose_status}" ]] || continue
  printf 'ИНФО метод=%s источник=docker-compose состояние=%s\n' \
    "${method_ctx}" \
    "${compose_status}"
done <<<"${compose_output}"

check_container_limits controller
check_container_limits ocr
check_container_limits lmstudio
check_controller_geometry
if ! "${script_dir}/verify-qwen-model.sh" --api-only "${lmstudio_url}"; then
  result=1
fi
check_url "OCR" "http://127.0.0.1:8788/healthz"

if controller_state="$(curl --silent --show-error --fail --max-time 5 \
  "${controller_url}/api/v1/state" 2>/dev/null)"; then
  printf 'ИНФО метод=%s сервис=controller состояние=ДОСТУПЕН ответ=%s\n' \
    "${method_ctx}" "${controller_state}"
  if ! grep -Eq '"mode"[[:space:]]*:[[:space:]]*"(PAUSED|OBSERVE|SIMULATE)"' \
    <<<"${controller_state}"; then
    printf 'ОШИБКА метод=%s сообщение=%s\n' \
      "${method_ctx}" \
      "controller не подтвердил безопасный режим PAUSED, OBSERVE или SIMULATE" >&2
    result=1
  fi
else
  printf 'ОШИБКА метод=%s сервис=controller состояние=НЕДОСТУПЕН\n' \
    "${method_ctx}" >&2
  result=1
fi

mapfile -t running_containers < <(
  docker compose --file "${compose_file}" ps --quiet controller ocr lmstudio
)
if ((${#running_containers[@]} > 0)); then
  docker stats --no-stream \
    --format "ИНФО метод=${method_ctx}.docker_stats контейнер={{.Name}} память={{.MemUsage}} процессор={{.CPUPerc}}" \
    "${running_containers[@]}" 2>/dev/null || true
fi

exit "${result}"

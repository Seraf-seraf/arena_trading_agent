#!/usr/bin/env bash
set -Eeuo pipefail

readonly method_ctx="scripts.start-full-local"
readonly script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
readonly repo_root="$(cd -- "${script_dir}/.." && pwd -P)"
readonly run_dir="${repo_root}/data/run"
readonly log_dir="${repo_root}/data/logs"
readonly controller_bin="${repo_root}/data/build/controller-linux"
readonly agent_staged="${repo_root}/data/build/windows-agent.exe"
readonly runtime_config="${repo_root}/configs/runtime.local.json"
readonly screen_config="${repo_root}/configs/screens.local.json"
readonly database_path="${repo_root}/data/arena.db"
readonly recordings_path="${repo_root}/data/frames"
readonly ocr_python="${repo_root}/services/ocr/.venv/bin/python"
readonly controller_url="http://127.0.0.1:8787"
readonly ocr_url="http://127.0.0.1:8788"
readonly lmstudio_url="http://127.0.0.1:1234"
readonly windows_controller_url="ws://localhost:8787/ws/agent"

controller_pid=""
ocr_pid=""

fail() {
  local context="$1"
  shift
  printf 'ОШИБКА метод=%s сообщение=%s\n' "${context}" "$*" >&2
  exit 1
}

require_command() {
  local function_ctx="${method_ctx}.require_command"
  command -v "$1" >/dev/null 2>&1 || fail "${function_ctx}" "не найдена команда $1"
}

wait_for_url() {
  local function_ctx="${method_ctx}.wait_for_url"
  local service_name="$1"
  local url="$2"
  local attempts="$3"
  local attempt
  for ((attempt = 1; attempt <= attempts; attempt++)); do
    if curl --silent --show-error --fail --max-time 2 "${url}" >/dev/null 2>&1; then
      printf 'ИНФО метод=%s сообщение=%s адрес=%s\n' \
        "${function_ctx}" "${service_name} отвечает" "${url}"
      return 0
    fi
    sleep 1
  done
  fail "${function_ctx}" "${service_name} не ответил на ${url}"
}

wait_for_agent() {
  local function_ctx="${method_ctx}.wait_for_agent"
  local attempt
  local state
  for ((attempt = 1; attempt <= 20; attempt++)); do
    state="$(curl --silent --show-error --fail --max-time 2 \
      "${controller_url}/api/v1/state" 2>/dev/null || true)"
    if grep -Eq '"agent_id"[[:space:]]*:[[:space:]]*"windows-local"' <<<"${state}"; then
      printf 'ИНФО метод=%s сообщение=%s агент=%s\n' \
        "${function_ctx}" "Windows Agent подключён к controller" "windows-local"
      return 0
    fi
    sleep 1
  done
  fail "${function_ctx}" "Windows Agent не подключился к controller за 20 секунд"
}

pause_existing_controller() {
  local function_ctx="${method_ctx}.pause_existing_controller"
  if ! curl --silent --show-error --max-time 2 \
    "${controller_url}/api/v1/mode" >/dev/null 2>&1; then
    return 0
  fi
  curl --silent --show-error --fail --max-time 5 \
    --request PUT \
    --header 'Content-Type: application/json' \
    --data '{"mode":"PAUSED"}' \
    "${controller_url}/api/v1/mode" >/dev/null ||
    fail "${function_ctx}" "работающий controller не принял режим PAUSED"
  printf 'ИНФО метод=%s сообщение=%s\n' \
    "${function_ctx}" "работающий controller переведён в PAUSED"
}

stop_exact_binary() {
  local function_ctx="${method_ctx}.stop_exact_binary"
  local expected_binary="$1"
  local proc_exe
  local actual_binary
  local pid
  for proc_exe in /proc/[0-9]*/exe; do
    actual_binary="$(readlink -f -- "${proc_exe}" 2>/dev/null || true)"
    actual_binary="${actual_binary% (deleted)}"
    [[ "${actual_binary}" == "${expected_binary}" ]] || continue
    pid="${proc_exe#/proc/}"
    pid="${pid%/exe}"
    kill -TERM "${pid}" 2>/dev/null || true
    for _ in {1..20}; do
      kill -0 "${pid}" 2>/dev/null || break
      sleep 0.25
    done
    kill -0 "${pid}" 2>/dev/null &&
      fail "${function_ctx}" "процесс ${expected_binary} с PID ${pid} не завершился"
    printf 'ИНФО метод=%s сообщение=%s pid=%s\n' \
      "${function_ctx}" "остановлен предыдущий процесс" "${pid}"
  done
}

cleanup_failed_start() {
  local status=$?
  trap - EXIT
  if ((status == 0)); then
    return
  fi
  if [[ "${controller_pid}" =~ ^[0-9]+$ ]]; then
    kill -TERM "${controller_pid}" >/dev/null 2>&1 || true
  fi
  if [[ "${ocr_pid}" =~ ^[0-9]+$ ]]; then
    kill -TERM "${ocr_pid}" >/dev/null 2>&1 || true
  fi
  printf 'ОШИБКА метод=%s сообщение=%s код=%d\n' \
    "${method_ctx}.cleanup_failed_start" \
    "запуск не завершён; созданные WSL-процессы остановлены" \
    "${status}" >&2
  exit "${status}"
}

trap cleanup_failed_start EXIT

require_command curl
require_command grep
require_command nohup
require_command powershell.exe
require_command readlink
require_command sleep
require_command wslpath

for required_file in \
  "${controller_bin}" \
  "${agent_staged}" \
  "${runtime_config}" \
  "${screen_config}" \
  "${ocr_python}"; do
  [[ -f "${required_file}" ]] ||
    fail "${method_ctx}.validate_files" "не найден обязательный файл ${required_file}"
done

mkdir -p "${run_dir}" "${log_dir}" "${recordings_path}"

lmstudio_ready=false
for endpoint in /api/v1/models /v1/models /api/v0/models; do
  if curl --silent --show-error --fail --max-time 3 \
    "${lmstudio_url}${endpoint}" >/dev/null 2>&1; then
    lmstudio_ready=true
    printf 'ИНФО метод=%s сообщение=%s адрес=%s\n' \
      "${method_ctx}.check_lmstudio" \
      "LM Studio API доступен; существующий GUI/server не перезапускается" \
      "${lmstudio_url}${endpoint}"
    break
  fi
done
[[ "${lmstudio_ready}" == "true" ]] ||
  fail "${method_ctx}.check_lmstudio" \
    "LM Studio API недоступен на ${lmstudio_url}; запустите GUI и включите Local Server"

pause_existing_controller
stop_exact_binary "${controller_bin}"

if curl --silent --show-error --fail --max-time 2 \
  "${ocr_url}/healthz" >/dev/null 2>&1; then
  printf 'ИНФО метод=%s сообщение=%s\n' \
    "${method_ctx}.start_ocr" "уже работающий OCR будет использован повторно"
else
  nohup env \
    OMP_NUM_THREADS=1 \
    OMP_THREAD_LIMIT=1 \
    OPENBLAS_NUM_THREADS=1 \
    MKL_NUM_THREADS=1 \
    NUMEXPR_NUM_THREADS=1 \
    "${ocr_python}" -m uvicorn app.main:app \
      --app-dir "${repo_root}/services/ocr" \
      --host 127.0.0.1 \
      --port 8788 \
      >>"${log_dir}/ocr.log" 2>&1 &
  ocr_pid=$!
  printf '%s\n' "${ocr_pid}" >"${run_dir}/ocr.pid"
  wait_for_url "OCR" "${ocr_url}/healthz" 30
fi

nohup env \
  GOMAXPROCS=1 \
  GOMEMLIMIT=384MiB \
  "${controller_bin}" \
    -listen 0.0.0.0:8787 \
    -config "${runtime_config}" \
    -db "${database_path}" \
    -recordings "${recordings_path}" \
    -lm-studio "${lmstudio_url}" \
    -lm-model qwen3.5-0.8b \
    -lm-api-key= \
    -lm-auto-load=true \
    -lm-context 2048 \
    -ocr "${ocr_url}" \
    -screen-config "${screen_config}" \
    -observe-interval 5s \
    -expected-width 1920 \
    -expected-height 1080 \
    -expected-dpi 100 \
    -min-confidence 0.90 \
    >>"${log_dir}/controller.log" 2>&1 &
controller_pid=$!
printf '%s\n' "${controller_pid}" >"${run_dir}/controller.pid"
wait_for_url "controller" "${controller_url}/healthz" 30

screen_config_windows="$(wslpath -w "${screen_config}")"
agent_staged_windows="$(wslpath -w "${agent_staged}")"
agent_launcher_windows="$(wslpath -w "${script_dir}/start-windows-agent-full.ps1")"

powershell.exe -NoProfile -ExecutionPolicy Bypass \
  -File "${agent_launcher_windows}" \
  -StagedExecutable "${agent_staged_windows}" \
  -Controller "${windows_controller_url}" \
  -AgentId "windows-local" \
  -ProcessName "UAGame.exe" \
  -WindowTitle "Arena Breakout Infinite" \
  -ScreenConfig "${screen_config_windows}"

wait_for_agent

trap - EXIT
printf 'ИНФО метод=%s сообщение=%s\n' \
  "${method_ctx}" \
  "OCR, controller и Windows Agent запущены; полный ввод разрешён, стартовый режим PAUSED"
printf 'ИНФО метод=%s панель=%s/ режим=%s\n' \
  "${method_ctx}" "${controller_url}" "PAUSED"
printf 'ИНФО метод=%s логи_controller=%s логи_ocr=%s\n' \
  "${method_ctx}" "${log_dir}/controller.log" "${log_dir}/ocr.log"

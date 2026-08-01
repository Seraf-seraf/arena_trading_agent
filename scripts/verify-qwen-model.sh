#!/usr/bin/env bash
set -Eeuo pipefail

readonly model_filename="Qwen3.5-0.8B-Q4_K_M.gguf"
readonly projector_filename="mmproj-Qwen3.5-0.8B-BF16.gguf"
readonly model_key="qwen3.5-0.8b"
readonly model_subdir="models/lmstudio-community/Qwen3.5-0.8B-GGUF"
readonly expected_model_size="527502816"
readonly expected_projector_size="207345952"
readonly expected_model_sha256="f5b14da98939b60bbe1019a964eba656407e1e0b64f1fe3003ff6d650e93bfec"
readonly expected_projector_sha256="6fdd1b4bdc3d2ae8bd15d783e23260dd07dcf83f45604a21dabfd6efad8f8bc5"
readonly method_ctx="scripts.verify-qwen-model"
model_dir=""

fail() {
  local context="$1"
  shift
  printf 'ОШИБКА метод=%s сообщение=%s\n' "${context}" "$*" >&2
  exit 1
}

resolve_model_dir() {
  local function_ctx="${method_ctx}.resolve_model_dir"
  local arena_user_home
  local lmstudio_home

  command -v getent >/dev/null 2>&1 ||
    fail "${function_ctx}" "не найдена команда getent"
  command -v cut >/dev/null 2>&1 ||
    fail "${function_ctx}" "не найдена команда cut"
  command -v id >/dev/null 2>&1 ||
    fail "${function_ctx}" "не найдена команда id"
  if ! arena_user_home="$(getent passwd "$(id -u)" | cut -d: -f6)"; then
    fail "${function_ctx}" "не удалось прочитать домашний каталог пользователя"
  fi
  [[ -n "${arena_user_home}" && "${arena_user_home}" == /* ]] ||
    fail "${function_ctx}" "не удалось определить абсолютный домашний каталог пользователя"
  lmstudio_home="${ARENA_LMSTUDIO_HOME:-${arena_user_home}/.lmstudio}"
  [[ "${lmstudio_home}" == /* ]] ||
    fail "${function_ctx}" "ARENA_LMSTUDIO_HOME должен быть абсолютным путём"
  model_dir="${ARENA_LM_MODEL_DIR:-${lmstudio_home}/${model_subdir}}"
  [[ "${model_dir}" == /* ]] ||
    fail "${function_ctx}" "ARENA_LM_MODEL_DIR должен быть абсолютным путём"
}

check_file() {
  local function_ctx="${method_ctx}.check_file"
  local path="$1"
  local expected_size="$2"
  local expected_sha256="$3"
  local actual_size
  local actual_sha256

  [[ -f "${path}" ]] || fail "${function_ctx}" "не найден файл ${path}"
  actual_size="$(stat -c '%s' "${path}")"
  [[ "${actual_size}" == "${expected_size}" ]] ||
    fail "${function_ctx}" "${path}: размер ${actual_size}, ожидался ${expected_size}; файл неполный или выбрана другая квантовка"
  actual_sha256="$(sha256sum -- "${path}")"
  actual_sha256="${actual_sha256%% *}"
  [[ "${actual_sha256}" == "${expected_sha256}" ]] ||
    fail "${function_ctx}" "${path}: SHA-256 ${actual_sha256}, ожидался ${expected_sha256}"
}

check_files() {
  local function_ctx="${method_ctx}.check_files"
  local model_path="${model_dir}/${model_filename}"
  local projector_path="${model_dir}/${projector_filename}"
  local unexpected_quant

  command -v find >/dev/null 2>&1 ||
    fail "${function_ctx}" "не найдена команда find"
  command -v grep >/dev/null 2>&1 ||
    fail "${function_ctx}" "не найдена команда grep"
  command -v sha256sum >/dev/null 2>&1 ||
    fail "${function_ctx}" "не найдена команда sha256sum"
  command -v stat >/dev/null 2>&1 ||
    fail "${function_ctx}" "не найдена команда stat"

  if find "${model_dir}" -maxdepth 1 -type f \
    -iname '*qwen3.5-0.8b*.part' -print -quit 2>/dev/null | grep -q .; then
    fail "${function_ctx}" "в ${model_dir} остались .part-файлы; дождитесь завершения загрузки"
  fi

  check_file "${model_path}" "${expected_model_size}" "${expected_model_sha256}"
  check_file "${projector_path}" "${expected_projector_size}" "${expected_projector_sha256}"

  unexpected_quant="$(find "${model_dir}" -maxdepth 1 -type f \
    -iname 'Qwen3.5-0.8B-*.gguf' ! -name "${model_filename}" -print -quit 2>/dev/null)"
  [[ -z "${unexpected_quant}" ]] ||
    fail "${function_ctx}" "найдена другая квантовка ${unexpected_quant}; запуск разрешён только с Q4_K_M"

  printf 'ИНФО метод=%s сообщение=%s ключ_модели=%s квантовка=%s проектор=%s\n' \
    "${function_ctx}" \
    "файлы модели проверены" \
    "${model_key}" \
    "Q4_K_M" \
    "BF16"
  printf 'ИНФО метод=%s файл_модели=%s\n' "${function_ctx}" "${model_path}"
  printf 'ИНФО метод=%s файл_проектора=%s\n' "${function_ctx}" "${projector_path}"
}

check_api() {
  local function_ctx="${method_ctx}.check_api"
  local base_url="$1"
  local response
  local parsed
  local status
  local detail

  command -v curl >/dev/null 2>&1 ||
    fail "${function_ctx}" "не найдена команда curl"
  command -v python3 >/dev/null 2>&1 ||
    fail "${function_ctx}" "не найдена команда python3 для строгой проверки JSON"
  base_url="${base_url%/}"
  [[ "${base_url}" == http://* || "${base_url}" == https://* ]] ||
    fail "${function_ctx}" "URL LM Studio должен использовать http или https"
  if ! response="$(curl --silent --show-error --fail --max-time 5 \
    "${base_url}/api/v1/models")"; then
    fail "${function_ctx}" "не удалось получить список моделей LM Studio"
  fi
  if ! parsed="$(python3 -c '
import json
import sys

expected = sys.argv[1]
try:
    payload = json.load(sys.stdin)
except Exception:
    print("INVALID_JSON")
    raise SystemExit(0)
models = payload.get("models") if isinstance(payload, dict) else None
if not isinstance(models, list):
    print("INVALID_MODELS")
    raise SystemExit(0)
matches = [
    item for item in models
    if isinstance(item, dict) and item.get("key") == expected
]
if not matches:
    print("NOT_FOUND")
    raise SystemExit(0)
if len(matches) != 1:
    print("DUPLICATE")
    raise SystemExit(0)
model = matches[0]
if model.get("type") != "llm":
    print("WRONG_TYPE\t" + str(model.get("type", "")))
    raise SystemExit(0)
capabilities = model.get("capabilities")
if not isinstance(capabilities, dict) or capabilities.get("vision") is not True:
    print("NO_VISION")
    raise SystemExit(0)
unexpected = []
for item in models:
    if not isinstance(item, dict) or item.get("key") == expected:
        continue
    loaded = item.get("loaded_instances")
    if isinstance(loaded, list) and loaded:
        key = " ".join(str(item.get("key", "")).split())
        unexpected.append(key or "<без ключа>")
if unexpected:
    print("UNEXPECTED_LOADED\t" + ",".join(unexpected))
    raise SystemExit(0)
print("OK")
' "${model_key}" <<<"${response}")"; then
    fail "${function_ctx}" "не удалось выполнить строгую проверку ответа LM Studio"
  fi
  IFS=$'\t' read -r status detail <<<"${parsed}"
  case "${status}" in
  OK)
    printf 'ИНФО метод=%s сообщение=%s ключ_модели=%s capability=%s посторонние_загруженные_модели=%s\n' \
      "${function_ctx}" \
      "LM Studio публикует ожидаемую vision-модель" \
      "${model_key}" \
      "vision" \
      "нет"
    ;;
  INVALID_JSON | INVALID_MODELS)
    fail "${function_ctx}" "LM Studio вернула некорректный JSON списка моделей"
    ;;
  NOT_FOUND)
    fail "${function_ctx}" "LM Studio не публикует обязательный ключ модели ${model_key}"
    ;;
  DUPLICATE)
    fail "${function_ctx}" "LM Studio вернула несколько записей для ключа ${model_key}"
    ;;
  WRONG_TYPE)
    fail "${function_ctx}" "модель ${model_key} имеет тип ${detail:-не задан}, ожидался llm"
    ;;
  NO_VISION)
    fail "${function_ctx}" "модель ${model_key} не подтверждает capability vision"
    ;;
  UNEXPECTED_LOADED)
    fail "${function_ctx}" "в LM Studio загружены посторонние модели: ${detail:-ключ не указан}"
    ;;
  *)
    fail "${function_ctx}" "получен неизвестный результат проверки списка моделей: ${parsed}"
    ;;
  esac
}

case "$#" in
0)
  resolve_model_dir
  check_files
  ;;
2)
  [[ "$1" == "--api-only" ]] ||
    fail "${method_ctx}" "поддерживается только параметр --api-only <URL>"
  check_api "$2"
  ;;
*)
  fail "${method_ctx}" "использование: verify-qwen-model.sh [--api-only <URL>]"
  ;;
esac

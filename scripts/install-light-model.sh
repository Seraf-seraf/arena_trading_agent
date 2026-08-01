#!/usr/bin/env bash
set -Eeuo pipefail

readonly repository="https://huggingface.co/lmstudio-community/Qwen3.5-0.8B-GGUF/resolve/main"
readonly model_filename="Qwen3.5-0.8B-Q4_K_M.gguf"
readonly projector_filename="mmproj-Qwen3.5-0.8B-BF16.gguf"
readonly method_ctx="scripts.install-light-model"
readonly model_subdir="models/lmstudio-community/Qwen3.5-0.8B-GGUF"

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
command -v cut >/dev/null 2>&1 || {
  printf 'ОШИБКА метод=%s сообщение=%s\n' \
    "${method_ctx}" \
    "не найдена команда cut" >&2
  exit 1
}
command -v getent >/dev/null 2>&1 || {
  printf 'ОШИБКА метод=%s сообщение=%s\n' \
    "${method_ctx}" \
    "не найдена команда getent" >&2
  exit 1
}
command -v id >/dev/null 2>&1 || {
  printf 'ОШИБКА метод=%s сообщение=%s\n' \
    "${method_ctx}" \
    "не найдена команда id" >&2
  exit 1
}
if ! arena_user_home="$(getent passwd "$(id -u)" | cut -d: -f6)"; then
  printf 'ОШИБКА метод=%s сообщение=%s\n' \
    "${method_ctx}" \
    "не удалось прочитать домашний каталог пользователя" >&2
  exit 1
fi
[[ -n "${arena_user_home}" && "${arena_user_home}" == /* ]] || {
  printf 'ОШИБКА метод=%s сообщение=%s\n' \
    "${method_ctx}" \
    "не удалось определить абсолютный домашний каталог пользователя" >&2
  exit 1
}
lmstudio_home="${ARENA_LMSTUDIO_HOME:-${arena_user_home}/.lmstudio}"
if [[ -n "${ARENA_LM_MODEL_DIR:-}" ]]; then
  model_dir="${ARENA_LM_MODEL_DIR}"
else
  model_dir="${lmstudio_home}/${model_subdir}"
fi
[[ "${lmstudio_home}" == /* && "${model_dir}" == /* ]] || {
  printf 'ОШИБКА метод=%s сообщение=%s\n' \
    "${method_ctx}" \
    "ARENA_LMSTUDIO_HOME и ARENA_LM_MODEL_DIR должны быть абсолютными путями" >&2
  exit 1
}
export ARENA_LMSTUDIO_HOME="${lmstudio_home}"
export ARENA_LM_MODEL_DIR="${model_dir}"
printf 'ИНФО метод=%s сообщение=%s lmstudio_home=%s каталог_модели=%s\n' \
  "${method_ctx}" \
  "каталоги LM Studio согласованы" \
  "${ARENA_LMSTUDIO_HOME}" \
  "${ARENA_LM_MODEL_DIR}"

command -v curl >/dev/null 2>&1 || {
  printf 'ОШИБКА метод=%s сообщение=%s\n' \
    "${method_ctx}" \
    "не найдена команда curl" >&2
  exit 1
}
mkdir -p "${model_dir}"

download() {
  local function_ctx="${method_ctx}.download"
  local filename="$1"
  local destination="${model_dir}/${filename}"
  if [[ -f "${destination}" ]]; then
    printf 'ИНФО метод=%s сообщение=%s путь=%s\n' \
      "${function_ctx}" \
      "файл модели уже загружен" \
      "${destination}"
    return
  fi
  printf 'ИНФО метод=%s сообщение=%s файл=%s\n' \
    "${function_ctx}" \
    "начинается загрузка файла модели" \
    "${filename}"
  curl --fail --location --retry 5 --retry-delay 2 \
    --continue-at - \
    --output "${destination}.part" \
    "${repository}/${filename}"
  mv -- "${destination}.part" "${destination}"
  printf 'ИНФО метод=%s сообщение=%s путь=%s\n' \
    "${function_ctx}" \
    "загрузка файла модели завершена" \
    "${destination}"
}

download "${model_filename}"
download "${projector_filename}"
"${script_dir}/verify-qwen-model.sh"

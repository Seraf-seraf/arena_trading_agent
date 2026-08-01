# Эксплуатация и восстановление после WSL crash

## Граница безопасности

Перезапуск WSL не должен продолжать прерванное денежное действие. Controller
создаётся в `PAUSED`, а безопасный startup дополнительно отправляет
`{"mode":"PAUSED"}` после проверки HTTP. Windows Agent для наблюдения запускается
без `-allow-input`, поэтому даже ошибочный запрос действия не дойдёт до
`SendInput`.

Безопасными для первичного запуска считаются `PAUSED`, `OBSERVE` и `SIMULATE`.
Режим `SCAN` технически разрешает `ACTION_REQUEST` и потому не является
read-only.

Скрипты запуска не переводят систему в `SCAN` или `TRADE`. Если ожидаемая
геометрия не задана, controller получает нулевые значения и обязан отклонить
любой переход в режим с вводом.

## Автоматическое восстановление

```bash
cd /home/seraf/arena_trading_agent
chmod +x scripts/*.sh
./scripts/start-wsl-safe.sh
./scripts/status-safe.sh
```

Опциональная локальная runtime-конфигурация с calibrated detector должна
находиться внутри `configs/`, потому что каталог монтируется в controller
read-only:

```bash
export ARENA_RUNTIME_CONFIG_CONTAINER=/app/configs/runtime.local.json
./scripts/start-wsl-safe.sh
```

До проверки модели и сборки скрипт требует от отвечающего controller переход в
`PAUSED`, подтверждает его через API и полностью останавливает прежний
compose-стек. Ошибка перехода не игнорируется: стек останавливается, а startup
завершается с ошибкой до тяжёлых операций. Поэтому старый runtime и ограниченный
build-контейнер не конкурируют за память.

Каталоги LM Studio задаются согласованной парой абсолютных путей. Если задан
только `ARENA_LMSTUDIO_HOME`, startup выводит каталог Qwen внутри него. Для
модели вне этого home задайте оба значения:

```bash
export ARENA_LMSTUDIO_HOME=/home/seraf/.lmstudio
export ARENA_LM_MODEL_DIR=/srv/arena-models/Qwen3.5-0.8B-GGUF
./scripts/start-wsl-safe.sh
```

Второй каталог монтируется в контейнер read-only. При прямом вызове
`docker compose`, минуя safe startup, также экспортируйте обе переменные.
Готовый шаблон для `source`:
[`safe-runtime.env.example`](safe-runtime.env.example). Локальную копию храните
в игнорируемом каталоге `data/run/`, а не меняйте проверяемый шаблон:

```bash
cp docs/safe-runtime.env.example data/run/safe-runtime.env
# Проверьте абсолютные пути перед подключением.
source data/run/safe-runtime.env
./scripts/start-wsl-safe.sh
```

`ARENA_EXPECTED_WIDTH`, `ARENA_EXPECTED_HEIGHT` и `ARENA_EXPECTED_DPI`
задаются только одновременно и только положительными целыми числами. Для
`PAUSED`, `OBSERVE` и `SIMULATE` их можно не задавать. Для `SCAN` и `TRADE`
все три переменные обязательны:

```bash
export ARENA_EXPECTED_WIDTH=1920
export ARENA_EXPECTED_HEIGHT=1080
export ARENA_EXPECTED_DPI=100
./scripts/start-wsl-safe.sh
```

Это ожидаемые размеры именно client area, а DPI задаётся в процентах:
`96 DPI = 100`, `120 DPI = 125`, `144 DPI = 150`. Значения примера нельзя
копировать без измерения текущего окна.

Артефакты:

| Путь                          | Содержимое                                      |
| ----------------------------- | ----------------------------------------------- |
| `data/build/controller-linux` | controller, собранный в ограниченном контейнере |
| `data/arena.db`               | SQLite                                          |
| `data/frames/`                | кадры и observation sidecar                     |

Журналы текущего стека находятся в Docker и читаются безопасной командой
`docker compose -f compose.safe.yml logs --tail 100`. Файлы в `data/run/` могут
остаться от старого host-запуска; startup использует их только для точечного
завершения процесса, если PID и его командная строка совпали.

## Ограничения ресурсов

Все production-сервисы запускаются в Docker с жёсткими лимитами:

| Сервис       |   Память | CPU | PIDs |
| ------------ | -------: | --: | ---: |
| `lmstudio`   | 1536 MiB | 1.5 |  160 |
| `ocr`        |  768 MiB | 1.0 |   96 |
| `controller` |  384 MiB | 1.0 |   64 |

Для каждого контейнера `memswap_limit` равен `mem_limit`. В Docker это
запрещает использовать swap сверх выделенной памяти и не позволяет одному
процессу вытеснить WSL по памяти. Go-сборки и тесты запускаются через
`scripts/docker-go.sh` с 1024 MiB, одним CPU, 128 PID и без сети. BuildKit
создаётся `scripts/docker-build-safe.sh` с 1024 MiB, одним CPU и без swap.

Не увеличивайте лимиты после OOM без одновременной проверки общего лимита WSL.
Пиковый лимит суммы трёх runtime-контейнеров — 2688 MiB, не считая Docker/WSL.
Safe startup сначала останавливает этот стек и лишь затем использует отдельный
build-контейнер с лимитом 1024 MiB, поэтому эти лимиты не суммируются.

Проверить состояние без изменения режима, ввода или загрузки модели:

```bash
./scripts/status-safe.sh
docker compose -f compose.safe.yml ps
docker compose -f compose.safe.yml logs --tail 100 controller
curl --max-time 5 http://127.0.0.1:8787/api/v1/mode
curl --max-time 5 http://127.0.0.1:8787/api/v1/state
```

`status-safe.sh` проверяет режим, точные container memory/no-swap/CPU/PID
limits, параметры ожидаемой геометрии и read-only health endpoints. Ответ
`LM Studio /api/v1/models` дополнительно проверяется на exact key
`qwen3.5-0.8b`, `type=llm`, capability `vision` и отсутствие посторонних
загруженных моделей. Эта проверка перечисляет модели, но не загружает их.

## Ручной порядок

### 1. Проверить модель

Используется только:

```text
LM Studio runtime key: qwen3.5-0.8b
Download artifact:     lmstudio-community/Qwen3.5-0.8B-GGUF
Qwen3.5-0.8B-Q4_K_M.gguf
  size:   527 502 816 байт
  sha256: f5b14da98939b60bbe1019a964eba656407e1e0b64f1fe3003ff6d650e93bfec
mmproj-Qwen3.5-0.8B-BF16.gguf
  size:   207 345 952 байт
  sha256: 6fdd1b4bdc3d2ae8bd15d783e23260dd07dcf83f45604a21dabfd6efad8f8bc5
```

Проверка:

```bash
./scripts/verify-qwen-model.sh
```

Если модель ещё не установлена:

```bash
./scripts/install-light-model.sh
```

Не переименовывайте `.part` до окончания загрузки. Не подставляйте Gemma или
другую модель под runtime key `qwen3.5-0.8b`: controller проверяет модель через
API LM Studio и передаёт кадр как vision input.

### 2. Измерить client area и DPI без ввода

В PowerShell из корня проекта:

```powershell
powershell -ExecutionPolicy Bypass `
  -File .\scripts\inspect-game-window.ps1 `
  -ProcessName UAGame
```

Скрипт только читает дескриптор окна, client area и DPI. Он не вызывает
`SetForegroundWindow`, не двигает мышь и не отправляет клавиши. Перенесите
выданные `ARENA_EXPECTED_WIDTH`, `ARENA_EXPECTED_HEIGHT` и
`ARENA_EXPECTED_DPI` в окружение WSL перед запуском стека. Повторите измерение
после изменения разрешения, оконного режима или Windows scaling.

### 3. Запустить LM Studio API

Запустите установленный AppImage LM Studio, в разделе локального сервера
укажите `127.0.0.1:1234` и включите загрузку модели по требованию. CLI `lms`
для этого рабочего процесса не нужен.

Проверьте сервер напрямую через API:

```bash
curl --fail --max-time 5 http://127.0.0.1:1234/v1/models
curl --fail --max-time 5 http://127.0.0.1:1234/api/v0/models
```

Controller загружает ровно `qwen3.5-0.8b` при первом vision health/grounding
запросе. Параметры: context `2048`, flash attention и хранение KV cache в RAM;
GPU offload отключён.

Проверить состояния моделей без CLI:

```bash
curl --silent http://127.0.0.1:1234/api/v0/models
```

В production-наблюдении должна загружаться только `qwen3.5-0.8b`. Управляйте
загрузкой и выгрузкой через интерфейс LM Studio и не завершайте модельные
процессы массово.

### 4. Запустить OCR

Первая установка:

```bash
cd services/ocr
uv sync --extra dev
```

Запуск:

```bash
uv run --project services/ocr \
  uvicorn app.main:app \
  --app-dir services/ocr \
  --host 127.0.0.1 \
  --port 8788
```

Проверка:

```bash
curl http://127.0.0.1:8788/healthz
```

ONNX-модель загружается лениво: первый реальный OCR-запрос может быть медленнее
последующих.

### 5. Запустить controller

```bash
./scripts/build-controller-linux.sh

./data/build/controller-linux \
  -listen 127.0.0.1:8787 \
  -config /home/seraf/arena_trading_agent/configs/runtime.example.json \
  -db /home/seraf/arena_trading_agent/data/arena.db \
  -recordings /home/seraf/arena_trading_agent/data/frames \
  -lm-studio http://127.0.0.1:1234 \
  -lm-model qwen3.5-0.8b \
  -lm-auto-load=true \
  -lm-context 2048 \
  -ocr http://127.0.0.1:8788
```

Проверьте стартовый режим:

```bash
curl http://127.0.0.1:8787/healthz
curl http://127.0.0.1:8787/api/v1/mode
```

Ожидается `PAUSED`. Не публикуйте controller или LM Studio на `0.0.0.0`: у
локального dashboard нет аутентификации.

Для ручного запуска с последующим `SCAN` или `TRADE` дополнительно обязательны:

```text
-expected-width <измеренная client width>
-expected-height <измеренная client height>
-expected-dpi <измеренный DPI percent>
```

Даже с этими параметрами controller стартует в `PAUSED`.

### 6. Подключить Windows Agent без ввода

Сборка:

```bash
./scripts/build-windows-agent.sh
```

Перед заменой установленного файла завершите только известный старый процесс
`windows-agent.exe`, затем скопируйте `data/build/windows-agent.exe` в
`%LOCALAPPDATA%\ArenaTradingAgent\`.

Запуск в PowerShell:

```powershell
powershell -ExecutionPolicy Bypass `
  -File .\scripts\start-windows-agent-observe.ps1 `
  -Executable "$env:LOCALAPPDATA\ArenaTradingAgent\windows-agent.exe"
```

Скрипт не имеет параметра для live input. Признаки подключения:

```bash
curl http://127.0.0.1:8787/api/v1/state
```

У агента без ввода отсутствуют features `sequential_actions` и `send_input`.
Это ещё один независимый запрет `SCAN`/`TRADE`: для live input необходим
отдельный осознанный запуск с `-allow-input` и откалиброванным
`-screen-config`. Без завершённой калибровки такой запуск запрещён.

## Проверка OBSERVE и SIMULATE

Игра должна быть foreground и не свёрнута.

```bash
curl -X PUT http://127.0.0.1:8787/api/v1/mode \
  -H 'Content-Type: application/json' \
  -d '{"mode":"OBSERVE"}'

curl -X POST http://127.0.0.1:8787/api/v1/runtime/observe

curl -X POST \
  'http://127.0.0.1:8787/api/v1/runtime/preflight?trade=false'
```

`OBSERVE` повторяет распознавание с интервалом controller. Для единичной
проверки можно оставить `PAUSED` и вызвать `/runtime/observe` вручную.

```bash
curl -X PUT http://127.0.0.1:8787/api/v1/mode \
  -H 'Content-Type: application/json' \
  -d '{"mode":"SIMULATE"}'
```

`SIMULATE` не открывает input gate и пересчитывает возможности по уже
сохранённым котировкам и снимкам. Он не запускает наблюдение или input-capable
сканеры и не отправляет `ACTION_REQUEST`.

## Аварийная остановка

На Windows нажмите `Ctrl+Alt+F12`. Также можно отправить:

```bash
curl -X POST \
  http://127.0.0.1:8787/api/v1/agents/windows-local/commands \
  -H 'Content-Type: application/json' \
  -d '{"type":"EMERGENCY_STOP","reason":"ручная остановка"}'
```

После emergency stop выясните причину и перезапустите агент без
`-allow-input`. Не пытайтесь продолжить частичную сделку до reconciliation.

## Диагностика

### LM Studio не отвечает

```bash
timeout 8s lms daemon status
timeout 8s lms server status
curl --max-time 5 http://127.0.0.1:1234/api/v1/models
```

Если CLI завис после crash, не удаляйте `.lmstudio`. Проверьте процессы
`llmster`, завершите только подтверждённую зависшую копию и снова выполните
`lms daemon up`.

### Модель не найдена

Проверьте отсутствие `.part`, оба размера через
`scripts/verify-qwen-model.sh`, затем перезапустите daemon/API для повторного
индексирования. В `/api/v1/models` должен присутствовать runtime key
`qwen3.5-0.8b` с capability `vision`.

### Кадр чёрный

Игра должна быть foreground. Adaptive capture сначала использует GDI окна, а
при пустом результате — desktop capture с проверкой foreground. Не перекрывайте
игру другим окном во время калибровочного кадра.

### Windows Agent не подключается

Проверьте `curl http://localhost:8787/healthz` из Windows и URL
`ws://localhost:8787/ws/agent`. Если Windows↔WSL localhost forwarding не
работает, сначала исправьте WSL networking; не открывайте dashboard на всю сеть
как постоянный обходной путь.

### Вернуть безопасный режим

```bash
curl -X PUT http://127.0.0.1:8787/api/v1/mode \
  -H 'Content-Type: application/json' \
  -d '{"mode":"PAUSED"}'
```

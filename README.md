# Arena Trading Agent

Локальный runtime v1 для наблюдения, анализа и последующего исполнения
торговых сценариев Arena Breakout Infinite. Архитектура разделена по границе
ОС:

- `windows-agent.exe` на Windows — окно игры, захват кадров, единственная
  последовательная очередь ввода и аварийная остановка;
- `controller` в WSL — состояние сессии, распознавание, навигация, экономика,
  риск, журнал и dashboard;
- LM Studio — только visual grounding неизвестных экранов;
- OCR Service — только чтение заданных ROI;
- SQLite — постоянное состояние и аудит.

LM Studio и OCR не могут нажимать кнопки и не принимают экономических решений.

## Безопасность по умолчанию

`controller` всегда стартует в `PAUSED`. `windows-agent.exe` по умолчанию
собирает кадры, но вообще не создаёт WinAPI `SendInputDriver`. Для ввода нужны
сразу два независимых условия: агент запущен с `-allow-input`, а controller
переведён в `SCAN` или `TRADE`.

Для калибровки и текущих проверок используйте только:

- `PAUSED` — никакой автоматики;
- `OBSERVE` — периодический захват и распознавание;
- `SIMULATE` — расчёты без разрешения ввода.

`SCAN` и `TRADE` допускают ввод. Не включайте их до завершения калибровки и
реального preflight. Обычный физический ввод пользователя не приостанавливает
автоматику. Аварийная комбинация на Windows: `Ctrl+Alt+F12`.

## Модель

Зафиксирована одна лёгкая vision-модель:
[`lmstudio-community/Qwen3.5-0.8B-GGUF`](https://huggingface.co/lmstudio-community/Qwen3.5-0.8B-GGUF),
квантовка `Q4_K_M`, вместе с
`mmproj-Qwen3.5-0.8B-BF16.gguf`. Фактический runtime key локального API —
`qwen3.5-0.8b`. Общий размер файлов — около 735 МБ. Gemma не используется.

Установка через LM Studio CLI:

```bash
./scripts/install-light-model.sh
```

Проверка сверяет размеры и SHA-256 обоих полностью загруженных файлов и
отклоняет `.part`. При нестандартном размещении экспортируйте абсолютный
`ARENA_LMSTUDIO_HOME`; каталог модели по умолчанию будет вычислен внутри него.
Если GGUF-файлы находятся вне LM Studio home, дополнительно экспортируйте
абсолютный `ARENA_LM_MODEL_DIR`: safe compose смонтирует его в ожидаемый каталог
read-only. Согласованный shell-пример:
[`docs/safe-runtime.env.example`](docs/safe-runtime.env.example).

## Запуск после падения WSL

Из корня репозитория:

```bash
chmod +x scripts/*.sh
./scripts/start-wsl-safe.sh
./scripts/status-safe.sh
```

Сценарий:

1. требует от уже работающего controller режим `PAUSED`, затем полностью
   останавливает прежний compose-стек;
2. проверяет Qwen3.5-0.8B и vision projector;
3. собирает образы с ограничением памяти уже без параллельного runtime-стека;
4. поднимает daemon и локальный API LM Studio на `127.0.0.1:1234`;
5. подтверждает exact key `qwen3.5-0.8b`, capability `vision` и отсутствие
   посторонних загруженных моделей;
6. поднимает OCR и controller и явно подтверждает `PAUSED`.

Логи текущего стека находятся в Docker. `data/run/` используется только для
точечной остановки устаревших host-процессов. Dashboard:
<http://127.0.0.1:8787/>.

Подробный ручной порядок и диагностика:
[`docs/operations.md`](docs/operations.md).

## Windows Agent

Полный локальный запуск из WSL одной командой (LM Studio GUI и Local Server
должны быть уже включены):

```bash
./scripts/start-full-local.sh
```

Скрипт запускает OCR, controller и установленный Windows Agent, передаёт
геометрию `1920x1080 @ 100%`, локальные конфиги и `-allow-input`. Controller
намеренно остаётся в `PAUSED` до выбора `SCAN` или `TRADE` в панели.

Сборка в WSL не заменяет уже установленный бинарник:

```bash
./scripts/build-windows-agent.sh
```

Результат: `data/build/windows-agent.exe`. После явной остановки старого
процесса скопируйте его в Windows, например в
`%LOCALAPPDATA%\ArenaTradingAgent\windows-agent.exe`.

Безопасный запуск из PowerShell:

```powershell
powershell -ExecutionPolicy Bypass `
  -File .\scripts\start-windows-agent-observe.ps1
```

Скрипт намеренно не принимает и не передаёт `-allow-input`. Агент сам
переподключается к `ws://localhost:8787/ws/agent`, поэтому после перезапуска WSL
перезапускать работающий Windows-процесс обычно не нужно.

## Controller API

Состояние:

```bash
curl http://127.0.0.1:8787/healthz
curl http://127.0.0.1:8787/api/v1/state
curl http://127.0.0.1:8787/api/v1/runtime
```

Включить безопасное наблюдение и запросить один свежий кадр:

```bash
curl -X PUT http://127.0.0.1:8787/api/v1/mode \
  -H 'Content-Type: application/json' \
  -d '{"mode":"OBSERVE"}'

curl -X POST http://127.0.0.1:8787/api/v1/runtime/observe
```

Перейти в симуляцию без ввода:

```bash
curl -X PUT http://127.0.0.1:8787/api/v1/mode \
  -H 'Content-Type: application/json' \
  -d '{"mode":"SIMULATE"}'
```

`SIMULATE` пересчитывает возможности по уже сохранённым данным, но не запускает
наблюдение и не открывает input gate. Для отдельной проверки сервисов, окна и
распознавания без требования ввода:

```bash
curl -X POST \
  'http://127.0.0.1:8787/api/v1/runtime/preflight?trade=false'
```

Основные маршруты:

- `GET /api/v1/state` — режим и подключения;
- `GET /api/v1/frame?agent_id=...` — последний кадр в JSON;
- `GET /api/v1/agents/{id}/frame?raw=true` — исходные байты кадра;
- `GET|PUT /api/v1/mode` — режим;
- `GET /api/v1/runtime` — состояние session coordinator;
- `POST /api/v1/runtime/observe` — свежий кадр и `Observation`;
- `POST /api/v1/runtime/preflight?trade=false` — безопасный preflight;
- `POST /api/v1/commands` — только безопасные диагностические transport-команды
  (`FRAME_REQUEST`, `FRAME_REGION_REQUEST`, `WINDOW_STATUS_REQUEST`) и
  `EMERGENCY_STOP`.

Произвольный `ACTION_REQUEST` через HTTP всегда запрещён: действия создают
только Navigator/Trade Executor внутри controller. Disconnect,
heartbeat timeout, duplicate reconnect и `EMERGENCY_STOP` возвращают runtime в
`PAUSED`.

## Калибровка

Без `-screen-config` все экраны направляются в Qwen VLM. После фиксации версии
игры, языка, разрешения и DPI известные экраны следует описать anchors и OCR ROI
в JSON. Один и тот же конфиг передаётся controller и Windows Agent.

Формат и безопасный рабочий процесс:
[`docs/calibration.md`](docs/calibration.md). Пример:
[`configs/screens.example.json`](configs/screens.example.json).

## Проверки

```bash
go test ./...
go vet ./...
go test -tags=e2e ./tests/e2e
GOOS=windows GOARCH=amd64 go test ./...

cd services/ocr
uv sync --extra dev
uv run pytest
```

Транспортный E2E запускает реальные процессы без сетевых заглушек. Игровой
приёмочный прогон требует откалиброванных реальных кадров, тестового бюджета и
50 успешных повторений фиксированного маршрута. Текущие условия и честный
статус: [`docs/e2e-test-plan.md`](docs/e2e-test-plan.md).

# Калибровка экранов и OCR ROI

## Что фиксируется

Detector зависит от реального UI. Для каждого набора калибровки сохраните:

| Поле          | Пример                        |
| ------------- | ----------------------------- |
| Версия игры   | версия клиента на дату записи |
| Язык UI       | русский                       |
| Client area   | `1280x1024`                   |
| DPI           | `100%`                        |
| Режим окна    | windowed / borderless         |
| Состояние     | `MAIN_MENU`                   |
| SHA-256 кадра | хеш исходного файла           |

После обновления игры, смены языка, разрешения или DPI калибровку нужно
перепроверить. Используйте только кадры, которые разрешено хранить.

## Безопасный сбор кадра

1. Запустите controller и Windows Agent без `-allow-input`.
2. Оставьте controller в `PAUSED`.
3. Выведите нужный экран игры на foreground.
4. Запросите observation:

```bash
curl -X POST http://127.0.0.1:8787/api/v1/runtime/observe
```

5. Узнайте `agent_id`:

```bash
curl http://127.0.0.1:8787/api/v1/state
```

6. Сохраните исходный кадр:

```bash
mkdir -p data/calibration/templates
curl -o data/calibration/templates/main-menu.png \
  'http://127.0.0.1:8787/api/v1/agents/windows-local/frame?raw=true'
sha256sum data/calibration/templates/main-menu.png
```

Вызов `/runtime/observe` сохраняет ещё одну аудируемую копию и JSON sidecar в
`data/frames/`.

## Координаты

Все прямоугольники нормализованы относительно client area:

```text
x      = left / frame_width
y      = top / frame_height
width  = roi_width / frame_width
height = roi_height / frame_height
```

Каждое значение должно быть в `0..1`, ширина и высота — больше нуля, правая и
нижняя границы не должны выходить за `1`.

## Detector config

Начните с [`configs/screens.example.json`](../configs/screens.example.json).
Для каждого известного состояния задаются:

- `anchors` — стабильные участки UI для perceptual dHash;
- `max_distance` — допустимое Hamming distance `0..64`;
- `min_confidence` — минимальная общая уверенность;
- `regions` — именованные ROI, отправляемые в OCR.

Пример:

```json
{
  "screens": [
    {
      "state": "MAIN_MENU",
      "min_confidence": 0.9,
      "anchors": [
        {
          "region": {
            "x": 0.02,
            "y": 0.02,
            "width": 0.12,
            "height": 0.08
          },
          "template": "../data/calibration/templates/main-menu.png",
          "max_distance": 6
        }
      ],
      "regions": {
        "balance": {
          "x": 0.78,
          "y": 0.01,
          "width": 0.2,
          "height": 0.06
        }
      }
    }
  ]
}
```

Путь `template` разрешается относительно самого JSON-конфига. Detector берёт
заданный `region` из template и вычисляет hash при старте; вручную вычислять
dHash не обязательно. Вместо `template` можно задать ровно 16 hex-символов в
`hash`, но не оба поля одновременно.

Anchor выбирайте на неизменяемой графике: заголовок вкладки, рамка или
постоянная иконка. Не используйте баланс, цены, таймер, имя игрока, случайный
фон или анимацию.

ROI делайте минимальным прямоугольником вокруг одной величины. Имена ROI
становятся ключами `Observation.Values`, поэтому используйте устойчивые
доменные имена: `balance`, `purchase_price`, `sale_commission`,
`listing_fee`, `ingredient_1_quantity`.

## Запуск с конфигом

Controller получает detector через поле `detector_config` runtime-конфигурации.
Сохраните `screens.local.json` внутри `configs/`, создайте
`configs/runtime.local.json` на основе примера и укажите в нём:

```json
{
  "detector_config": "screens.local.json"
}
```

Остальные обязательные поля `runtime.local.json` сохраните из
`runtime.example.json`. Затем запустите:

```bash
export ARENA_RUNTIME_CONFIG_CONTAINER=/app/configs/runtime.local.json
./scripts/start-wsl-safe.sh
```

PowerShell:

```powershell
powershell -ExecutionPolicy Bypass `
  -File .\scripts\start-windows-agent-observe.ps1 `
  -ScreenConfig "C:\ArenaTradingAgent\screens.local.json"
```

Controller и Windows Agent должны получить detector одинакового содержания.
Файлы могут лежать в разных ОС, но используемые template/hash должны описывать
одну версию калибровки. Для Windows удобнее заменить `template` готовым `hash`,
чтобы агенту не требовались PNG-файлы.

## Проверка

Для каждого состояния выполните не менее нескольких кадров с небольшими
вариациями содержимого:

```bash
curl -X POST http://127.0.0.1:8787/api/v1/runtime/observe
curl http://127.0.0.1:8787/api/v1/runtime
```

Проверьте:

- правильный `state`;
- confidence выше заданного порога;
- ROI не захватывают соседние значения;
- `raw`, `normalized`, `source=OCR` и confidence каждого числа;
- неизвестный или изменившийся экран возвращает `UNKNOWN` и уходит в Qwen VLM;
- одинаковые параметры client area и DPI на всех кадрах.

До стабильного распознавания и нулевого числа денежных OCR-ошибок не
запускайте Windows Agent с `-allow-input` и не переводите controller в
`SCAN`/`TRADE`.

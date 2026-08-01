# Arena OCR Service

Локальный CPU-сервис распознавания заданных ROI. Изображение и координаты
передаются в JSON; наружу сервис не обращается после первоначальной установки
зависимостей и OCR-моделей.

```bash
cd services/ocr
uv sync --extra dev
uv run uvicorn app.main:app --host 127.0.0.1 --port 8788
```

Проверка:

```bash
curl http://127.0.0.1:8788/healthz
uv run pytest
```

После WSL crash сервис можно поднять вместе со всем безопасным стеком из корня
репозитория:

```bash
./scripts/start-wsl-safe.sh
```

Процесс слушает только `127.0.0.1`. ONNX-модель загружается лениво при первом
`POST /v1/ocr`, поэтому первый запрос может занять больше времени. Описание
именованных ROI находится в [`docs/calibration.md`](../../docs/calibration.md).

# Arena Trading Agent

Монорепозиторий production-runtime v1. Система состоит из двух процессов:

- `windows-agent` захватывает изображение и последовательно выполняет ввод;
- `controller` управляет сессией и принимает решения.

## Быстрый запуск

```bash
go run ./cmd/controller -listen :8787
go run ./cmd/windows-agent -controller ws://localhost:8787/ws/agent
```

Контроллер публикует `GET /healthz`, а агент самостоятельно подключается к
`/ws/agent` и поддерживает heartbeat. Экономика и ввод в инфраструктурном
этапе подключаются к системным адаптерам через интерфейсы.

## Модули runtime

- `internal/agent` — безопасный последовательный исполнитель действий и транспорт;
- `internal/observation` — local detector → OCR либо VLM grounding;
- `internal/navigation` — граф экранов и поиск кратчайшего пути;
- `internal/economy` и `internal/risk` — расчёт, scoring и обязательные лимиты;
- `internal/trade` — единственная активная торговая saga и recovery;
- `internal/repository` — контракт хранилища и реализация для симуляции.

Системные адаптеры GDI, SendInput, LM Studio, OCR и постоянная SQL-реализация
подключаются к этим контрактам отдельно. Для калибровки vision необходимы
лицензированные снимки интерфейса целевых версий игры; синтетические изображения
не используются как замена реальному набору данных.

## Проверки

```bash
go test ./...
go test -tags=e2e ./tests/e2e
GOOS=windows GOARCH=amd64 go build ./cmd/windows-agent
```

E2E-тест собирает и запускает реальные процессы `controller` и `windows-agent`,
проверяет подключение и штатное отключение без транспортных заглушек. План и
обязательные условия полного прогона с игрой описаны в
[`docs/e2e-test-plan.md`](docs/e2e-test-plan.md). Наличие этого теста не означает,
что торговые сценарии проверены до подключения WinAPI, vision-сервисов и игры.

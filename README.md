# reddit-bridge

RSS/Atom-бридж для Reddit. Решает JS-челлендж, читает серверный HTML лент и
отдаёт Atom 1.0 или RSS 2.0.

Ошибки upstream, включая 429, не попадают в ридер: кэш с `stale-while-error`
отдаёт последнюю валидную ленту.

## Запуск

```sh
docker compose up -d --build
```

Фид: `GET /feed?r=selfhosted`. Веб-интерфейс: `GET /`.

## Параметры фида

`r` — сабреддиты через запятую; `u` — посты пользователя; `sort`, `t` —
сортировка и окно; `score`, `min_comments` — фильтры; `q`, `f` — поиск и
flair; `limit`; `format=atom|rss`; `frontend` — хост для ссылок (только хосты
из `FRONTEND_ALLOWLIST`).

## Эндпоинты

`/` — конструктор фида; `/feed` — лента; `/healthz` — состояние;
`/metrics` — метрики Prometheus; `/api/detect` — параметры по ссылке Reddit.

## Сборка и тесты

```sh
go build ./...
go test ./...
```

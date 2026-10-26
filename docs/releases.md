# Локальные сборки и релизный процесс

Relay собирается локально, без публикации, установки пакетов и автоматических
обновлений. Все результаты этой поставки **UNSIGNED**: сертификаты не предоставлены,
подписывание, notarization и поиск ключей не выполняются. SHA-256 помогает обнаружить
повреждение, но не подтверждает автора. Ожидаемые версию и checksum необходимо получать
через доверенный канал. Подписанный update feed и запуск update binaries не реализованы.

Версия доступна через `relay --version`: команда печатает JSON с version, commit,
sourceDateEpoch, unsigned, goos, goarch и goVersion до открытия профиля и GUI.
Обычная сборка без linker flags сообщает `dev`, `unknown`, epoch `0` и unsigned `true`.

## Воспроизводимая сборка

Требуются Go из `go.mod`, Node 24.19.0, зависимости из lock-файлов и соответствующий
native toolchain. Linux дополнительно требует GTK3/WebKitGTK 4.1 development packages.
Полный пример из корня репозитория:

```sh
export GOCACHE=/tmp/relay-go-build
export SOURCE_DATE_EPOCH=1792886400
go run ./tools/release --version 0.1.0 \
  --commit "$(git rev-parse HEAD)" --target linux/amd64 \
  --out /tmp/relay-release-linux
go run ./tools/release --verify /tmp/relay-release-linux \
  --version 0.1.0 --commit "$(git rev-parse HEAD)"
```

Output должен быть новым или пустым каталогом: существующие результаты не перезаписываются.
`--skip-frontend` разрешает использовать уже собранный `frontend/dist`; manifest явно
отмечает этот режим. По умолчанию инструмент выполняет `npm ci && npm run build`.
Значения version/commit/epoch проверяются до запуска сборки. Version допускает SemVer
без `v` и `+build`; commit — полный lowercase hex SHA-1/SHA-256; epoch — целое Unix-время
в общем диапазоне ZIP/gzip 1980–2106. Значение epoch должно описывать выбранный исходный snapshot,
а не текущее время каждого запуска.

Команда использует `production` (Linux: `production,gtk3`), `-trimpath`,
`-buildvcs=false`, `-buildid=`, фиксированные linker metadata, UTC, нормализованные
режимы и временные метки файлов. Tar/ZIP имеют стабильный порядок, tar uid/gid равны
нулю; deb создаётся как детерминированный ar с control/data tar.gz. Внутри каждого
пакета лежит `RELEASE.json`; рядом с пакетами — `manifest.json` и `SHA256SUMS`.
Manifest фиксирует toolchain, target, unsigned status и ограничения платформы.
Незакоммиченная рабочая копия явно обозначается: её содержимое нельзя восстановить
только по base commit. Профили, базы данных, секреты и файлы запросов в пакет не входят.

Воспроизводимость означает одинаковые байты при одинаковых исходниках (включая
frontend), Go/Node/C toolchain, зависимостях и системных библиотеках. Равенство пакетов
между различными Linux дистрибутивами или версиями компилятора не обещается.
Для проверки выполните две сборки в разные пустые output-каталоги и сравните
`SHA256SUMS`. Native macOS и Windows runtime подтверждаются только на соответствующих ОС.

## Проверка скачанного пакета

```sh
go run ./tools/release --verify /path/to/downloaded-directory --version 0.1.0
```

Проверяющий код использует только Go standard library и **не запускает бинарники,
скрипты установщика и содержимое архива**. Он сверяет ожидаемую версию, optional commit,
имена файлов, размеры, SHA-256, unsigned label, вложенный `RELEASE.json` и обязательный
inventory. Архивы проверяются без извлечения: traversal, абсолютные/Windows пути,
symlink/hardlink, special files, дубликаты, неизвестные файлы, небезопасные права,
обрезанные/повреждённые потоки и превышение лимитов отвергаются. Проверяются безопасный
`Exec=relay` в desktop entry, версия/архитектура deb и magic исполняемого файла.
Лимиты: артефакт 512 MiB, распакованный архив 1 GiB, metadata 1 MiB.

Успешная проверка неподписанного manifest не является доказательством подлинности:
злоумышленник, способный заменить пакет и manifest, способен заменить и SHA256SUMS.
До появления сертификатов автоматическая установка обновлений не поддерживается.

## Linux: GTK3 portable и deb

Результаты: `relay_<version>_linux_<arch>_unsigned.tar.gz` и `.deb`.
Portable архив содержит каталог `relay/` с executable, `.desktop`, SVG icon,
LICENSE, README и RELEASE metadata. Для запуска распакуйте его и выполните `./relay`.
Runtime библиотеки GTK3 и WebKitGTK 4.1 должны быть установлены в системе; архив их
не включает. На Debian/Ubuntu deb объявляет зависимости `libgtk-3-0` и
`libwebkit2gtk-4.1-0`; система может предложить совместимые t64 replacements.

Deb устанавливает `/usr/bin/relay`, desktop entry, scalable icon и документацию.
Пакет не содержит maintainer scripts, сервисов, задач автозапуска или updater.
Инструмент создаёт пакет, но не вызывает установщик и не требует root.
AppImage/rpm не создаются: в локальном окружении AppImage toolchain отсутствует.
Linux build требует совпадения host OS/architecture с target.

## Windows: portable ZIP

```sh
go run ./tools/release --version 0.1.0 --commit "$(git rev-parse HEAD)" \
  --epoch 1792886400 --target windows/amd64 --out /tmp/relay-release-windows
```

Windows binary собирается с `CGO_ENABLED=0`; cross-build доступен с Linux.
ZIP содержит `relay.exe`, LICENSE, README и RELEASE metadata. Для запуска необходим
Microsoft WebView2 Runtime. Installer/MSI и Authenticode signature отсутствуют;
cross-build и архивная проверка не заменяют запуск GUI на Windows.

## macOS: unsigned .app

На native macOS runner используйте `--target darwin/arm64` или `darwin/amd64`
с архитектурой хоста. Результат — tar.gz с `Relay.app`, Info.plist, executable,
RELEASE metadata и лицензией. Native WebKit и CGO используются при сборке.
Из Linux macOS пакет не создаётся. DMG, signing и notarization не выполняются.
Gatekeeper может заблокировать неподписанную сборку; публично подтверждённой
установки на macOS в текущем Linux окружении нет.

## CI и дальнейшая публикация

`.github/workflows/release.yml` запускается только вручную через workflow_dispatch.
Матрица строит Linux amd64, Windows amd64 и macOS с архитектурой native runner, проверяет
локальные пакеты и сохраняет только workflow artifacts на 14 дней. Workflow не создаёт
GitHub Release, tag, deployment, не использует signing secrets и не публикует приложение.
Удалённый запуск этой матрицы не выполнялся в локальном окружении.

Перед публичным релизом нужны platform runtime smoke-tests, независимая проверка
зависимостей и reproducibility на зафиксированном runner image. Для signed builds
потребуется отдельный согласованный процесс с сертификатами и проверкой signatures.

## Локальная проверка 2026-10-25

На Linux amd64 выполнены по два запуска для Linux и Windows с version `0.1.0`,
epoch `1792886400` и base commit `36edb8201328a4eb74309ec61b18b5c64db4fece`.
Рабочая копия содержит незакоммиченные файлы и обозначена в manifest как
`working tree not authenticated by the base commit`. Использовались одинаковые
production assets `index-BKnqUu6i.js` / `index-BWa3vdwS.css` через `--skip-frontend`;
Go 1.26.10, Node 24.19.0, npm 11.17.0, GCC 15.2.0, GTK 3.24.52, WebKitGTK 2.52.6.
Go build cache была общей; output и временные build-каталоги — разными.

Все артефакты, `manifest.json` и `SHA256SUMS` двух запусков совпали побайтово.
Повторный `npm run build` также дал побайтово одинаковые три frontend asset-файла.
Проверенные пакеты скопированы в игнорируемый Git каталог
[`dist/release/0.1.0/linux-amd64`](../dist/release/0.1.0/linux-amd64) и
[`dist/release/0.1.0/windows-amd64`](../dist/release/0.1.0/windows-amd64).
Для обеих копий повторно прошёл `go run ./tools/release --verify`.
Временные результаты сравнения находятся в
`/tmp/relay-release-stage8/{linux-a,linux-b,windows-a,windows-b}`:

| Артефакт | Размер, байт | SHA-256 |
| --- | ---: | --- |
| Linux portable tar.gz | 10327104 | `fa9c8be938363304edce1768d5004cfb08dc1b2c428973946ca8674c5c41a466` |
| Linux deb | 10328494 | `2bcc952e1d06703113ec8fb917915ccead7cf4f7ed4100b40a7a55cd16363c68` |
| Windows portable ZIP | 10456518 | `5ad08de273b126f7763f9ece6a16ae097d34607cd0517a40148b0f3d6b381ceb` |

`dpkg-deb --info`, `--contents` и извлечение в отдельный временный каталог прошли;
бинарник deb совпал с portable tar. Пакет не устанавливался в систему.
Собственный собранный Linux binary выполнил `--version` без DISPLAY/WAYLAND и
создания профиля, сообщил Go 1.26.10 и корректные release metadata.
Встроенные build settings содержат production/gtk3, trimpath и не содержат VCS
timestamp или абсолютных путей исходников/временного build-каталога.

Native GTK3 smoke прошёл на executable, извлечённом именно из проверенного
production portable tar: реальное WebSocket соединение и echo сообщения,
версия `0.1.0` и unsigned label в Settings. Автоматизация работала с AT-SPI
поддеревом собственного PID приложения. Результат сохранён в
`/tmp/relay-stage8-native-result.json`; запуск чужих приложений и установка deb
не выполнялись. Native Windows/macOS GUI и удалённая CI-матрица остаются
непроверенными в этом Linux окружении.

Восемь Go security-тестов release validator прошли с `-race`, `go vet` прошёл;
path fuzzing за 3 секунды проверил 79435 случаев. Проверены подмена checksum,
размера/версии/вложенной metadata, traversal и расхождение ZIP local/central имён,
дубликаты, links/special files/setuid, UID/GID, лишние и обрезанные данные,
corrupt deb headers, JSON key duplication и лимиты. macOS packaging проверено
на fixture inventory; native macOS сборка и запуск в этом окружении не выполнялись.

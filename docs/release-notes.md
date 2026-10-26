# Relay 0.1.0 — локальный release candidate

**UNSIGNED.** Эта версия подготовлена для локальной проверки. Публичный релиз,
подпись, notarization и автоматические обновления не выполнялись.

Поддерживаются HTTP/1.0, HTTP/1.1, HTTP/2 с TLS и h2c, HTTP/3 с JSON/Protobuf, gRPC unary и streams,
WebSocket `ws/wss` через HTTP/1.1 Upgrade, окружения, OS keyring, история и проектный
импорт/экспорт. Native SQLite profile отделён от пакетов приложения.

Доступны воспроизводимый Linux GTK3 portable tar.gz и deb, Windows portable ZIP;
для macOS добавлены native .app packaging и ручная CI-матрица. Все пакеты содержат
version/commit/epoch metadata, manifest и SHA256SUMS. Проверка архивов не запускает
их содержимое. `relay --version` работает до открытия профиля и GUI.
Упакованный production Linux binary прошёл native GTK3 smoke с WebSocket echo,
отображением версии `0.1.0` и unsigned label в Settings.

Ограничения: Linux требует системных GTK3/WebKitGTK 4.1, Windows — WebView2.
Native Windows/macOS runtime не проверен из Linux; macOS .app требует native runner.
AppImage, rpm, MSI, DMG и signed updater отсутствуют. WS over HTTP/2/3, WebSocket
autoreconnect и gRPC-Web не входят в первую поставку. Сохранённая история сообщений
содержит ограниченные previews; payloads сохраняются по явному выбору.

Подробные команды, проверка целостности, угрозы неподписанных checksums и границы
воспроизводимости описаны в [release process](releases.md).

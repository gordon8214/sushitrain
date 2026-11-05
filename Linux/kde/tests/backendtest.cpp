// Copyright (C) 2026 Gordon. SPDX-License-Identifier: MPL-2.0
#include "../src/backend.h"
#include "../src/previewwindow.h"
#include <QApplication>
#include <QDesktopServices>
#include <QDir>
#include <QFile>
#include <QJsonDocument>
#include <QLocalServer>
#include <QLocalSocket>
#include <QPointer>
#include <QScopeGuard>
#include <QScopedPointer>
#include <QQmlContext>
#include <QQmlEngine>
#include <QTemporaryDir>
#include <QTest>
#include <QTimer>
#include <sys/file.h>

class Fixture {
public:
  QTemporaryDir dir;
  QLocalServer server;
  QByteArray oldPath = qgetenv("PATH");
  QStringList calls;
  QList<QJsonObject> parameters;
  QString mode = "synchronize";
  std::function<QJsonObject(QString, QJsonObject)> handler;
  std::function<int(QString, QJsonObject)> delay;
  explicit Fixture(const QString &mode = "synchronize") : mode(mode) {
    server.listen(dir.path() + "/admin");
    QFile helper(dir.path() + "/synctrain-desktop");
    helper.open(QIODevice::WriteOnly);
    QJsonObject host{
        {"Mode", mode},
        {"FilesSocket", server.fullServerName()},
        {"Check",
         QJsonObject{
             {"Home", QJsonObject{{"socket", server.fullServerName()}}}}}};
    helper.write("#!/bin/sh\nprintf '%s\\n' '");
    helper.write(QJsonDocument(QJsonObject{{"result", host}})
                     .toJson(QJsonDocument::Compact));
    helper.write("'\n");
    helper.close();
    helper.setPermissions(QFile::ReadOwner | QFile::WriteOwner |
                          QFile::ExeOwner);
    qputenv("PATH", dir.path().toUtf8() + ":" + oldPath);
    QObject::connect(&server, &QLocalServer::newConnection, &server, [this] {
      auto *socket = server.nextPendingConnection();
      auto buffer = std::make_shared<QByteArray>();
      QObject::connect(
          socket, &QLocalSocket::readyRead, socket, [this, socket, buffer] {
            buffer->append(socket->readAll());
            if (!buffer->contains('\n'))
              return;
            auto request = QJsonDocument::fromJson(*buffer).object();
            auto method = request["method"].toString();
            auto params = request["params"].toObject();
            calls << method;
            parameters << params;
            auto response = handler ? handler(method, params) : QJsonObject{};
            const auto data = QJsonDocument(response).toJson(QJsonDocument::Compact) + '\n';
            QTimer::singleShot(delay ? delay(method, params) : 0, socket,
                               [socket, data] { socket->write(data); });
          });
    });
  }
  ~Fixture() { qputenv("PATH", oldPath); }
};
class UrlRecorder : public QObject {
  Q_OBJECT
public:
  QList<QUrl> urls;
public slots:
  void open(const QUrl &url) { urls << url; }
};
static QJsonObject result(QJsonValue value) { return {{"result", value}}; }
static QString key(const QVariantList &actions, const QString &label) {
  for (const auto &action : actions) {
    auto o = action.toMap();
    if (o["label"].toString() == label)
      return o["key"].toString();
  }
  return {};
}
static QString rowAction(Backend &backend, const QString &title,
                         const QString &label) {
  for (const auto &value : backend.rows()) {
    auto row = value.toMap();
    if (row["title"] == title)
      return key(row["actions"].toList(), label);
  }
  return {};
}
class BackendTest : public QObject {
  Q_OBJECT
private slots:
  void qmlCancelPreservesDirtyDraft() {
    Fixture f;
    f.handler = [](QString m, auto) {
      if (m == "settings.get")
        return result(QJsonObject{{"Token", "editor"}, {"Fields", QJsonArray{}}});
      if (m == "settings.save")
        return QJsonObject{{"error", "Invalid settings"}};
      return result(QJsonObject{});
    };
    Backend b;
    QQmlEngine engine;
    engine.rootContext()->setContextProperty("backend", &b);
    QQmlComponent component(&engine, QUrl("qrc:/test-ui/qml/Main.qml"));
    QScopedPointer<QObject> window(component.create());
    QVERIFY2(window, qPrintable(component.errorString()));
    QTRY_COMPARE_WITH_TIMEOUT(b.title(), QString("Overview"), 1000);
    b.navigate("Settings");
    b.activate(rowAction(b, "Network", "Edit"));
    QTRY_VERIFY_WITH_TIMEOUT(!b.dialog().isEmpty(), 1000);
    auto *editor = window->findChild<QObject *>("settingsEditor");
    auto *cancel = window->findChild<QObject *>("editorCancel");
    auto *save = window->findChild<QObject *>("editorSave");
    QVERIFY(editor && cancel && save);
    QTRY_VERIFY_WITH_TIMEOUT(editor->property("opened").toBool(), 1000);
    editor->setProperty("draft", QVariantMap{{"name", "unsaved"}});
    b.fieldChanged();
    QVERIFY(QMetaObject::invokeMethod(cancel, "clicked"));
    QCoreApplication::processEvents();
    QVERIFY(b.dialog()["discard"].toBool());
    QVERIFY(editor->property("opened").toBool());
    QCOMPARE(editor->property("draft").toMap()["name"].toString(), QString("unsaved"));
    // An invalid save must keep the same visible draft as well.
    QVERIFY(QMetaObject::invokeMethod(save, "clicked"));
    QTRY_COMPARE_WITH_TIMEOUT(b.error(), QString("Invalid settings"), 1000);
    QVERIFY(editor->property("opened").toBool());
    QCOMPARE(editor->property("draft").toMap()["name"].toString(), QString("unsaved"));
    QVERIFY(QMetaObject::invokeMethod(cancel, "clicked"));
    QTRY_VERIFY_WITH_TIMEOUT(b.dialog().isEmpty(), 1000);
  }
  void coldActivationWaitsForPreferences() {
    Fixture f;
    f.delay = [](QString method, auto) { return method == "mcp.preferences.get" ? 80 : 0; };
    f.handler = [](QString method, auto) {
      if (method == "mcp.preferences.get")
        return result(QJsonObject{{"Token", "app"}, {"Fields", QJsonArray{
            QJsonObject{{"Path", "Grid"}, {"Value", "true"}}}}});
      return result(QJsonArray{});
    };
    Backend b;
    b.arguments({"synctrain-kde", "status", "/sync/a"});
    QTRY_COMPARE_WITH_TIMEOUT(b.title(), QString("Sync Status"), 1000);
    QVERIFY(b.gridView());
    QVERIFY(f.calls.contains("mcp.preferences.close"));
    QCOMPARE(f.calls.count("desktop.items"), 1);
  }
  void settingsLoadsSerializeAndRestartPromptUsesWireKey() {
    Fixture f;
    f.delay = [](QString m, auto) { return m == "settings.get" ? 30 : 0; };
    f.handler = [](QString m, QJsonObject p) {
      if (m == "settings.get")
        return result(QJsonObject{{"Token", p["Section"]}, {"Fields", QJsonArray{}}});
      if (m == "settings.save")
        return result(QJsonObject{{"restartRequired", true}});
      return result(QJsonObject{});
    };
    Backend b;
    QTRY_COMPARE_WITH_TIMEOUT(b.title(), QString("Overview"), 1000);
    b.navigate("Settings");
    b.activate(rowAction(b, "Network", "Edit"));
    b.activate(rowAction(b, "Files & Previews", "Configure"));
    QTRY_VERIFY_WITH_TIMEOUT(!b.dialog().isEmpty(), 1000);
    QCOMPARE(f.calls.count("settings.get"), 1);
    QCOMPARE(f.calls.count("mcp.preferences.get"), 1); // Startup only.
    b.submit({});
    QTRY_COMPARE_WITH_TIMEOUT(b.dialog()["title"].toString(), QString("Restart Required"), 1000);
    const auto i = f.calls.indexOf("settings.save");
    QVERIFY(i >= 0);
    QCOMPARE(f.parameters[i]["Token"].toString(), QString("basic/options"));
  }
  void abandonedSettingsLoadClosesItsToken() {
    Fixture f;
    f.delay = [](QString m, auto) { return m == "settings.get" ? 30 : 0; };
    f.handler = [](QString m, auto) {
      if (m == "settings.get")
        return result(QJsonObject{{"Token", "abandoned"}, {"Fields", QJsonArray{}}});
      return result(QJsonObject{});
    };
    Backend b;
    QTRY_COMPARE_WITH_TIMEOUT(b.title(), QString("Overview"), 1000);
    b.navigate("Settings");
    b.activate(rowAction(b, "Network", "Edit"));
    b.navigate("Devices");
    QTRY_VERIFY_WITH_TIMEOUT(f.calls.contains("settings.close"), 1000);
    const auto i = f.calls.indexOf("settings.close");
    QCOMPARE(f.parameters[i]["Token"].toString(), QString("abandoned"));
    QVERIFY(b.dialog().isEmpty());
  }
  void remoteDefaultAppMaterializesAndDrainsActivation() {
    Fixture f;
    Backend b;
    QTRY_COMPARE_WITH_TIMEOUT(b.title(), QString("Overview"), 1000);
    // The child remains asynchronous even when this fixture replies immediately.
    QFile helper(f.dir.path() + "/synctrain-desktop");
    QVERIFY(helper.open(QIODevice::WriteOnly));
    helper.write("#!/bin/sh\nread -r request\nprintf '%s' \"$request\" > '");
    helper.write((f.dir.path() + "/request").toUtf8());
    helper.write("'\nprintf '%s\\n' '{\"result\":\"/tmp/materialized.docx\"}'\n");
    helper.close();
    UrlRecorder recorder;
    QDesktopServices::setUrlHandler("file", &recorder, "open");
    const auto cleanup = qScopeGuard([] { QDesktopServices::unsetUrlHandler("file"); });
    b.openDefault("http://127.0.0.1:20920/signed", "document.docx");
    QVERIFY(b.busy());
    b.arguments({"synctrain-kde", "status", "/sync/a"});
    QTRY_COMPARE_WITH_TIMEOUT(recorder.urls.size(), 1, 1000);
    QCOMPARE(recorder.urls[0], QUrl::fromLocalFile("/tmp/materialized.docx"));
    QTRY_COMPARE_WITH_TIMEOUT(b.title(), QString("Sync Status"), 1000);
    QFile request(f.dir.path() + "/request");
    QVERIFY(request.open(QIODevice::ReadOnly));
    const auto params = QJsonDocument::fromJson(request.readAll()).object()["params"].toObject();
    QVERIFY(params["External"].toBool());
    QCOMPARE(params["Name"].toString(), QString("document.docx"));
  }
  void mutationFailureDrainsActivation() {
    Fixture f;
    f.handler = [](QString m, auto) {
      return m == "engine.action" ? QJsonObject{{"error", "Action failed"}} : result(QJsonObject{});
    };
    Backend b;
    QTRY_COMPARE_WITH_TIMEOUT(b.title(), QString("Overview"), 1000);
    b.activate(key(b.commands(), "Pause All"));
    QVERIFY(b.busy());
    b.arguments({"synctrain-kde", "status", "/sync/a"});
    QTRY_COMPARE_WITH_TIMEOUT(b.title(), QString("Sync Status"), 1000);
  }
  void previewClosesAndReleasesWindow() {
    Fixture f;
    Backend b;
    QQmlEngine engine;
    engine.rootContext()->setContextProperty("backend", &b);
    QPointer<QQuickWindow> window = createPreviewWindow(
        &engine, QUrl("qrc:/test-ui/qml/Preview.qml"),
        {{"file", ""}, {"kind", "text"}, {"text", "Preview contents"},
         {"location", "/tmp/document.txt"}, {"name", "document.txt"}});
    QVERIFY(window);
    window->close();
    QTRY_VERIFY_WITH_TIMEOUT(window.isNull(), 1000);
  }
  void activePreviewHoldsCacheLeaseUntilClose() {
    Fixture f;
    const auto oldCache = qgetenv("XDG_CACHE_HOME");
    qputenv("XDG_CACHE_HOME", f.dir.path().toUtf8());
    const auto restore = qScopeGuard([oldCache] {
      if (oldCache.isNull()) qunsetenv("XDG_CACHE_HOME");
      else qputenv("XDG_CACHE_HOME", oldCache);
    });
    const auto directory = f.dir.path() + "/synctrain/desktop-previews/preview-open";
    QVERIFY(QDir().mkpath(directory));
    QFile file(directory + "/document.txt");
    QVERIFY(file.open(QIODevice::WriteOnly));
    file.write("Preview contents");
    file.close();
    Backend b;
    QQmlEngine engine;
    engine.rootContext()->setContextProperty("backend", &b);
    QPointer<QQuickWindow> window = createPreviewWindow(
        &engine, QUrl("qrc:/test-ui/qml/Preview.qml"),
        {{"file", QUrl::fromLocalFile(file.fileName()).toString()}, {"kind", "text"},
         {"text", "Preview contents"}, {"location", file.fileName()},
         {"name", "document.txt"}});
    QVERIFY(window);
    QFile lease(directory + "/.in-use");
    QVERIFY(lease.open(QIODevice::ReadWrite));
    QVERIFY(flock(lease.handle(), LOCK_EX | LOCK_NB) != 0);
    window->close();
    QTRY_VERIFY_WITH_TIMEOUT(window.isNull(), 1000);
    QCOMPARE(flock(lease.handle(), LOCK_EX | LOCK_NB), 0);
  }
  void coldPairingIsQueued() {
    Fixture f;
    f.handler = [](QString m, auto) {
      if (m == "mcp.preferences.get")
        return result(QJsonObject{{"Token", "pref"}, {"Fields", QJsonArray{}}});
      return result(QJsonObject{});
    };
    Backend b;
    b.arguments({"synctrain-kde",
                 "synctrain+pair://example.test:20920/?secret=fixture"});
    QTRY_VERIFY_WITH_TIMEOUT(f.calls.contains("history.pair"), 2000);
  }
  void statusRouteSurvivesRefreshTimer() {
    Fixture f;
    f.handler = [](QString m, auto) {
      if (m == "desktop.items")
        return result(
            QJsonArray{QJsonObject{{"Path", "/sync/a"}, {"State", "synced"}}});
      return result(QJsonObject{});
    };
    Backend b;
    QTRY_COMPARE_WITH_TIMEOUT(b.mode(), QString("synchronize"), 2000);
    b.arguments({"synctrain-kde", "status", "/sync/a"});
    QTRY_COMPARE_WITH_TIMEOUT(b.title(), QString("Sync Status"), 1000);
    QTest::qWait(2200);
    QCOMPARE(b.title(), QString("Sync Status"));
  }
  void invalidSettingsKeepDraft() {
    Fixture f;
    f.handler = [](QString m, auto) {
      if (m == "settings.get")
        return result(QJsonObject{
            {"Token", "editor"},
            {"Fields", QJsonArray{QJsonObject{{"Path", "name"},
                                              {"Label", "Name"},
                                              {"Kind", "string"},
                                              {"Value", "original"}}}}});
      if (m == "settings.save")
        return QJsonObject{
            {"error", "Configuration changed; refresh before saving"}};
      return result(QJsonObject{});
    };
    Backend b;
    QTRY_COMPARE_WITH_TIMEOUT(b.mode(), QString("synchronize"), 2000);
    b.navigate("Settings");
    QTRY_VERIFY_WITH_TIMEOUT(!rowAction(b, "Network", "Edit").isEmpty(), 1000);
    b.activate(rowAction(b, "Network", "Edit"));
    QTRY_VERIFY_WITH_TIMEOUT(!b.dialog().isEmpty(), 1000);
    b.fieldChanged();
    b.submit({{"name", "draft"}});
    QTRY_VERIFY_WITH_TIMEOUT(b.error().contains("Configuration changed"), 1000);
    QVERIFY(!b.dialog().isEmpty());
    QVERIFY(b.dirty());
    b.cancelDialog();
    QVERIFY(!b.dialog().isEmpty());
    b.cancelDialog();
    QVERIFY(b.dialog().isEmpty());
  }
  void permissionFailurePreservesDraftAndMapsNone() {
    Fixture f;
    f.handler = [](QString m, QJsonObject) {
      if (m == "service.status")
        return result(QJsonObject{{"enabled", true}});
      if (m == "clients.list")
        return result(QJsonArray{
            QJsonObject{{"name", "Test Client"}, {"clientID", "client"}}});
      if (m == "exports.list")
        return result(QJsonArray{
            QJsonObject{{"folderID", "folder"}, {"allowWrite", true}}});
      if (m == "clients.permission")
        return QJsonObject{{"error", "Permission update failed"}};
      return result(QJsonObject{});
    };
    Backend b;
    QTRY_COMPARE_WITH_TIMEOUT(b.mode(), QString("synchronize"), 2000);
    b.navigate("Remote Files");
    QTRY_VERIFY_WITH_TIMEOUT(
        !rowAction(b, "Test Client", "Permissions").isEmpty(), 1000);
    b.activate(rowAction(b, "Test Client", "Permissions"));
    QTRY_VERIFY_WITH_TIMEOUT(!b.dialog().isEmpty(), 1000);
    b.fieldChanged();
    b.submit({{"folder", "none"}});
    QTRY_VERIFY_WITH_TIMEOUT(b.error().contains("Permission update failed"),
                             1000);
    const int i = f.calls.indexOf("clients.permission");
    QVERIFY(i >= 0);
    QCOMPARE(f.parameters[i]["access"].toString(), QString());
    QVERIFY(b.dirty());
    QVERIFY(!b.dialog().isEmpty());
  }
  void pairingCanBeCanceled() {
    Fixture f("files");
    Backend b;
    QTRY_COMPARE_WITH_TIMEOUT(b.mode(), QString("files"), 2000);
    b.navigate("Folders");
    QTRY_VERIFY_WITH_TIMEOUT(!key(b.commands(), "Pair…").isEmpty(), 1000);
    b.activate(key(b.commands(), "Pair…"));
    b.submit({{"Link", "synctrain+pair://fixture"}});
    QVERIFY(b.cancelable());
    b.cancelDialog();
    QVERIFY(!b.busy());
    QVERIFY(b.dialog().isEmpty());
    QTRY_VERIFY_WITH_TIMEOUT(f.calls.contains("pair.cancel"), 1000);
    const auto i = f.calls.indexOf("pair.cancel");
    QVERIFY(!f.parameters[i]["ID"].toString().isEmpty());
  }
  void paginationReplacesItsAction() {
    Fixture f("files");
    f.handler = [](QString m, QJsonObject p) {
      if (m == "files.childrenPage") {
        const bool second = p["Offset"].toInt() == 256;
        return result(QJsonObject{
            {"Entries",
             QJsonArray{QJsonObject{{"Name", second ? "b.txt" : "a.txt"},
                                    {"Item", second ? 2 : 1},
                                    {"Node", qint64(9007199254740993LL)},
                                    {"Kind", 0},
                                    {"Size", 1}}}},
            {"Next", second ? 0 : 256}});
      }
      if (m == "files.thumbnail")
        return QJsonObject{{"error", "No thumbnail"}};
      return result(QJsonObject{});
    };
    Backend b;
    QTRY_COMPARE_WITH_TIMEOUT(b.mode(), QString("files"), 2000);
    b.navigate("Files");
    QTRY_VERIFY_WITH_TIMEOUT(!key(b.commands(), "Load More").isEmpty(), 1000);
    const auto old = key(b.commands(), "Load More");
    b.activate(old);
    QTRY_VERIFY_WITH_TIMEOUT(key(b.commands(), "Load More").isEmpty(), 1000);
    QTRY_COMPARE_WITH_TIMEOUT(b.rows().size(), 2, 1000);
    b.activate(old);
    QTest::qWait(30);
    QCOMPARE(f.calls.count("files.childrenPage"), 2);
  }
};
QTEST_MAIN(BackendTest)
#include "backendtest.moc"

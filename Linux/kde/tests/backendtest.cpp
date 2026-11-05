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
#include <QQmlListReference>
#include <QQuickItem>
#include <QQuickWindow>
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
static QQuickItem *visualItem(QQuickItem *root, const QString &name) {
  if (root->objectName() == name)
    return root;
  for (auto *child : root->childItems()) {
    if (auto *found = visualItem(child, name))
      return found;
  }
  return nullptr;
}
class BackendTest : public QObject {
  Q_OBJECT
private slots:
  void qmlDialogLayout_data() {
    QTest::addColumn<bool>("longForm");
    QTest::addColumn<bool>("noticeOnly");
    QTest::addColumn<QSize>("windowSize");
    QTest::newRow("mixed-fields") << false << false << QSize(1100, 780);
    QTest::newRow("confirmation") << false << true << QSize(1100, 780);
    QTest::newRow("scrolling") << true << false << QSize(1100, 780);
    QTest::newRow("compact-window") << true << false << QSize(640, 480);
  }
  void qmlDialogLayout() {
    QFETCH(bool, longForm);
    QFETCH(bool, noticeOnly);
    QFETCH(QSize, windowSize);
    QJsonArray fields{QJsonObject{{"Path", "notice"}, {"Kind", "notice"},
        {"Label", "Approve this machine on the gateway. Cancel stops waiting."}}};
    if (!noticeOnly) {
      fields.append(QJsonObject{{"Path", "Enabled"}, {"Kind", "bool"},
          {"Label", "Enable image previews"}, {"Value", true}});
      fields.append(QJsonObject{{"Path", "Name"}, {"Kind", "string"},
          {"Label", "Device name"}, {"Value", "Fedora KDE"}});
      fields.append(QJsonObject{{"Path", "Mode"}, {"Kind", "enum"},
          {"Label", "Mode"}, {"Value", "normal"},
          {"Options", QJsonArray{"normal", "background"}}});
      fields.append(QJsonObject{{"Path", "Secret"}, {"Kind", "password"},
          {"Label", "Password"}, {"IsSecret", true}, {"Value", "hidden"}});
      fields.append(QJsonObject{{"Path", "Rules"}, {"Kind", "list"},
          {"Label", "Ignore patterns"}, {"Value", "*.tmp\n*.bak"}});
    }
    if (longForm) {
      for (int i = 0; i < 40; ++i)
        fields.append(QJsonObject{{"Path", QString("Option%1").arg(i)},
            {"Kind", "string"}, {"Label", QString("Option %1").arg(i)},
            {"Value", "value"}});
    }
    Fixture f;
    f.handler = [fields](QString m, auto) {
      if (m == "settings.get")
        return result(QJsonObject{{"Token", "editor"}, {"Fields", fields}});
      return result(QJsonObject{});
    };
    Backend b;
    QQmlEngine engine;
    engine.rootContext()->setContextProperty("backend", &b);
    QQmlComponent component(&engine, QUrl("qrc:/test-ui/qml/Main.qml"));
    QScopedPointer<QObject> window(component.create());
    QVERIFY2(window, qPrintable(component.errorString()));
    auto *quickWindow = qobject_cast<QQuickWindow *>(window.data());
    QVERIFY(quickWindow);
    quickWindow->resize(windowSize);
    quickWindow->show();
    QVERIFY(QTest::qWaitForWindowExposed(quickWindow, 1000));
    QTRY_COMPARE_WITH_TIMEOUT(b.title(), QString("Overview"), 1000);
    b.navigate("Settings");
    b.activate(rowAction(b, "Network", "Edit"));
    auto *editor = window->findChild<QObject *>("settingsEditor");
    QVERIFY(editor);
    QTRY_VERIFY_WITH_TIMEOUT(editor->property("opened").toBool(), 1000);
    auto *form = window->findChild<QQuickItem *>("editorForm");
    auto *content = qvariant_cast<QQuickItem *>(editor->property("contentItem"));
    QVERIFY(form && content);
    const qreal padding = editor->property("padding").toReal();
    QVERIFY(padding > 0);
    const auto formOrigin = form->mapToScene(QPointF());
    QVERIFY(formOrigin.x() >= editor->property("x").toReal() + padding);
    QVERIFY(formOrigin.x() + form->width() <=
            editor->property("x").toReal() + editor->property("width").toReal() - padding);
    QVERIFY(editor->property("y").toReal() >= 0);
    QVERIFY(editor->property("height").toReal() <= quickWindow->height());
    if (!noticeOnly) {
      auto *loader = visualItem(form, "editorField_Enabled");
      QVERIFY(loader);
      auto *checkBox = qvariant_cast<QObject *>(loader->property("item"));
      QVERIFY(checkBox);
      QCOMPARE(checkBox->property("text").toString(), QString("Enable image previews"));
      QVERIFY(checkBox->property("checked").toBool());
      auto *secret = visualItem(form, "editorField_Secret");
      QVERIFY(secret);
      auto *password = qvariant_cast<QObject *>(secret->property("item"));
      QVERIFY(password);
      QVERIFY(password->property("text").toString().isEmpty());
    }
    if (longForm) {
      QVERIFY(content->property("contentHeight").toReal() > content->height());
      auto *flickable = qvariant_cast<QObject *>(content->property("contentItem"));
      QVERIFY(flickable);
      const qreal end = flickable->property("contentHeight").toReal() - flickable->property("height").toReal();
      QVERIFY(flickable->setProperty("contentY", end));
      auto *last = visualItem(form, "editorField_Option39");
      QVERIFY(last);
      QVERIFY(last->mapToItem(content, QPointF()).y() + last->height() <= content->height() + 1);
    }
    b.cancelDialog();
    QTRY_VERIFY_WITH_TIMEOUT(b.dialog().isEmpty(), 1000);
  }
  void qmlAdvancedPreservesDraft() {
    Fixture f;
    bool rejectSave = true;
    f.handler = [&rejectSave](QString m, auto) {
      if (m == "settings.get")
        return result(QJsonObject{{"Token", "editor"}, {"Fields", QJsonArray{
            QJsonObject{{"Path", "MinDiskFree"}, {"Kind", "string"},
                {"Label", "Minimum free disk space"}, {"Value", "1"}},
            QJsonObject{{"Path", "Copiers"}, {"Kind", "string"},
                {"Label", "Copiers"}, {"Value", "7"}, {"IsAdvanced", true}}}}});
      if (m == "settings.save" && rejectSave)
        return QJsonObject{{"error", "Copiers: enter a whole number"}};
      return result(QJsonObject{});
    };
    Backend b;
    QQmlEngine engine;
    engine.rootContext()->setContextProperty("backend", &b);
    QQmlComponent component(&engine, QUrl("qrc:/test-ui/qml/Main.qml"));
    QScopedPointer<QObject> window(component.create());
    QVERIFY2(window, qPrintable(component.errorString()));
    auto *quickWindow = qobject_cast<QQuickWindow *>(window.data());
    QVERIFY(quickWindow);
    quickWindow->show();
    QVERIFY(QTest::qWaitForWindowExposed(quickWindow, 1000));
    QTRY_COMPARE_WITH_TIMEOUT(b.title(), QString("Overview"), 1000);
    b.navigate("Settings");
    auto open = [&] { b.activate(rowAction(b, "Network", "Edit")); };
    open();
    auto *editor = window->findChild<QObject *>("settingsEditor");
    auto *disclosure = window->findChild<QQuickItem *>("editorAdvanced");
    auto *advancedForm = window->findChild<QQuickItem *>("editorAdvancedForm");
    QVERIFY(editor && disclosure && advancedForm);
    QTRY_VERIFY_WITH_TIMEOUT(editor->property("opened").toBool(), 1000);
    QVERIFY(!disclosure->property("checked").toBool());
    QVERIFY(!advancedForm->isVisible());
    auto toggle = [&] {
      disclosure->forceActiveFocus();
      QTest::keyClick(quickWindow, Qt::Key_Space);
    };
    toggle();
    QTRY_VERIFY_WITH_TIMEOUT(advancedForm->isVisible(), 1000);
    // Expansion alone must not trigger the discard-changes prompt.
    b.cancelDialog();
    QTRY_VERIFY_WITH_TIMEOUT(b.dialog().isEmpty(), 1000);
    open();
    QTRY_VERIFY_WITH_TIMEOUT(editor->property("opened").toBool(), 1000);
    QVERIFY(!disclosure->property("checked").toBool());
    toggle();
    QTRY_VERIFY_WITH_TIMEOUT(advancedForm->isVisible(), 1000);
    auto *loader = visualItem(advancedForm, "editorField_Copiers");
    QVERIFY(loader);
    auto *input = qvariant_cast<QQuickItem *>(loader->property("item"));
    QVERIFY(input);
    input->forceActiveFocus();
    QTest::keyClick(quickWindow, Qt::Key_A, Qt::ControlModifier);
    for (const auto c : QString("invalid"))
      QTest::keyClick(quickWindow, c.toLatin1());
    QCOMPARE(editor->property("draft").toMap()["Copiers"].toString(), QString("invalid"));
    toggle();
    QVERIFY(!advancedForm->isVisible());
    toggle();
    QVERIFY(advancedForm->isVisible());
    QCOMPARE(input->property("text").toString(), QString("invalid"));
    toggle();
    const auto reads = f.calls.count("settings.get");
    auto *save = window->findChild<QObject *>("editorSave");
    QVERIFY(save);
    QVERIFY(QMetaObject::invokeMethod(save, "clicked"));
    QTRY_COMPARE_WITH_TIMEOUT(b.error(), QString("Copiers: enter a whole number"), 1000);
    QTRY_VERIFY_WITH_TIMEOUT(advancedForm->isVisible(), 1000);
    QCOMPARE(input->property("text").toString(), QString("invalid"));
    QCOMPARE(f.calls.count("settings.get"), reads);
    // A retained error must not override the user's later collapse choice.
    toggle();
    QVERIFY(!advancedForm->isVisible());
    auto *basicForm = window->findChild<QQuickItem *>("editorForm");
    QVERIFY(basicForm);
    auto *basicLoader = visualItem(basicForm, "editorField_MinDiskFree");
    QVERIFY(basicLoader);
    auto *basicInput = qvariant_cast<QQuickItem *>(basicLoader->property("item"));
    QVERIFY(basicInput);
    basicInput->forceActiveFocus();
    QTest::keyClick(quickWindow, Qt::Key_A, Qt::ControlModifier);
    QTest::keyClick(quickWindow, '2');
    QCOMPARE(editor->property("draft").toMap()["MinDiskFree"].toString(), QString("2"));
    QVERIFY(!advancedForm->isVisible());
    // Another failed submission with the same error must reveal Advanced again.
    QVERIFY(QMetaObject::invokeMethod(save, "clicked"));
    QTRY_COMPARE_WITH_TIMEOUT(f.calls.count("settings.save"), 2, 1000);
    QTRY_VERIFY_WITH_TIMEOUT(!b.busy(), 1000);
    QTRY_VERIFY_WITH_TIMEOUT(advancedForm->isVisible(), 1000);
    QCOMPARE(input->property("text").toString(), QString("invalid"));
    input->forceActiveFocus();
    QTest::keyClick(quickWindow, Qt::Key_A, Qt::ControlModifier);
    QTest::keyClick(quickWindow, '9');
    toggle();
    rejectSave = false;
    QVERIFY(QMetaObject::invokeMethod(save, "clicked"));
    QTRY_VERIFY_WITH_TIMEOUT(b.dialog().isEmpty(), 1000);
    const auto values = f.parameters.at(f.calls.lastIndexOf("settings.save"))["Values"].toObject();
    QCOMPARE(values["Copiers"].toString(), QString("9"));
    QCOMPARE(values["MinDiskFree"].toString(), QString("2"));
  }
  void qmlSidebarTracksNavigation() {
    Fixture f;
    f.handler = [](QString m, auto) {
      if (m == "settings.get")
        return result(QJsonObject{{"Token", "editor"}, {"Fields", QJsonArray{}}});
      return result(QJsonObject{});
    };
    Backend b;
    QQmlEngine engine;
    engine.rootContext()->setContextProperty("backend", &b);
    QQmlComponent component(&engine, QUrl("qrc:/test-ui/qml/Main.qml"));
    QScopedPointer<QObject> window(component.create());
    QVERIFY2(window, qPrintable(component.errorString()));
    QTRY_COMPARE_WITH_TIMEOUT(b.title(), QString("Overview"), 1000);
    auto *sidebar = window->findChild<QObject *>("navigationSidebar");
    QVERIFY(sidebar);
    QVERIFY(sidebar->property("drawerOpen").toBool());
    QVERIFY(!sidebar->property("collapsible").toBool());
    QVERIFY(!sidebar->property("collapseButtonVisible").toBool());
    QQmlListReference actions(sidebar, "actions");
    QCOMPARE(actions.count(), 8);
    auto *overview = actions.at(0);
    auto *devices = actions.at(2);
    auto *settings = actions.at(7);
    QVERIFY(overview->property("checked").toBool());
    QVERIFY(QMetaObject::invokeMethod(settings, "trigger"));
    QCOMPARE(b.page(), QString("Settings"));
    QVERIFY(settings->property("checked").toBool());
    QVERIFY(!overview->property("checked").toBool());
    // A detail page keeps its parent selected even though the title changes.
    b.activate(rowAction(b, "Advanced Configuration", "Open"));
    QCOMPARE(b.title(), QString("Advanced Configuration"));
    QVERIFY(settings->property("checked").toBool());
    b.activate(rowAction(b, "options", "Edit"));
    QTRY_VERIFY_WITH_TIMEOUT(!b.dialog().isEmpty(), 1000);
    QVERIFY(!devices->property("enabled").toBool());
    b.navigate("Devices");
    QCOMPARE(b.page(), QString("Settings"));
    QVERIFY(settings->property("checked").toBool());
    b.cancelDialog();
    QVERIFY(QMetaObject::invokeMethod(devices, "trigger"));
    QCOMPARE(b.page(), QString("Devices"));
    QVERIFY(devices->property("checked").toBool());
    QVERIFY(!settings->property("checked").toBool());
  }
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

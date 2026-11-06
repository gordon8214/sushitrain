// Copyright (C) 2026 Gordon. SPDX-License-Identifier: MPL-2.0
#include "../src/backend.h"
#include "../src/modelutil.h"
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
#include <QSignalSpy>
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
static QQuickItem *editorItem(QObject *window, const QString &name) {
  auto *editor = window->findChild<QObject *>("settingsEditor");
  if (!editor) return nullptr;
  auto *scope = editor->property("isPage").toBool()
      ? window->findChild<QQuickItem *>("settingsStackPage")
      : qvariant_cast<QQuickItem *>(editor->property("contentItem"));
  return scope ? visualItem(scope, name) : nullptr;
}
class BackendTest : public QObject {
  Q_OBJECT
private slots:
  void settingsEscapeKeepsQueryBinding() {
    Fixture fixture;
    Backend backend;
    QQmlEngine engine; engine.rootContext()->setContextProperty("backend", &backend);
    QQmlComponent component(&engine, QUrl("qrc:/test-ui/qml/Main.qml"));
    QScopedPointer<QObject> object(component.create());
    QVERIFY2(object, qPrintable(component.errorString()));
    auto *window = qobject_cast<QQuickWindow *>(object.data());
    QVERIFY(window);
    window->show(); window->requestActivate();
    QTRY_COMPARE(backend.mode(), QString("synchronize"));
    backend.navigate("Settings");
    auto *search = object->findChild<QQuickItem *>("settingsSearch");
    auto *results = object->findChild<QQuickItem *>("settingsResults");
    QVERIFY(search); QVERIFY(results);
    for (auto *focus : {search, results}) {
      backend.setSettingsQuery("launch at login");
      QTRY_COMPARE(search->property("text").toString(), backend.settingsQuery());
      focus->forceActiveFocus();
      QTRY_VERIFY(focus->hasActiveFocus());
      QTest::keyClick(window, Qt::Key_Escape);
      QTRY_VERIFY(backend.settingsQuery().isEmpty());
      backend.setSettingsQuery("cache");
      QTRY_COMPARE(search->property("text").toString(), QString("cache"));
      backend.navigate("Files"); backend.navigate("Settings");
      QTRY_COMPARE(search->property("text").toString(), QString("cache"));
    }
  }
  void overviewLaunchesSystemFileBrowser_data() {
    QTest::addColumn<QString>("mode");
    QTest::newRow("files") << QString("files");
    QTest::newRow("synchronize") << QString("synchronize");
  }
  void overviewLaunchesSystemFileBrowser() {
    QFETCH(QString, mode);
    Fixture f(mode);
    const QString path = f.dir.path() + "/Synctrain";
    f.handler = [path](QString method, auto) {
      if (method == "status") return result(QJsonObject{{"mount", path}});
      if (method == "engine.folders") return result(QJsonArray{QJsonObject{{"ID", "docs"}, {"Label", "Documents"}, {"Path", path}}});
      return result(QJsonObject{});
    };
    UrlRecorder recorder;
    QDesktopServices::setUrlHandler("file", &recorder, "open");
    const auto cleanup = qScopeGuard([] { QDesktopServices::unsetUrlHandler("file"); });
    Backend b;
    QTRY_VERIFY(!rowAction(b, mode == "files" ? "Files mode" : "Documents", "Open in File Manager").isEmpty());
    b.activate(rowAction(b, mode == "files" ? "Files mode" : "Documents", "Open in File Manager"));
    QTRY_COMPARE(recorder.urls.size(), 1);
    QCOMPARE(recorder.urls.first(), QUrl::fromLocalFile(path));
    QCOMPARE(b.title(), QString("Overview"));
    QVERIFY(!f.calls.contains("files.resolve"));
  }
  void gatewaySearchRevealsFieldOnlyForTheExplicitResult() {
    Fixture f;
    const QJsonObject entry{{"id", "gateway.port"}, {"title", "Remote service port"},
      {"category", "remoteAccess"}, {"capability", "synchronize"}, {"action", "remoteService"},
      {"anchor", "gateway.port"}, {"field", "publicPort"}};
    f.handler = [entry](QString method, auto) {
      if (method == "settings.index") return result(QJsonArray{entry});
      if (method == "service.status") return result(QJsonObject{{"enabled", false}, {"publicPort", 8443}});
      return result(QJsonObject{});
    };
    Backend b;
    QTRY_COMPARE(b.title(), QString("Overview"));
    b.setSettingsQuery("remote service port");
    QTRY_VERIFY(!b.settingsResults().isEmpty());
    b.openSettingsResult(entry.toVariantMap());
    QTRY_COMPARE(b.dialog().value("title").toString(), QString("Remote File Service"));
    QCOMPARE(b.page(), QString("Remote Access"));
    QCOMPARE(b.settingsField(), QString("publicPort"));
    b.cancelDialog();
    QTRY_VERIFY(b.dialog().isEmpty());
    b.navigate("Overview");
    b.navigate("Remote Access");
    QTRY_VERIFY(!rowAction(b, "Remote File Service", "Configure").isEmpty());
    QVERIFY(b.dialog().isEmpty());
  }
  void settingsSearchFindsControlsAndProtectsDrafts() {
    Fixture fixture;
    const QJsonObject entry{{"id","engine.folder/docs.copiers"},{"title","Copiers"},{"breadcrumb","Folders › Documents"},
      {"category","advanced"},{"capability","synchronize"},{"action","folder/docs"},{"section","folder/docs"},{"field","copiers"},{"anchor","Copiers"}};
    fixture.handler=[entry](QString method,auto) {
      if(method=="settings.index") return result(QJsonArray{entry});
      if(method=="settings.get") return result(QJsonObject{{"Token","search-editor"},{"Fields",QJsonArray{QJsonObject{{"Path","Copiers"},{"Label","Copiers"},{"Value","7"},{"Kind","string"},{"IsAdvanced",true}}}}});
      return result(QJsonObject{});
    };
    Backend backend;
    QQmlEngine engine; engine.rootContext()->setContextProperty("backend",&backend);
    QQmlComponent component(&engine,QUrl("qrc:/test-ui/qml/Main.qml"));
    QScopedPointer<QObject> window(component.create());
    QVERIFY2(window,qPrintable(component.errorString()));
    QTRY_COMPARE(backend.mode(),QString("synchronize"));
    backend.navigate("Settings");
    QTRY_VERIFY(fixture.calls.contains("settings.index"));
    backend.setSettingsQuery("DOCUMENTS copiers");
    QTRY_COMPARE(backend.settingsResults().size(),1);
    QCOMPARE(fixture.calls.count("settings.get"),0);
    backend.openSettingsResult(backend.settingsResults().first().toMap());
    QTRY_VERIFY(backend.dialog().value("page").toBool());
    auto *editor=window->findChild<QObject *>("settingsEditor");
    QVERIFY(editor);
    QTRY_VERIFY(editor->property("advancedExpanded").toBool());
    backend.fieldChanged();
    backend.setSettingsQuery("launch at login");
    QVERIFY(backend.dirty());
    QVERIFY(!backend.dialog().contains("discard"));
    QVERIFY(!backend.settingsResults().isEmpty());
    backend.openSettingsResult(backend.settingsResults().first().toMap());
    QTRY_VERIFY(backend.dialog().value("discard").toBool());
    backend.keepEditing();
    QVERIFY(backend.dirty());
    QCOMPARE(fixture.calls.count("settings.get"),1);
    backend.setSettingsQuery("");
    QVERIFY(backend.settingsResults().isEmpty());
    QVERIFY(backend.dirty());
  }
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
    b.navigate("settings:network");
    b.activate(rowAction(b, "Connectivity, Discovery, and Relays", "Configure"));
    auto *editor = window->findChild<QObject *>("settingsEditor");
    QVERIFY(editor);
    QTRY_VERIFY_WITH_TIMEOUT(editor->property("presented").toBool(), 1000);
    auto *form = editorItem(window.data(), "editorForm");
    auto *page = window->findChild<QQuickItem *>("settingsStackPage");
    QVERIFY(page && form);
    auto *content = qvariant_cast<QQuickItem *>(page->property("flickable"));
    QVERIFY(content);
    QVERIFY(editor->property("isPage").toBool());
    QVERIFY(!editor->property("opened").toBool());
    const auto formOrigin = form->mapToItem(content, QPointF());
    QVERIFY(formOrigin.x() >= 0);
    QVERIFY(formOrigin.x() + form->width() <= content->width() + 1);
    QVERIFY(page->height() <= quickWindow->height());
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
      const qreal end = content->property("contentHeight").toReal() - content->height();
      QVERIFY(content->setProperty("contentY", end));
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
    b.navigate("settings:network");
    auto open = [&] { b.activate(rowAction(b, "Connectivity, Discovery, and Relays", "Configure")); };
    open();
    auto *editor = window->findChild<QObject *>("settingsEditor");
    QVERIFY(editor);
    QTRY_VERIFY_WITH_TIMEOUT(editor->property("presented").toBool(), 1000);
    auto *disclosure = editorItem(window.data(), "editorAdvanced");
    auto *advancedForm = editorItem(window.data(), "editorAdvancedForm");
    QVERIFY(disclosure && advancedForm);
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
    QTRY_VERIFY_WITH_TIMEOUT(editor->property("presented").toBool(), 1000);
    disclosure = editorItem(window.data(), "editorAdvanced");
    advancedForm = editorItem(window.data(), "editorAdvancedForm");
    QVERIFY(disclosure && advancedForm);
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
    auto *basicForm = editorItem(window.data(), "editorForm");
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
  void resetPreviewRoutesAndCancel() {
    Fixture f;
    f.handler = [](QString m, auto) {
      if (m == "engine.folders")
        return result(QJsonArray{QJsonObject{{"ID", "test"}, {"Label", "Test Folder"}}});
      if (m == "settings.get")
        return result(QJsonObject{{"Token", "reset-editor"}, {"Fields", QJsonArray{
            QJsonObject{{"Path", "SyncXattrs"}, {"Kind", "bool"}, {"Label", "Synchronize extended attributes"}, {"Value", "true"}},
            QJsonObject{{"Path", "Copiers"}, {"Kind", "string"}, {"Label", "Copiers"}, {"Value", "0"}, {"IsAdvanced", true}}}}});
      return result(QJsonObject{});
    };
    Backend b;
    QQmlEngine engine;
    engine.rootContext()->setContextProperty("backend", &b);
    QQmlComponent component(&engine, QUrl("qrc:/test-ui/qml/Main.qml"));
    QScopedPointer<QObject> window(component.create());
    QVERIFY2(window, qPrintable(component.errorString()));
    QTRY_COMPARE_WITH_TIMEOUT(b.title(), QString("Overview"), 1000);
    b.navigate("Folders");
    QTRY_VERIFY_WITH_TIMEOUT(!rowAction(b, "Test Folder", "Reset Synchronization Settings…").isEmpty(), 1000);
    b.activate(rowAction(b, "Test Folder", "Reset Synchronization Settings…"));
    auto *editor = window->findChild<QObject *>("settingsEditor");
    QVERIFY(editor);
    QTRY_VERIFY_WITH_TIMEOUT(editor->property("presented").toBool(), 1000);
    auto *advanced = editorItem(window.data(), "editorAdvancedForm");
    QVERIFY(advanced);
    QCOMPARE(f.parameters.at(f.calls.lastIndexOf("settings.get"))["Section"].toString(), QString("reset/folder/test"));
    QCOMPARE(editor->property("draft").toMap()["Copiers"].toString(), QString("0"));
    QVERIFY(!advanced->isVisible());
    const auto fields = b.dialog()["fields"].toList();
    QVERIFY(!fields.isEmpty());
    QCOMPARE(fields.first().toMap()["Kind"].toString(), QString("notice"));
    QVERIFY(fields.first().toMap()["Label"].toString().contains("then choose Save"));
    b.cancelDialog();
    QTRY_VERIFY_WITH_TIMEOUT(b.dialog().isEmpty(), 1000);
    QTRY_COMPARE_WITH_TIMEOUT(f.calls.count("settings.close"), 1, 1000);
    QCOMPARE(f.calls.count("settings.save"), 0);
    b.navigate("settings:network");
    b.navigate("settings:defaults");
    b.activate(rowAction(b, "Reset Synchronization Defaults…", "Configure"));
    QTRY_VERIFY_WITH_TIMEOUT(editor->property("presented").toBool(), 1000);
    QCOMPARE(f.parameters.at(f.calls.lastIndexOf("settings.get"))["Section"].toString(), QString("reset/defaults/folder"));
    advanced = editorItem(window.data(), "editorAdvancedForm");QVERIFY(advanced);
    QVERIFY(!advanced->isVisible());
    b.cancelDialog();
    QCOMPARE(f.calls.count("settings.save"), 0);
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
    QCOMPARE(actions.count(), 18);
    auto *overview = actions.at(1);
    QVERIFY(overview->property("checked").toBool());
    b.navigate("Settings");
    QCOMPARE(b.page(), QString("settings:general"));
    QCOMPARE(actions.count(), 18);
    QCOMPARE(actions.at(7)->property("text").toString(), QString("General"));
    QCOMPARE(actions.at(17)->property("text").toString(), QString("About"));
    QVERIFY(actions.at(7)->property("checked").toBool());
    b.navigate("settings:advanced");
    b.activate(rowAction(b, "Expert Configuration", "Configure"));
    QCOMPARE(b.title(), QString("Advanced Configuration"));
    b.activate(rowAction(b, "options", "Edit"));
    QTRY_VERIFY_WITH_TIMEOUT(!b.dialog().isEmpty(), 1000);
    // An in-page editor keeps the sidebar available; dirty departures are guarded.
    QVERIFY(actions.at(1)->property("enabled").toBool());
    b.fieldChanged();
    b.navigate("Devices");
    QCOMPARE(b.page(), QString("settings:advanced"));
    QVERIFY(b.dialog().value("discard").toBool());
    b.keepEditing();
    b.cancelDialog();
    b.cancelDialog();
    QVERIFY(QMetaObject::invokeMethod(actions.at(1), "trigger"));
    QCOMPARE(b.page(), QString("Overview"));
    QCOMPARE(actions.count(), 18);
    b.navigate("Devices");
    QVERIFY(actions.at(3)->property("checked").toBool());
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
    b.navigate("settings:network");
    b.activate(rowAction(b, "Connectivity, Discovery, and Relays", "Configure"));
    QTRY_VERIFY_WITH_TIMEOUT(!b.dialog().isEmpty(), 1000);
    auto *editor = window->findChild<QObject *>("settingsEditor");
    auto *back = window->findChild<QObject *>("settingsPageBack");
    auto *save = window->findChild<QObject *>("settingsPageSave");
    auto *keep = window->findChild<QObject *>("settingsPageKeepEditing");
    auto *discard = window->findChild<QObject *>("settingsPageDiscardChanges");
    QVERIFY(editor && back && save && keep && discard);
    QTRY_VERIFY_WITH_TIMEOUT(editor->property("presented").toBool(), 1000);
    QVERIFY(editor->property("isPage").toBool());
    QVERIFY(!editor->property("opened").toBool());
    editor->setProperty("draft", QVariantMap{{"name", "unsaved"}});
    b.fieldChanged();
    QVERIFY(QMetaObject::invokeMethod(back, "clicked"));
    QCoreApplication::processEvents();
    QVERIFY(b.dialog()["discard"].toBool());
    QVERIFY(editor->property("presented").toBool());
    QCOMPARE(editor->property("draft").toMap()["name"].toString(), QString("unsaved"));
    // An invalid save must keep the same visible draft as well.
    QVERIFY(QMetaObject::invokeMethod(keep, "clicked"));
    QVERIFY(!b.dialog().value("discard").toBool());
    QVERIFY(QMetaObject::invokeMethod(save, "trigger")); // Save the preserved draft
    QTRY_COMPARE_WITH_TIMEOUT(b.error(), QString("Invalid settings"), 1000);
    QVERIFY(editor->property("presented").toBool());
    QCOMPARE(editor->property("draft").toMap()["name"].toString(), QString("unsaved"));
    QVERIFY(QMetaObject::invokeMethod(back, "clicked"));
    QVERIFY(b.dialog()["discard"].toBool());
    QVERIFY(editor->property("presented").toBool());
    QVERIFY(QMetaObject::invokeMethod(discard, "clicked"));
    QTRY_VERIFY_WITH_TIMEOUT(b.dialog().isEmpty(), 1000);
  }
  void closePreservesDirtyEditorUntilExplicitDiscard() {
    Fixture f;
    f.handler = [](QString method, auto) {
      if (method == "settings.get") return result(QJsonObject{{"Token", "close-fixture"}, {"Fields", QJsonArray{}}});
      return result(QJsonObject{});
    };
    Backend b;
    QTRY_COMPARE_WITH_TIMEOUT(b.title(), QString("Overview"), 1000);
    b.navigate("settings:network");
    b.activate(rowAction(b, "Connectivity, Discovery, and Relays", "Configure"));
    QTRY_VERIFY_WITH_TIMEOUT(!b.dialog().isEmpty(), 1000);
    b.fieldChanged();
    QSignalSpy closed(&b, &Backend::closeReady);
    QVERIFY(!b.requestClose());
    QVERIFY(b.dialog().value("discard").toBool());
    QVERIFY(!b.requestClose());
    QCOMPARE(closed.count(),0);
    QVERIFY(b.dialog().value("discard").toBool());
    b.keepEditing();
    QVERIFY(b.dirty());
    QCOMPARE(closed.count(), 0);
    QVERIFY(!b.requestClose());
    b.cancelDialog();
    QTRY_COMPARE_WITH_TIMEOUT(closed.count(), 1, 1000);
    QVERIFY(b.dialog().isEmpty());
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
    QTRY_VERIFY(f.calls.contains("mcp.preferences.close"));
    QTRY_COMPARE_WITH_TIMEOUT(f.calls.count("desktop.items"), 1, 1000);
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
    b.navigate("settings:network");
    b.activate(rowAction(b, "Connectivity, Discovery, and Relays", "Configure"));
    b.activate(rowAction(b, "Connectivity, Discovery, and Relays", "Configure"));
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
    b.navigate("settings:network");
    b.activate(rowAction(b, "Connectivity, Discovery, and Relays", "Configure"));
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
    b.navigate("settings:network");
    QTRY_VERIFY_WITH_TIMEOUT(!rowAction(b, "Connectivity, Discovery, and Relays", "Configure").isEmpty(), 1000);
    b.activate(rowAction(b, "Connectivity, Discovery, and Relays", "Configure"));
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
    b.navigate("Remote Access");
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
    b.navigate("History");
    b.navigate("Files");
    QCOMPARE(b.rows().size(),2);
    QCOMPARE(f.calls.count("files.childrenPage"),2);
  }
  void settingsPagesGuardBackAndCategoryNavigation() {
    Fixture fixture;
    fixture.handler=[](QString method,auto) {
      if(method=="settings.get") return result(QJsonObject{{"Token","stack-token"},{"Fields",QJsonArray{QJsonObject{{"Path","Name"},{"Kind","string"},{"Value","confirmed"},{"Label","Name"}}}}});
      return result(QJsonObject{});
    };
    Backend backend;QTRY_COMPARE_WITH_TIMEOUT(backend.title(),QString("Overview"),1000);
    backend.navigate("settings:network");
    backend.activate(rowAction(backend,"Connection Options","Configure"));
    QTRY_VERIFY_WITH_TIMEOUT(backend.dialog().value("page").toBool(),1000);
    backend.fieldChanged();backend.cancelDialog();
    QVERIFY(backend.dialog().value("discard").toBool());
    backend.keepEditing();QVERIFY(backend.dirty());
    backend.navigate("settings:advanced");
    QVERIFY(backend.dialog().value("discard").toBool());
    QCOMPARE(backend.page(),QString("settings:network"));
    backend.keepEditing();backend.cancelDialog();backend.cancelDialog();
    QTRY_VERIFY_WITH_TIMEOUT(backend.dialog().isEmpty(),1000);
    QCOMPARE(backend.page(),QString("settings:network"));
    backend.activate(rowAction(backend,"Connection Options","Configure"));
    QTRY_VERIFY_WITH_TIMEOUT(!backend.dialog().isEmpty(),1000);
    backend.fieldChanged();backend.navigate("settings:advanced");backend.cancelDialog();
    QTRY_COMPARE_WITH_TIMEOUT(backend.page(),QString("settings:advanced"),1000);
  }

  void overviewProgressBelongsToTransferRows() {
    Fixture fixture;
    fixture.delay=[](QString method,auto) { return method=="engine.stats"?40:0; };
    fixture.handler=[](QString method,auto) {
      if(method=="engine.status") return result(QJsonObject{{"name","Upload"}});
      if(method=="engine.folders") return result(QJsonArray{QJsonObject{{"ID","folder"},{"Label","Download"}}});
      if(method=="engine.stats") return result(QJsonObject{
        {"download",QJsonObject{{"Percentage",0.25}}},{"upload",QJsonObject{{"Percentage",0.75}}}});
      return result(QJsonObject{});
    };
    Backend backend;QTRY_COMPARE_WITH_TIMEOUT(backend.title(),QString("Overview"),1000);
    auto progressRows=[&] { int count=0;for(auto value:backend.rows())if(value.toMap().contains("progress"))++count;return count; };
    QTRY_COMPARE_WITH_TIMEOUT(progressRows(),2,1000);
    for(auto value:backend.rows()) {
      auto row=value.toMap();
      if(!row["actions"].toList().isEmpty())QVERIFY(!row.contains("progress"));
      if(row.contains("progress")) {
        QVERIFY(row["actions"].toList().isEmpty());
        QCOMPARE(row["progress"].toDouble(),row["title"]=="Download"?0.25:0.75);
      }
    }
  }

  void filesFoldersAndRemoteAccessHaveDistinctContent() {
    Fixture fixture("files");
    fixture.handler=[](QString method,auto) {
      if(method=="status") return result(QJsonObject{
        {"gateways",QJsonArray{QJsonObject{{"name","Fixture Gateway"},{"serviceID","gateway"},{"state","offline"}}}},
        {"folders",QJsonArray{QJsonObject{{"label","Fixture Folder"},{"id","folder"},{"gateway","gateway"},{"state","offline"}}}}});
      return result(QJsonObject{});
    };
    Backend backend;QTRY_COMPARE_WITH_TIMEOUT(backend.mode(),QString("files"),1000);
    backend.navigate("Folders");
    QTRY_VERIFY_WITH_TIMEOUT(!rowAction(backend,"Fixture Folder","Browse").isEmpty(),1000);
    QVERIFY(rowAction(backend,"Fixture Gateway","Add Folders").isEmpty());
    QVERIFY(!key(backend.commands(),"Add Folder…").isEmpty());
    backend.navigate("Remote Access");
    QTRY_VERIFY_WITH_TIMEOUT(!rowAction(backend,"Fixture Gateway","Add Folders").isEmpty(),1000);
    QVERIFY(rowAction(backend,"Fixture Folder","Browse").isEmpty());
    QVERIFY(!key(backend.commands(),"Pair…").isEmpty());
  }
  void immediatePreferenceFailureKeepsConfirmedValue() {
    Fixture fixture("files");
    fixture.handler=[](QString method,QJsonObject params) {
      if(method=="mcp.settings") {
        if(params.contains("Enabled")) return QJsonObject{{"error","Fixture persistence failure"}};
        return result(QJsonObject{{"Enabled",true}});
      }
      return result(QJsonObject{});
    };
    Backend backend;
    QQmlEngine engine;engine.rootContext()->setContextProperty("backend",&backend);
    QQmlComponent component(&engine,QUrl("qrc:/test-ui/qml/Main.qml"));
    QScopedPointer<QObject> window(component.create());QVERIFY2(window,qPrintable(component.errorString()));
    auto *quickWindow=qobject_cast<QQuickWindow *>(window.data());QVERIFY(quickWindow);
    quickWindow->show();QVERIFY(QTest::qWaitForWindowExposed(quickWindow,1000));
    QTRY_COMPARE_WITH_TIMEOUT(backend.mode(),QString("files"),1000);
    backend.navigate("settings:integrations");
    backend.activate(rowAction(backend,"MCP","Configure"));
    QTRY_COMPARE_WITH_TIMEOUT(backend.rows().size(),1,1000);
    auto *toggle=visualItem(quickWindow->contentItem(),"immediatePreferenceToggle");QVERIFY(toggle);
    QVERIFY(toggle->isVisible());QVERIFY(toggle->property("checked").toBool());
    toggle->forceActiveFocus();QTest::keyClick(quickWindow,Qt::Key_Space);
    QTRY_VERIFY_WITH_TIMEOUT(!backend.error().isEmpty(),1000);
    QVERIFY(backend.rows().first().toMap()["checked"].toBool());
    toggle=visualItem(quickWindow->contentItem(),"immediatePreferenceToggle");QVERIFY(toggle);
    QVERIFY(toggle->property("checked").toBool());
    QVERIFY(backend.dialog().isEmpty());
  }

  void operatingModeIsVisible_data() {
    QTest::addColumn<QString>("mode");
    QTest::addColumn<QString>("current");
    QTest::addColumn<QString>("target");
    QTest::newRow("synchronize") << QString("synchronize") << QString("Synchronize") << QString("Files");
    QTest::newRow("files") << QString("files") << QString("Files") << QString("Synchronize");
  }

  void operatingModeIsVisible() {
    QFETCH(QString, mode);
    QFETCH(QString, current);
    QFETCH(QString, target);
    Fixture fixture(mode);
    Backend backend;
    QTRY_COMPARE_WITH_TIMEOUT(backend.mode(), mode, 1000);
    QQmlEngine engine;
    engine.rootContext()->setContextProperty("backend", &backend);
    QQmlComponent component(&engine, QUrl("qrc:/test-ui/qml/Main.qml"));
    QScopedPointer<QObject> object(component.create());
    QVERIFY2(object, qPrintable(component.errorString()));
    auto *window = qobject_cast<QQuickWindow *>(object.data());
    QVERIFY(window);
    window->show();
    backend.navigate("settings:general");
    std::function<QQuickItem *(QQuickItem *, const QString &)> visibleText =
        [&](QQuickItem *item, const QString &text) -> QQuickItem * {
      if (item->isVisible() && item->property("text").toString() == text)
        return item;
      for (auto *child : item->childItems())
        if (auto *found = visibleText(child, text)) return found;
      return nullptr;
    };
    QTRY_VERIFY_WITH_TIMEOUT(visibleText(window->contentItem(), "Current mode: " + current), 1000);
    const auto label = "Switch to " + target + "…";
    QTRY_VERIFY_WITH_TIMEOUT(visibleText(window->contentItem(), label), 1000);
    auto *button = visibleText(window->contentItem(), label);
    QVERIFY(QMetaObject::invokeMethod(button, "clicked"));
    QVERIFY(backend.dialog()["title"].toString().contains(target.toLower()));
    backend.cancelDialog();
    QCOMPARE(backend.mode(), mode);
    QVERIFY(visibleText(window->contentItem(), "Current mode: " + current));
  }

  void operatingModeChangeDiscardsBrowserSnapshot() {
    Fixture fixture("synchronize");
    fixture.handler=[](QString method,auto) {
      if(method=="engine.folders")return result(QJsonArray{QJsonObject{{"ID","old"},{"Label","Old Folder"}}});
      if(method=="engine.browsePage")return result(QJsonObject{{"Entries",QJsonArray{QJsonObject{{"Name","old.txt"},{"Path","old.txt"},{"Directory",false},{"Local",true},{"Identity","old-token"}}}}});
      if(method=="files.childrenPage")return result(QJsonObject{{"Entries",QJsonArray{QJsonObject{{"Name","New Remote Folder"},{"Item",1},{"Kind",3}}}}});
      return result(QJsonObject{});
    };
    Backend backend;QTRY_COMPARE_WITH_TIMEOUT(backend.mode(),QString("synchronize"),1000);
    backend.navigate("folder:old");QTRY_VERIFY_WITH_TIMEOUT(!rowAction(backend,"old.txt","Open").isEmpty(),1000);
    QTest::qWait(20);
    backend.navigate("settings:general");
    QFile helper(fixture.dir.path()+"/synctrain-desktop");QVERIFY(helper.open(QIODevice::WriteOnly|QIODevice::Truncate));
    QJsonObject host{{"Mode","files"},{"FilesSocket",fixture.server.fullServerName()}};
    helper.write("#!/bin/sh\nprintf '%s\\n' '");helper.write(QJsonDocument(result(host)).toJson(QJsonDocument::Compact));helper.write("'\n");helper.close();
    backend.activate(rowAction(backend,"Operating Mode","Switch to Files…"));backend.submit({});
    QTRY_COMPARE_WITH_TIMEOUT(backend.mode(),QString("files"),1000);
    QVERIFY(!rowAction(backend,"Operating Mode","Switch to Synchronize…").isEmpty());
    for (const auto &value : backend.rows()) {
      const auto row = value.toMap();
      if (row["title"] == "Operating Mode")
        QCOMPARE(row["subtitle"].toString(), QString("Current mode: Files"));
    }
    backend.navigate("Files");
    QTRY_VERIFY_WITH_TIMEOUT(!rowAction(backend,"New Remote Folder","Open Folder").isEmpty(),1000);
    QVERIFY(rowAction(backend,"old.txt","Open").isEmpty());
  }

  void catalogCategoriesAndCapabilitiesAcrossModes() {
    QFile resource(":/desktop-ui/catalog.json"); QVERIFY(resource.open(QIODevice::ReadOnly));
    const auto catalog=QJsonDocument::fromJson(resource.readAll()).object();
    for(const auto &mode:QStringList{"synchronize","files"}) {
      Fixture fixture(mode);Backend backend;
      QTRY_COMPARE_WITH_TIMEOUT(backend.mode(),mode,1000);
      for(auto entry:catalog["settings"].toArray()) {
        auto category=entry.toObject();
        QJsonArray sections;
        for (auto section : category["sections"].toArray()) if (catalogAvailable(section.toObject(), mode)) sections.append(section);
        backend.navigate("settings:"+category["id"].toString());
        QCOMPARE(backend.navigation().size(),mode=="files" ? 13 : 18);
        if (sections.isEmpty()) { QCOMPARE(backend.page(), QString("Overview")); continue; }
        QCOMPARE(backend.title(),category["title"].toString());
        QCOMPARE(backend.rows().size(),sections.size());
        for(int i=0;i<sections.size();++i) {
          const auto section=sections[i].toObject(),row=QJsonObject::fromVariantMap(backend.rows()[i].toMap());
          QCOMPARE(row["title"],section["title"]);
          QVERIFY(!row["actions"].toArray().isEmpty());
        }
      }
    }
  }
};
QTEST_MAIN(BackendTest)
#include "backendtest.moc"

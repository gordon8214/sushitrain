// Copyright (C) 2026 Gordon. SPDX-License-Identifier: MPL-2.0
#include "../plugins/statuscache.h"
#include <KAbstractFileItemActionPlugin>
#include <KFileItem>
#include <KFileItemListProperties>
#include <KPluginFactory>
#include <KPluginMetaData>
#include <QElapsedTimer>
#include <QFile>
#include <QJsonArray>
#include <QJsonDocument>
#include <QMenu>
#include <QLocalServer>
#include <QLocalSocket>
#include <QPluginLoader>
#include <QScopedPointer>
#include <QTemporaryDir>
#include <QTest>
#include <sys/stat.h>
class DesktopHelperFixture {
  QByteArray path = qgetenv("PATH");

public:
  DesktopHelperFixture(const QString &dir, const QString &socket) {
    QFile file(dir + "/synctrain-desktop");
    file.open(QIODevice::WriteOnly);
    file.write("#!/bin/sh\nprintf '%s\\n' '");
    file.write(
        QJsonDocument(
            QJsonObject{
                {"result",
                 QJsonObject{
                     {"Mode", "synchronize"},
                     {"Check", QJsonObject{{"Home", QJsonObject{{"socket",
                                                                 socket}}}}}}}})
            .toJson(QJsonDocument::Compact));
    file.write("'\n");
    file.close();
    file.setPermissions(QFile::ReadOwner | QFile::WriteOwner | QFile::ExeOwner);
    qputenv("PATH", dir.toUtf8() + ":" + path);
  }
  ~DesktopHelperFixture() { qputenv("PATH", path); }
};
class DolphinTest : public QObject {
  Q_OBJECT
private slots:
  void coldMenuActionsRevalidateBeforeRootDiscovery() {
    QTemporaryDir dir;
    QLocalServer server;
    QVERIFY(server.listen(dir.path() + "/admin"));
    DesktopHelperFixture helper(dir.path(), server.fullServerName());
    bool rootsPending = false;
    int rescans = 0;
    connect(&server, &QLocalServer::newConnection, &server, [&] {
      auto *socket = server.nextPendingConnection();
      auto buffer = std::make_shared<QByteArray>();
      connect(socket, &QLocalSocket::readyRead, socket, [&, socket, buffer] {
        buffer->append(socket->readAll());
        if (!buffer->contains('\n'))
          return;
        auto request = QJsonDocument::fromJson(*buffer).object();
        auto method = request["method"].toString();
        QJsonValue response;
        if (method == "engine.folders") {
          rootsPending = true; // Deliberately complete capabilities first.
          return;
        }
        if (method == "desktop.items") {
          QJsonArray items;
          for (auto path : request["params"].toObject()["Paths"].toArray())
            items.append(QJsonObject{{"Path", path}, {"Folder", "folder"},
                                     {"RelativePath", path.toString().mid(6)},
                                     {"Actions", QJsonArray{"rescan", "versions"}}});
          response = items;
        } else if (method == "engine.action") {
          QCOMPARE(request["params"].toObject()["Action"].toString(), QString("rescan"));
          ++rescans;
        }
        socket->write(QJsonDocument(QJsonObject{{"result", response}}).toJson(QJsonDocument::Compact) + '\n');
      });
    });
    QPluginLoader loader(ACTIONS_PLUGIN);
    auto *factory = qobject_cast<KPluginFactory *>(loader.instance());
    QVERIFY2(factory, qPrintable(loader.errorString()));
    QScopedPointer<KAbstractFileItemActionPlugin> plugin(factory->create<KAbstractFileItemActionPlugin>(nullptr));
    QVERIFY(plugin);
    QWidget parent;
    KFileItemList selection{
        KFileItem(QUrl::fromLocalFile("/sync/a"), "text/plain", S_IFREG),
        KFileItem(QUrl::fromLocalFile("/sync/b"), "text/plain", S_IFREG)};
    const auto actions = plugin->actions(KFileItemListProperties(selection), &parent);
    QCOMPARE(actions.size(), 1);
    auto *menu = actions.first()->menu();
    QVERIFY(menu);
    QAction *rescan = nullptr;
    QTRY_VERIFY_WITH_TIMEOUT(([&] {
      for (auto *action : menu->actions())
        if (action->text() == "Rescan")
          rescan = action;
      return rescan != nullptr;
    })(), 1000);
    QVERIFY(rootsPending);
    for (auto *action : menu->actions())
      QVERIFY(!action->text().startsWith("Previous Versions"));
    rescan->trigger();
    QTRY_COMPARE_WITH_TIMEOUT(rescans, 2, 1000);
  }
  void containment() {
    QVERIFY(StatusCache::contains("/sync", "/sync/a"));
    QVERIFY(!StatusCache::contains("/sync", "/sync-other/a"));
    QVERIFY(StatusCache::localPath(QUrl("smb://server/sync/a")).isEmpty());
    QCOMPARE(StatusCache::localPath(QUrl("file:///sync/space%20%23/caf%C3%A9")),
             QString::fromUtf8("/sync/space #/café"));
  }
  void coldCacheAndServiceLoss() {
    QTemporaryDir dir;
    QLocalServer server;
    QVERIFY(server.listen(dir.path() + "/admin"));
    DesktopHelperFixture helper(dir.path(), server.fullServerName());
    bool requests = false;
    connect(&server, &QLocalServer::newConnection, &server, [&] {
      auto *socket = server.nextPendingConnection();
      connect(socket, &QLocalSocket::readyRead, socket, [&, socket] {
        auto data = socket->readAll();
        if (data.contains("engine.folders"))
          socket->write("{\"result\":[{\"Path\":\"/sync\"}]}\n");
        if (data.contains("desktop.items")) {
          requests = true;
          socket->write(
              "{\"result\":[{\"Path\":\"/sync/"
              "a\",\"State\":\"synced\",\"Actions\":[\"status\"]}]}\n");
        }
      });
    });
    StatusCache cache;
    // This request arrives before asynchronous root discovery has completed.
    QElapsedTimer elapsed;
    elapsed.start();
    QVERIFY(cache.status(QUrl::fromLocalFile("/sync/a")).isEmpty());
    QVERIFY(elapsed.elapsed() < 20);
    QTRY_VERIFY_WITH_TIMEOUT(requests, 500);
    QTRY_COMPARE_WITH_TIMEOUT(
        cache.status(QUrl::fromLocalFile("/sync/a"))["State"].toString(),
        QString("synced"), 500);
    server.close();
    for (auto *socket : server.findChildren<QLocalSocket *>())
      socket->abort();
    // A fresh request must clear cached success when the service disappears.
    QTest::qWait(2050);
    cache.status(QUrl::fromLocalFile("/sync/a"));
    QTRY_VERIFY_WITH_TIMEOUT(
        cache.status(QUrl::fromLocalFile("/sync/a"))["State"].toString() !=
            "synced",
        1000);
  }
  void unknownHasNoSuccessBadge() {
    QVERIFY(StatusCache::overlays("unknown").isEmpty());
    QVERIFY(StatusCache::overlays("ignored").isEmpty());
    QVERIFY(!StatusCache::overlays("synced").isEmpty());
  }
  void obsoleteResponseCannotRestoreBadge() {
    QTemporaryDir dir;
    QLocalServer server;
    QVERIFY(server.listen(dir.path() + "/admin"));
    DesktopHelperFixture helper(dir.path(), server.fullServerName());
    QLocalSocket *delayed = nullptr;
    connect(&server, &QLocalServer::newConnection, &server, [&] {
      auto *socket = server.nextPendingConnection();
      connect(socket, &QLocalSocket::readyRead, socket, [&, socket] {
        const auto data = socket->readAll();
        if (data.contains("engine.folders"))
          socket->write("{\"result\":[{\"Path\":\"/sync\"}]}\n");
        if (data.contains("desktop.items"))
          delayed = socket;
      });
    });
    StatusCache cache;
    cache.status(QUrl::fromLocalFile("/sync/a"));
    QTRY_VERIFY_WITH_TIMEOUT(delayed, 1000);
    // A mode or mount change makes the old response obsolete.
    cache.roots = {"/other"};
    delayed->write(
        "{\"result\":[{\"Path\":\"/sync/a\",\"State\":\"synced\"}]}\n");
    QTest::qWait(30);
    cache.roots = {"/sync"};
    QVERIFY(cache.status(QUrl::fromLocalFile("/sync/a")).isEmpty());
  }
  void actionsAreRevalidated() {
    QTemporaryDir dir;
    QLocalServer server;
    QVERIFY(server.listen(dir.path() + "/admin"));
    DesktopHelperFixture helper(dir.path(), server.fullServerName());
    connect(&server, &QLocalServer::newConnection, &server, [&] {
      auto *socket = server.nextPendingConnection();
      connect(socket, &QLocalSocket::readyRead, socket, [socket] {
        socket->readAll();
        socket->write("{\"result\":[{\"Path\":\"/sync/"
                      "a\",\"State\":\"unknown\",\"Actions\":[]}]}\n");
      });
    });
    StatusCache cache;
    cache.socket = server.fullServerName();
    cache.roots = {"/sync"};
    bool done = false;
    cache.perform("rescan", {QUrl::fromLocalFile("/sync/a")},
                  [&](auto, QString error) {
                    QVERIFY(error.contains("no longer available"));
                    done = true;
                  });
    QTRY_VERIFY_WITH_TIMEOUT(done, 1000);
  }
  void pluginsLoad() {
    KPluginMetaData metadata{QString(ACTIONS_PLUGIN)};
    QVERIFY(metadata.supportsMimeType("text/plain"));
    QVERIFY(metadata.supportsMimeType("inode/directory"));
    for (const auto &path : QStringList{ACTIONS_PLUGIN, OVERLAY_PLUGIN}) {
      QPluginLoader loader(path);
      QVERIFY2(loader.instance(), qPrintable(loader.errorString()));
    }
  }
};
QTEST_MAIN(DolphinTest)
#include "dolphintest.moc"

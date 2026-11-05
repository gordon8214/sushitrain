// Copyright (C) 2026 Gordon. SPDX-License-Identifier: MPL-2.0
#include "../src/serviceclient.h"
#include <QJsonDocument>
#include <QLocalServer>
#include <QLocalSocket>
#include <QTemporaryDir>
#include <QTest>
#include <QTimer>
class ClientTest : public QObject {
  Q_OBJECT
private slots:
  void partialResponse() {
    QTemporaryDir dir;
    QLocalServer server;
    QVERIFY(server.listen(dir.path() + "/admin"));
    connect(&server, &QLocalServer::newConnection, &server, [&server] {
      auto *s = server.nextPendingConnection();
      connect(s, &QLocalSocket::readyRead, s, [s] {
        s->readAll();
        s->write("{\"result\":{\"ID\":9007199254740993");
        s->flush();
        QTimer::singleShot(10, s, [s] { s->write("}}\n"); });
      });
    });
    ServiceClient client;
    QObject owner;
    bool done = false;
    client.call(server.fullServerName(), "status", {}, &owner,
                [&](QJsonValue v, QString error) {
                  QVERIFY(error.isEmpty());
                  QCOMPARE(v.toObject()["ID"].toInteger(),
                           qint64(9007199254740993LL));
                  done = true;
                });
    QTRY_VERIFY_WITH_TIMEOUT(done, 1000);
  }
  void absoluteDeadline() {
    QTemporaryDir dir;
    QLocalServer server;
    QVERIFY(server.listen(dir.path() + "/admin"));
    connect(&server, &QLocalServer::newConnection, &server, [&server] {
      auto *s = server.nextPendingConnection();
      auto *timer = new QTimer(s);
      timer->setInterval(5);
      connect(timer, &QTimer::timeout, s, [s] { s->write(" "); });
      timer->start();
    });
    ServiceClient client;
    QObject owner;
    bool done = false;
    client.call(
        server.fullServerName(), "status", {}, &owner,
        [&](auto, QString error) {
          QVERIFY(error.contains("in time"));
          done = true;
        },
        50);
    QTRY_VERIFY_WITH_TIMEOUT(done, 500);
  }
  void cancellation() {
    QTemporaryDir dir;
    QLocalServer server;
    QVERIFY(server.listen(dir.path() + "/admin"));
    ServiceClient client;
    auto *owner = new QObject;
    bool called = false;
    client.call(server.fullServerName(), "status", {}, owner,
                [&](auto, auto) { called = true; });
    delete owner;
    QTest::qWait(30);
    QVERIFY(!called);
  }
  void missingSocket() {
    ServiceClient client;
    QObject owner;
    bool done = false;
    client.call("/nonexistent/synctrain.sock", "status", {}, &owner,
                [&](auto, QString error) {
                  QVERIFY(!error.isEmpty());
                  done = true;
                });
    QTRY_VERIFY_WITH_TIMEOUT(done, 500);
  }
  void deadlineUnits() {
    auto doc =
        QJsonDocument::fromJson(ServiceClient::request("status", {}, 100));
    QVERIFY(doc["deadline"].toInteger() > qint64(1000000000000000000LL));
  }
};
QTEST_GUILESS_MAIN(ClientTest)
#include "clienttest.moc"

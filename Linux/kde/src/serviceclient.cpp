// Copyright (C) 2026 Gordon. SPDX-License-Identifier: MPL-2.0
#include "serviceclient.h"
#include <QCoreApplication>
#include <QDateTime>
#include <QDir>
#include <QFileInfo>
#include <QJsonDocument>
#include <QLocalSocket>
#include <QPointer>
#include <QProcess>
#include <QStandardPaths>
#include <QTimer>

QByteArray ServiceClient::request(const QString &method,
                                  const QJsonObject &params, int timeout) {
  return QJsonDocument(
             QJsonObject{
                 {"method", method},
                 {"params", params},
                 {"deadline",
                  QJsonValue((QDateTime::currentMSecsSinceEpoch() + timeout) *
                             qint64(1000000))}})
             .toJson(QJsonDocument::Compact) +
         '\n';
}
QString ServiceClient::sibling(const QString &name) {
  const auto path = QCoreApplication::applicationDirPath() + "/" + name;
  return QFileInfo(path).isExecutable() ? path
                                        : QStandardPaths::findExecutable(name);
}
void ServiceClient::decode(const QByteArray &data, const Reply &reply) {
  QJsonParseError error;
  const auto doc = QJsonDocument::fromJson(data, &error);
  if (error.error != QJsonParseError::NoError || !doc.isObject()) {
    reply({}, "The background service returned an invalid response.");
    return;
  }
  const auto obj = doc.object();
  reply(obj.value("result"), obj.value("error").toString());
}
void ServiceClient::call(const QString &path, const QString &method,
                         const QJsonObject &params, QObject *owner, Reply reply,
                         int timeout) {
  auto *socket = new QLocalSocket(owner);
  auto *timer = new QTimer(socket);
  timer->setSingleShot(true);
  auto done = std::make_shared<bool>(false);
  auto buffer = std::make_shared<QByteArray>();
  QPointer<QObject> guard(owner);
  auto finish = [socket, done, reply, guard](QJsonValue value, QString error) {
    if (*done)
      return;
    *done = true;
    socket->disconnect();
    socket->abort();
    socket->deleteLater();
    if (guard)
      reply(value, error);
  };
  connect(socket, &QLocalSocket::connected, socket,
          [socket, method, params, timeout] {
            socket->write(request(method, params, timeout));
          });
  connect(socket, &QLocalSocket::readyRead, socket, [socket, buffer, finish] {
    buffer->append(socket->readAll());
    if (buffer->size() > 16 * 1024 * 1024) {
      finish({}, "The background response is too large.");
      return;
    }
    const auto newline = buffer->indexOf('\n');
    if (newline >= 0)
      decode(buffer->left(newline), finish);
  });
  connect(socket, &QLocalSocket::errorOccurred, socket,
          [socket, finish](auto) { finish({}, socket->errorString()); });
  connect(socket, &QLocalSocket::disconnected, socket, [finish] {
    finish({}, "The background service closed its connection.");
  });
  connect(timer, &QTimer::timeout, socket, [finish] {
    finish({}, "The background service did not respond in time.");
  });
  timer->start(timeout);
  socket->connectToServer(path);
}
void ServiceClient::helper(const QString &method, const QJsonObject &params,
                           QObject *owner, Reply reply, int timeout) {
  auto *process = new QProcess(owner);
  auto *timer = new QTimer(process);
  timer->setSingleShot(true);
  auto done = std::make_shared<bool>(false);
  QPointer<QObject> guard(owner);
  auto finish = [process, done, reply, guard](QJsonValue result,
                                              QString error) {
    if (*done)
      return;
    *done = true;
    process->disconnect();
    if (process->state() != QProcess::NotRunning)
      process->kill();
    process->deleteLater();
    if (guard)
      reply(result, error);
  };
  connect(process, &QProcess::started, process,
          [process, method, params, timeout] {
            process->write(request(method, params, timeout));
            process->closeWriteChannel();
          });
  connect(process, qOverload<int, QProcess::ExitStatus>(&QProcess::finished),
          process, [process, finish](int code, auto status) {
            if (code != 0 || status != QProcess::NormalExit) {
              finish({}, QString::fromUtf8(process->readAllStandardError()));
              return;
            }
            decode(process->readAllStandardOutput(), finish);
          });
  connect(process, &QProcess::errorOccurred, process,
          [process, finish](auto) { finish({}, process->errorString()); });
  connect(timer, &QTimer::timeout, process, [finish] {
    finish({}, "The desktop operation did not finish in time.");
  });
  timer->start(timeout);
  process->start(sibling("synctrain-desktop"), {});
}

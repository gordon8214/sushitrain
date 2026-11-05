// Copyright (C) 2026 Gordon. SPDX-License-Identifier: MPL-2.0
#pragma once
#include <QJsonObject>
#include <QJsonValue>
#include <QObject>
#include <functional>

using Reply = std::function<void(QJsonValue, QString)>;
// Each request owns a connection: cancellation cannot consume another reply.
class ServiceClient : public QObject {
  Q_OBJECT
public:
  using QObject::QObject;
  void call(const QString &socket, const QString &method,
            const QJsonObject &params, QObject *owner, Reply reply,
            int timeout = 15000);
  void helper(const QString &method, const QJsonObject &params, QObject *owner,
              Reply reply, int timeout = 15000);
  static QByteArray request(const QString &method, const QJsonObject &params,
                            int timeout);
  static void decode(const QByteArray &data, const Reply &reply);
  static QString sibling(const QString &name);
};

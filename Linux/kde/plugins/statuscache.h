// Copyright (C) 2026 Gordon. SPDX-License-Identifier: MPL-2.0
#pragma once
#include "../src/serviceclient.h"
#include <QHash>
#include <QSet>
#include <QTimer>
#include <QUrl>

class StatusCache : public QObject {
  Q_OBJECT
public:
  explicit StatusCache(QObject *parent = nullptr);
  QJsonObject status(const QUrl &url);
  bool managed(const QUrl &url) const;
  void perform(const QString &operation, const QList<QUrl> &urls, Reply reply);
  static QString localPath(const QUrl &url);
  static QStringList overlays(const QString &state);
  static bool contains(const QString &root, const QString &path);
  // The app and plugins use the same admin protocol and service discovery.
  ServiceClient client;
  QString socket, mode;
  QStringList roots;
  void discover();
signals:
  void updated(const QUrl &url, const QJsonObject &status);

private:
  struct Cached {
    QJsonObject value;
    qint64 at = 0, seen = 0;
  };
  QHash<QString, Cached> items;
  QSet<QString> pending;
  QTimer batch, refresh;
  int ticks = 0;
  bool loading = false, discovering = false;
  void flush();
};

// Copyright (C) 2026 Gordon. SPDX-License-Identifier: MPL-2.0
#pragma once
#include <QDir>
#include <QFile>
#include <QFileInfo>
#include <QQmlComponent>
#include <QQmlEngine>
#include <QQuickWindow>
#include <QStandardPaths>
#include <sys/file.h>

inline QQuickWindow *createPreviewWindow(QQmlEngine *engine, const QUrl &source,
                                         const QVariantMap &properties) {
  QQmlComponent component(engine, source);
  auto *object = component.createWithInitialProperties(properties);
  auto *window = qobject_cast<QQuickWindow *>(object);
  if (!window) {
    qWarning() << component.errors();
    delete object;
    return nullptr;
  }
  object->setParent(engine);
  const auto file = QUrl(properties["file"].toString()).toLocalFile();
  const auto directory = QFileInfo(file).absolutePath();
  const auto cache = QDir::cleanPath(
      QStandardPaths::writableLocation(QStandardPaths::GenericCacheLocation) +
      "/synctrain/desktop-previews");
  if (!file.isEmpty() && QFileInfo(directory).absolutePath() == cache &&
      QFileInfo(directory).fileName().startsWith("preview-")) {
    auto *lease = new QFile(directory + "/.in-use", window);
    // Share the Go cache's flock lease until the native window is destroyed.
    if (!lease->open(QIODevice::ReadWrite) ||
        flock(lease->handle(), LOCK_SH | LOCK_NB) != 0) {
      qWarning() << "Could not retain the active preview" << file;
      delete window;
      return nullptr;
    }
  }
  // QQmlComponent creates C++-owned roots, which QML destroy() cannot release.
  QObject::connect(window, &QQuickWindow::closing, window, &QObject::deleteLater);
  return window;
}

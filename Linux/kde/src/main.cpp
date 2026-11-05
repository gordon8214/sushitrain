// Copyright (C) 2026 Gordon. SPDX-License-Identifier: MPL-2.0
#include "backend.h"
#include "previewwindow.h"
#include <KAboutData>
#include <KDBusService>
#include <KFilePlacesModel>
#include <Prison/Barcode>
#include <QApplication>
#include <QDir>
#include <QFile>
#include <QJsonDocument>
#include <QQmlApplicationEngine>
#include <QQmlComponent>
#include <QQmlContext>
#include <QQuickImageProvider>
#include <QQuickStyle>
#include <QQuickWindow>
#include <QSaveFile>
#include <QStandardPaths>
#include <QTimer>

class QRProvider : public QQuickImageProvider {
public:
  QRProvider() : QQuickImageProvider(Image) {}
  QImage requestImage(const QString &id, QSize *size, const QSize &) override {
    auto barcode = Prison::Barcode::create(Prison::QRCode);
    if (!barcode)
      return {};
    barcode->setData(QUrl::fromPercentEncoding(id.toUtf8()));
    auto image = barcode->toImage(QSizeF(320, 320));
    if (size)
      *size = image.size();
    return image;
  }
};
// A short-lived helper updates Places even when no Synctrain window is open.
static int syncPlaces() {
  QFile input;
  if (!input.open(stdin, QIODevice::ReadOnly))
    return 1;
  auto request = QJsonDocument::fromJson(input.readAll()).object();
  const auto mount = request["mount"].toString();
  if (!QDir::isAbsolutePath(mount))
    return 2;
  const auto state =
      QStandardPaths::writableLocation(QStandardPaths::GenericStateLocation) +
      "/synctrain";
  QDir().mkpath(state);
  QFile oldFile(state + "/kde-places.json");
  auto owned = oldFile.open(QIODevice::ReadOnly)
                   ? QJsonDocument::fromJson(oldFile.readAll()).object()
                   : QJsonObject{};
  QJsonObject wanted;
  for (auto value : request["folders"].toArray()) {
    auto folder = value.toObject();
    const auto name = folder["dirName"].toString();
    if (name.isEmpty() || name.contains('/') || name == "..")
      continue;
    wanted[QUrl::fromLocalFile(mount + "/" + name).toString()] =
        folder["label"].toString();
  }
  KFilePlacesModel model;
  // Only remove a place when both its URL and original label still identify
  // ours.
  for (int i = model.rowCount() - 1; i >= 0; --i) {
    auto index = model.index(i, 0);
    auto url = model.url(index).toString();
    if (owned.contains(url) && !wanted.contains(url) &&
        model.text(index) == owned[url].toString())
      model.removePlace(index);
  }
  QJsonObject next;
  for (auto it = wanted.constBegin(); it != wanted.constEnd(); ++it) {
    bool found = false;
    for (int i = 0; i < model.rowCount(); ++i) {
      auto index = model.index(i, 0);
      if (model.url(index).toString() == it.key()) {
        found = true;
        if (owned.contains(it.key()) &&
            model.text(index) == owned[it.key()].toString()) {
          model.editPlace(index, it->toString(), model.url(index),
                          "folder-remote");
          next[it.key()] = it.value();
        }
        break;
      }
    }
    if (!found) {
      model.addPlace(it->toString(), QUrl(it.key()), "folder-remote");
      next[it.key()] = it.value();
    }
  }
  QSaveFile output(state + "/kde-places.json");
  if (!output.open(QIODevice::WriteOnly))
    return 1;
  output.write(QJsonDocument(next).toJson());
  return output.commit() ? 0 : 1;
}
int main(int argc, char **argv) {
  const bool places = argc == 2 && QByteArray(argv[1]) == "--sync-places";
  if (places)
    qputenv("QT_QPA_PLATFORM", "offscreen");
  QApplication app(argc, argv);
  app.setApplicationName("synctrain-kde");
  app.setOrganizationDomain("gordonbyrnes.com");
  app.setDesktopFileName("com.gordonbyrnes.Synctrain");
  app.setApplicationDisplayName("Synctrain");
  if (places)
    return syncPlaces();
  if (app.arguments().contains("--help")) {
    qInfo("Usage: synctrain-kde [--background | versions <path> | open <path> "
          "| status <path> | pairing-link]");
    return 0;
  }
  KAboutData about("synctrain-kde", "Synctrain", "2.8",
                   "Synchronize folders or open gateway files on demand",
                   KAboutLicense::MPL_V2, "Copyright 2026 Gordon");
  about.setDesktopFileName("com.gordonbyrnes.Synctrain");
  KAboutData::setApplicationData(about);
  app.setDesktopFileName("com.gordonbyrnes.Synctrain");
  app.setWindowIcon(QIcon::fromTheme("com.gordonbyrnes.Synctrain"));
  if (app.arguments().contains("--background"))
    app.setQuitOnLastWindowClosed(false);
  KDBusService service(KDBusService::Unique);
  QQuickStyle::setStyle("org.kde.desktop");
  QQmlApplicationEngine engine;
  Backend backend;
  engine.rootContext()->setContextProperty("backend", &backend);
  engine.addImageProvider("qr", new QRProvider);
  engine.loadFromModule("com.gordonbyrnes.synctrain", "Main");
  if (engine.rootObjects().isEmpty())
    return 1;
  auto *window = qobject_cast<QQuickWindow *>(engine.rootObjects().first());
  const auto show = [window] {
    window->show();
    window->raise();
    window->requestActivate();
  };
  QObject::connect(&backend, &Backend::present, &app, show);
  QObject::connect(&service, &KDBusService::activateRequested, &app,
                   [&backend, show](const QStringList &args, const QString &) {
                     show();
                     backend.arguments(args);
                   });
  QObject::connect(
      &backend, &Backend::preview, &app,
      [&engine](const QString &file, const QString &kind, const QString &text,
                const QString &location, const QString &name) {
        createPreviewWindow(
            &engine, QUrl("qrc:/qt/qml/com/gordonbyrnes/synctrain/qml/Preview.qml"),
            {{"file", file}, {"kind", kind}, {"text", text},
             {"location", location}, {"name", name}});
      });
  if (app.arguments().contains("--background"))
    window->hide();
  else
    show();
  QTimer::singleShot(0, &backend, [&backend, args = app.arguments()] {
    backend.arguments(args);
  });
  return app.exec();
}

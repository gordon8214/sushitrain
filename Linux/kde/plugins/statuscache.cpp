// Copyright (C) 2026 Gordon. SPDX-License-Identifier: MPL-2.0
#include "statuscache.h"
#include <QDateTime>
#include <QDir>
#include <QJsonArray>
#include <QProcess>

StatusCache::StatusCache(QObject *parent) : QObject(parent), client(this) {
  batch.setSingleShot(true);
  batch.setInterval(0);
  connect(&batch, &QTimer::timeout, this, &StatusCache::flush);
  refresh.setInterval(2000);
  connect(&refresh, &QTimer::timeout, this, [this] {
    const auto now = QDateTime::currentMSecsSinceEpoch();
    for (auto it = items.begin(); it != items.end();) {
      if (now - it->seen > 30000) {
        pending.remove(it.key());
        it = items.erase(it);
        continue;
      }
      if (managed(QUrl::fromLocalFile(it.key())))
        pending.insert(it.key());
      ++it;
    }
    flush();
    if (socket.isEmpty() || ++ticks % 5 == 0)
      discover();
  });
  refresh.start();
  QTimer::singleShot(0, this, &StatusCache::discover);
}
QString StatusCache::localPath(const QUrl &url) {
  if (!url.isLocalFile() ||
      (!url.host().isEmpty() && url.host() != "localhost"))
    return {};
  auto path = url.toLocalFile();
  return QDir::isAbsolutePath(path) ? QDir::cleanPath(path) : QString();
}
bool StatusCache::contains(const QString &root, const QString &path) {
  return !root.isEmpty() &&
         (path == root || path.startsWith(root == "/" ? root : root + "/"));
}
bool StatusCache::managed(const QUrl &url) const {
  auto path = localPath(url);
  if (path.isEmpty())
    return false;
  for (const auto &root : roots)
    if (contains(root, path))
      return true;
  return false;
}
QJsonObject StatusCache::status(const QUrl &url) {
  const auto path = localPath(url);
  if (path.isEmpty())
    return {};
  const auto now = QDateTime::currentMSecsSinceEpoch();
  if (items.size() >= 2048 && !items.contains(path)) {
    auto oldest = items.begin();
    for (auto it = items.begin(); it != items.end(); ++it)
      if (it->seen < oldest->seen)
        oldest = it;
    pending.remove(oldest.key());
    items.erase(oldest);
  }
  auto &cached = items[path];
  cached.seen = now;
  if (!managed(url))
    return {};
  if (now - cached.at >= 2000) {
    pending.insert(path);
    if (!batch.isActive())
      batch.start();
  }
  return cached.value;
}
QStringList StatusCache::overlays(const QString &state) {
  if (state == "synced" || state == "kept")
    return {"emblem-success"};
  if (state == "local")
    return {"emblem-checked"};
  if (state == "remote")
    return {"emblem-shared"};
  if (state == "syncing" || state == "scanning" || state == "pending" ||
      state == "partial" || state == "scan-waiting" || state == "sync-waiting")
    return {"emblem-synchronizing-symbolic"};
  if (state == "paused")
    return {"emblem-pause"};
  if (state == "error" || state == "failed" || state == "blocked")
    return {"emblem-error"};
  if (state == "local-only")
    return {"emblem-important"};
  return {};
}
void StatusCache::discover() {
  if (discovering)
    return;
  discovering = true;
  client.helper(
      "desktop.inspect", {}, this,
      [this](QJsonValue v, QString error) {
        discovering = false;
        if (!error.isEmpty())
          return;
        auto host = v.toObject();
        mode = host["Mode"].toString();
        socket = mode == "files" ? host["FilesSocket"].toString()
                                 : host["Check"]
                                       .toObject()["Home"]
                                       .toObject()["socket"]
                                       .toString();
        if (socket.isEmpty())
          return;
        client.call(
            socket, mode == "files" ? "status" : "engine.folders", {}, this,
            [this](QJsonValue v, QString error) {
              if (!error.isEmpty())
                return;
              roots.clear();
              if (mode == "files") {
                auto o = v.toObject();
                for (auto f : o["folders"].toArray())
                  roots << QDir::cleanPath(o["mount"].toString() + "/" +
                                           f.toObject()["dirName"].toString());
              } else
                for (auto f : v.toArray())
                  roots << QDir::cleanPath(f.toObject()["Path"].toString());
              for (const auto &path : items.keys()) {
                const auto url = QUrl::fromLocalFile(path);
                if (managed(url)) {
                  pending.insert(path);
                  items[path].at = 0;
                } else {
                  pending.remove(path);
                  items[path].value = {};
                }
                emit updated(url, items[path].value);
              }
              batch.start();
            },
            2000);
      },
      3000);
}
void StatusCache::flush() {
  if (loading || pending.isEmpty() || socket.isEmpty())
    return;
  loading = true;
  QJsonArray paths;
  while (!pending.isEmpty() && paths.size() < 256) {
    auto it = pending.begin();
    const auto path = *it;
    pending.erase(it);
    if (items.contains(path) && managed(QUrl::fromLocalFile(path)))
      paths.append(path);
  }
  if (paths.isEmpty()) {
    loading = false;
    return;
  }
  const auto endpoint = socket;
  const auto requestedRoots = roots;
  client.call(
      socket, "desktop.items", {{"Paths", paths}}, this,
      [this, paths, endpoint, requestedRoots](QJsonValue v, QString error) {
        loading = false;
        if (socket != endpoint || roots != requestedRoots) {
          for (const auto &path : paths)
            if (managed(QUrl::fromLocalFile(path.toString())))
              pending.insert(path.toString());
          batch.start();
          return;
        }
        const auto now = QDateTime::currentMSecsSinceEpoch();
        if (!error.isEmpty()) {
          for (auto it = items.begin(); it != items.end(); ++it) {
            it->value = QJsonObject{{"Path", it.key()}, {"State", "unknown"}};
            it->at = now;
            emit updated(QUrl::fromLocalFile(it.key()), it->value);
          }
          socket.clear();
          discover();
          return;
        }
        for (auto value : v.toArray()) {
          auto item = value.toObject();
          const auto path = item["Path"].toString();
          if (!items.contains(path))
            continue;
          items[path].value = item;
          items[path].at = now;
          emit updated(QUrl::fromLocalFile(path), item);
        }
        if (!pending.isEmpty())
          batch.start();
      },
      2000);
}
void StatusCache::perform(const QString &operation, const QList<QUrl> &urls,
                          Reply reply) {
  if (urls.isEmpty() || urls.size() > 256) {
    reply({}, "Choose a managed item.");
    return;
  }
  QJsonArray paths;
  for (const auto &url : urls) {
    // Cached roots are only a hint. A cold menu may have received its item
    // capabilities before root discovery; the daemon revalidates every path.
    const auto path = localPath(url);
    if (path.isEmpty() || (!roots.isEmpty() && !managed(url))) {
      reply({}, "Choose items in Synctrain folders.");
      return;
    }
    paths.append(path);
  }
  const auto endpoint = socket;
  const auto requestedMode = mode;
  client.call(
      socket, "desktop.items", {{"Paths", paths}}, this,
      [this, operation, urls, reply, endpoint, requestedMode](QJsonValue v,
                                                              QString error) {
        if (!error.isEmpty()) {
          reply({}, error);
          return;
        }
        if (socket != endpoint || mode != requestedMode) {
          reply(
              {},
              "The service changed. Refresh these items before trying again.");
          return;
        }
        auto values = v.toArray();
        if (values.size() != urls.size()) {
          reply({}, "Refresh these items before trying again.");
          return;
        }
        for (auto value : values) {
          auto item = value.toObject();
          if (!item["Actions"].toArray().contains(operation)) {
            reply({},
                  "This action is no longer available for the selected item.");
            return;
          }
        }
        if (operation == "open" || operation == "status" ||
            operation == "versions") {
          for (auto value : values) {
            const auto path = value.toObject()["Path"].toString();
            if (!QProcess::startDetached(
                    ServiceClient::sibling("synctrain-kde"),
                    {operation, path})) {
              reply({}, "Could not open Synctrain.");
              return;
            }
          }
          reply({}, {});
          return;
        }
        auto remaining = std::make_shared<int>(values.size());
        auto firstError = std::make_shared<QString>();
        for (auto value : values) {
          auto item = value.toObject();
          const bool keep = operation == "keep" || operation == "stop-keeping";
          auto p = keep ? QJsonObject{{"Path", item["Path"]},
                                      {"Keep", operation == "keep"}}
                        : QJsonObject{{"Folder", item["Folder"]},
                                      {"Path", item["RelativePath"]},
                                      {"Action", "rescan"}};
          client.call(
              socket, keep ? "keep" : "engine.action", p, this,
              [this, item, reply, remaining, firstError](QJsonValue v,
                                                         QString error) {
                if (!error.isEmpty() && firstError->isEmpty())
                  *firstError = error;
                const auto path = item["Path"].toString();
                if (items.contains(path))
                  items[path].at = 0;
                pending.insert(path);
                batch.start();
                if (--*remaining == 0)
                  reply(v, *firstError);
              },
              120000);
        }
      },
      2000);
}

// Copyright (C) 2026 Gordon. SPDX-License-Identifier: MPL-2.0
#include "statuscache.h"
#include <KAbstractFileItemActionPlugin>
#include <KFileItemListProperties>
#include <KPluginFactory>
#include <QAction>
#include <QJsonArray>
#include <QMenu>
#include <QPointer>
class SynctrainActions : public KAbstractFileItemActionPlugin {
  Q_OBJECT
public:
  SynctrainActions(QObject *parent, const QVariantList &)
      : KAbstractFileItemActionPlugin(parent) {}
  QList<QAction *> actions(const KFileItemListProperties &files,
                           QWidget *parent) override {
    const auto urls = files.urlList();
    if (urls.isEmpty() || urls.size() > 256)
      return {};
    for (const auto &url : urls) {
      if (StatusCache::localPath(url).isEmpty())
        return {};
      if (!cache.roots.isEmpty() && !cache.managed(url))
        return {};
    }
    auto *menu = new QMenu("Synctrain", parent);
    menu->setIcon(QIcon::fromTheme("com.gordonbyrnes.Synctrain"));
    connect(this, &QObject::destroyed, menu, &QObject::deleteLater);
    menu->addAction("Loading status…")->setEnabled(false);
    QPointer<QMenu> weak(menu);
    auto populate = [this, weak, urls] {
      if (!weak)
        return;
      QJsonArray paths;
      for (const auto &url : urls)
        paths.append(StatusCache::localPath(url));
      cache.client.call(
          cache.socket, "desktop.items", {{"Paths", paths}}, weak,
          [this, weak, urls](QJsonValue value, QString error) {
            if (!weak)
              return;
            weak->clear();
            if (!error.isEmpty() || value.toArray().size() != urls.size()) {
              weak->menuAction()->setVisible(false);
              return;
            }
            const QList<std::pair<QString, QString>> choices = {
                {"open", "Open in Synctrain"},
                {"status", "View Sync Status"},
                {"versions", "Previous Versions…"},
                {"rescan", "Rescan"},
                {"keep", "Keep on This Device"},
                {"stop-keeping", "Stop Keeping on This Device"}};
            for (const auto &[operation, label] : choices) {
              if (operation == "versions" && urls.size() != 1)
                continue;
              bool allowed = true;
              for (auto item : value.toArray())
                allowed &=
                    item.toObject()["Actions"].toArray().contains(operation);
              if (!allowed)
                continue;
              auto *entry = weak->addAction(label);
              connect(
                  entry, &QAction::triggered, this, [this, operation, urls] {
                    cache.perform(operation, urls, [this](auto, QString error) {
                      if (!error.isEmpty())
                        emit this->error(error);
                    });
                  });
            }
            weak->menuAction()->setVisible(!weak->actions().isEmpty());
          },
          2000);
    };
    if (!cache.socket.isEmpty())
      populate();
    else
      cache.client.helper(
          "desktop.inspect", {}, menu,
          [this, weak, populate](QJsonValue value, QString error) {
            if (!weak)
              return;
            if (!error.isEmpty()) {
              weak->menuAction()->setVisible(false);
              return;
            }
            auto host = value.toObject();
            cache.mode = host["Mode"].toString();
            cache.socket = cache.mode == "files"
                               ? host["FilesSocket"].toString()
                               : host["Check"]
                                     .toObject()["Home"]
                                     .toObject()["socket"]
                                     .toString();
            if (cache.socket.isEmpty()) {
              weak->menuAction()->setVisible(false);
              return;
            }
            populate();
          },
          3000);
    return {menu->menuAction()};
  }

private:
  StatusCache cache;
};
K_PLUGIN_CLASS_WITH_JSON(SynctrainActions, "actions.json")
#include "actions.moc"

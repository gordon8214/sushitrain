// Copyright (C) 2026 Gordon. SPDX-License-Identifier: MPL-2.0
#include "backend.h"
#include <KNotification>
#include <QApplication>
#include <QClipboard>
#include <QDateTime>
#include <QDesktopServices>
#include <QFile>
#include <QFileDialog>
#include <QFileInfo>
#include <QJsonDocument>
#include <QLocale>
#include <QMenu>
#include <QStandardPaths>
#include <QTimer>
#include <QUuid>

#include "modelutil.h"
Backend::Backend(QObject *parent) : QObject(parent), client(this) {
  reads = new QObject(this);
  auto *timer = new QTimer(this);
  timer->setInterval(2000);
  connect(timer, &QTimer::timeout, this, [this] {
    if (m_page == "Overview" && m_title == "Overview" && activeReads == 0 &&
        !m_busy && m_dialog.isEmpty())
      refresh();
  });
  timer->start();
  QTimer::singleShot(0, this, &Backend::inspect);
}
void Backend::fail(const QString &error) {
  if (!error.isEmpty()) {
    m_error = error;
    emit changed();
  }
}
QString Backend::action(const QString &label, std::function<void()> fn,
                        QVariantList *target) {
  const auto key = QString::number(++serial);
  actions.insert(key, std::move(fn));
  if (target)
    target->append(QVariantMap{{"label", label}, {"key", key}});
  return key;
}
void Backend::command(const QString &label, std::function<void()> fn) {
  action(label, std::move(fn), &m_commands);
  emit changed();
}
void Backend::row(
    const QString &title, const QString &subtitle,
    const QList<std::pair<QString, std::function<void()>>> &buttons) {
  QVariantList list;
  for (const auto &[name, fn] : buttons)
    action(name, fn, &list);
  m_rows.append(
      QVariantMap{{"title", title}, {"subtitle", subtitle}, {"actions", list}});
  emit changed();
}
void Backend::clear(const QString &title) {
  delete reads;
  activeReads = 0;
  moreKey.clear();
  reads = new QObject(this);
  actions.clear();
  m_rows.clear();
  m_commands.clear();
  m_title = title;
  m_error.clear();
  emit changed();
}
void Backend::call(const QString &method, const QJsonObject &p, Reply reply,
                   bool mutation, int timeout) {
  if (mutation && m_busy)
    return;
  if (!mutation)
    ++activeReads;
  if (mutation) {
    m_busy = true;
    emit changed();
  }
  if (mutation) {
    mutationOwner = new QObject(this);
    if (method == "history.pair" || method == "pair") {
      activePairing = p["ID"].toString();
      activeCancelMethod = method == "pair" ? "pair.cancel" : "history.cancel";
    }
  }
  client.call(
      m_socket, method, p, mutation ? mutationOwner : reads,
      [this, reply, mutation, method](QJsonValue value, QString error) {
        if (!mutation)
          activeReads = qMax(0, activeReads - 1);
        if (mutation) {
          if (mutationOwner)
            mutationOwner->deleteLater();
          mutationOwner = nullptr;
          activePairing.clear();
          m_busy = false;
          emit changed();
        }
        if (!error.isEmpty()) {
          fail(error);
          if (mutation)
            drainArguments();
          return;
        }
        if (mutation && !m_dialog.isEmpty()) {
          m_dialog.clear();
          m_dirty = false;
          acceptDialog = {};
          emit dialogChanged();
        }
        if (method == "pair" || method == "history.pair")
          KNotification::event(KNotification::Notification, "Synctrain",
                               "Gateway pairing completed.");
        else if (method == "engine.restoreVersion" ||
                 method == "engine.restoreDownloaded" ||
                 method == "files.versionRestore" ||
                 method == "files.restoreDownloaded")
          KNotification::event(KNotification::Notification, "Synctrain",
                               "The previous version was restored.");
        reply(value, {});
        if (mutation)
          drainArguments();
      },
      timeout);
}
void Backend::helper(const QString &method, const QJsonObject &p, Reply reply,
                     int timeout) {
  if (m_busy)
    return;
  m_busy = true;
  emit changed();
  client.helper(
      method, p, this,
      [this, reply](QJsonValue result, QString error) {
        m_busy = false;
        emit changed();
        if (!error.isEmpty()) {
          fail(error);
          drainArguments();
          return;
        }
        reply(result, {});
        drainArguments();
      },
      timeout);
}
void Backend::inspect() {
  inspected = false;
  delete initializationOwner;
  initializationOwner = new QObject(this);
  client.helper(
      "desktop.inspect", {}, initializationOwner, [this](QJsonValue value, QString error) {
        if (!error.isEmpty()) {
          fail(error);
          return;
        }
        host = value.toObject();
        m_mode = str(host, "Mode");
        m_socket = m_mode == "files" ? str(host, "FilesSocket")
                                     : host["Check"]
                                           .toObject()["Home"]
                                           .toObject()["socket"]
                                           .toString();
        const auto configured =
            qEnvironmentVariable(m_mode == "files" ? "SYNCTRAIN_FILES_SOCKET"
                                                   : "SYNCTRAIN_GATEWAY_ADMIN");
        if (!configured.isEmpty())
          m_socket = configured;
        if (m_mode.isEmpty()) {
          inspected = true;
          refresh();
          drainArguments();
          return;
        }
        client.call(m_socket, "mcp.preferences.get", {{"Section", "app"}},
               initializationOwner, [this](QJsonValue v, QString error) {
                 auto editor = v.toObject();
                 for (const auto f : editor["Fields"].toArray()) {
                   auto o = f.toObject();
                   preferences.insert(str(o, "Path"), o["Value"]);
                 }
                 client.call(m_socket, "mcp.preferences.close",
                             {{"Token", editor["Token"]}}, this,
                             [](auto, auto) {});
                 grid = preferences["Grid"].toString() == "true";
                 inspected = true;
                 refresh();
                 setupTray();
                 fail(error);
                 drainArguments();
               });
      });
}
void Backend::activate(const QString &key) {
  if (m_busy)
    return;
  const auto fn = actions.value(key);
  if (fn)
    fn();
}
void Backend::fieldChanged() {
  m_dirty = true;
  emit changed();
}
void Backend::closeEditor() {
  if (editorToken.isEmpty())
    return;
  const auto token = editorToken;
  editorToken.clear();
  client.call(m_socket,
              preferenceEditor ? "mcp.preferences.close" : "settings.close",
              {{"Token", token}}, this, [](auto, auto) {});
}
void Backend::form(const QString &title, const QJsonArray &fields,
                   std::function<void(QJsonObject)> done,
                   const QString &accept) {
  if (!m_dialog.isEmpty()) {
    fail("Finish the open dialog first.");
    return;
  }
  QVariantList list;
  for (const auto v : fields) {
    auto f = v.toObject();
    if (!f["Value"].isString() && !f["Value"].isBool())
      f["Value"] = display(f["Value"]);
    list.append(f.toVariantMap());
  }
  m_dialog = {{"title", title}, {"fields", list}, {"accept", accept}};
  acceptDialog = std::move(done);
  m_dirty = false;
  emit dialogChanged();
  emit changed();
}
void Backend::confirm(const QString &title, const QString &text,
                      std::function<void()> done) {
  form(
      title, {field("notice", text, {}, "notice")},
      [this, done](auto) {
        m_dialog.clear();
        m_dirty = false;
        emit dialogChanged();
        done();
      },
      "Continue");
}
void Backend::submit(const QVariantMap &values) {
  if (m_busy || !acceptDialog)
    return;
  auto done = acceptDialog;
  done(QJsonObject::fromVariantMap(values));
}
void Backend::cancelDialog() {
  if (m_busy) {
    if (activePairing.isEmpty())
      return;
    client.call(m_socket, activeCancelMethod, {{"ID", activePairing}}, this,
                [](auto, auto) {});
    delete mutationOwner;
    mutationOwner = nullptr;
    activePairing.clear();
    m_busy = false;
    m_dirty = false;
  }
  if (m_dirty) {
    m_dirty = false;
    m_dialog["discard"] = true;
    emit dialogChanged();
    return;
  }
  closeEditor();
  m_dialog.clear();
  acceptDialog = {};
  emit dialogChanged();
  emit changed();
  drainArguments();
}
void Backend::drainArguments() {
  QTimer::singleShot(0, this, [this] {
    if (inspected && !m_busy && m_dialog.isEmpty() && !queuedArguments.isEmpty()) {
      auto args = queuedArguments;
      queuedArguments.clear();
      arguments(args);
    }
  });
}
void Backend::navigate(const QString &page) {
  if (m_busy || !m_dialog.isEmpty())
    return;
  closeEditor();
  m_page = page;
  refresh();
}
void Backend::refresh() {
  if (m_busy || !m_dialog.isEmpty())
    return;
  clear(m_mode.isEmpty() ? "Set Up Synctrain" : m_page);
  if (m_mode.isEmpty()) {
    row("Synchronize", "Run Syncthing here and synchronize complete folders.",
        {{"Set Up Synchronize", [this] { changeMode("synchronize"); }}});
    row("Files", "Open another gateway’s folders on demand in ~/Synctrain.",
        {{"Set Up Files", [this] { changeMode("files"); }}});
    return;
  }
  if (m_page == "Overview")
    overview();
  else if (m_page == "Folders") {
    if (m_mode == "files")
      filesService();
    else
      folders();
  } else if (m_page == "Devices") {
    if (m_mode == "files")
      filesService();
    else
      devices();
  } else if (m_page == "Files")
    browse();
  else if (m_page == "Settings")
    settings();
  else if (m_page == "Remote Files") {
    if (m_mode == "files")
      filesService();
    else
      remote();
  } else if (m_page == "History Sources")
    historySources();
  else if (m_page == "Pending Changes")
    pending();
}
void Backend::overview() {
  if (m_mode == "files") {
    call("status", {}, [this](QJsonValue v, QString) {
      auto s = v.toObject();
      row("Files mode", str(s, "mount"));
      for (auto f : s["folders"].toArray()) {
        auto o = f.toObject();
        row(str(o, "label"), str(o, "state"));
      }
    });
    call("storage", {}, [this](QJsonValue v, QString) {
      auto o = v.toObject();
      const QLocale locale;
      row("Storage",
          "Cached: " + locale.formattedDataSize(o["used"].toInteger()) +
              " · Kept: " + locale.formattedDataSize(o["kept"].toInteger()) +
              " · Cache budget: " +
              locale.formattedDataSize(o["budget"].toInteger()));
    });
  } else {
    call("engine.status", {}, [this](QJsonValue v, QString) {
      auto o = v.toObject();
      row(str(o, "name"), "Device ID: " + str(o, "deviceID"),
          {{"Copy ID",
            [o] { QApplication::clipboard()->setText(str(o, "deviceID")); }}});
      row("Engine", str(o, "engineVersion") + " · " + str(o, "error"));
      if (o["restartRequired"].toBool())
        row("Restart Required", "Saved settings require an engine restart.",
            {{"Restart", [this] { engineAction("restart"); }}});
    });
    call("engine.folders", {}, [this](QJsonValue v, QString) {
      for (auto f : v.toArray()) {
        auto o = f.toObject();
        row(str(o, "Label"), str(o, "State") + " · " + str(o, "Error"),
            {{"Browse", [this, o] {
                m_folder = str(o, "ID");
                m_directory.clear();
                navigate("Files");
              }}});
      }
    });
    call("engine.stats", {}, [this](QJsonValue v, QString) {
      auto stats = v.toObject();
      auto totals = stats["totals"].toObject();
      const QLocale locale;
      row("Local Files",
          locale.toString(totals["Local"].toObject()["Files"].toInteger()) +
              " files · " +
              locale.formattedDataSize(
                  totals["Local"].toObject()["Bytes"].toInteger()) + " · " +
              locale.toString(totals["LocalNeed"].toObject()["Files"].toInteger()) +
              " files remaining");
      for (const auto &name : QStringList{"Download", "Upload"}) {
        auto progress = stats[name.toLower()].toObject();
        row(name, progress.isEmpty()
                      ? QString("Idle")
                      : locale.formattedDataSize(progress["BytesDone"].toInteger()) +
                            " of " + locale.formattedDataSize(
                                         progress["BytesTotal"].toInteger()));
        auto model = m_rows.takeLast().toMap();
        model["progress"] = qBound(0.0, progress["Percentage"].toDouble(), 1.0);
        m_rows.append(model);
      }
      emit changed();
    });
    call("policy.get", {}, [this](QJsonValue v, QString) {
      const auto reason = v.toObject()["Reason"].toString();
      if (!reason.isEmpty())
        row("Automatic Pause", reason);
    });
    command("Pause All", [this] { engineAction("pauseAll"); });
    command("Resume All", [this] { engineAction("resumeAll"); });
    command("Rescan All", [this] { engineAction("rescanAll"); });
  }
  command("Refresh", [this] { inspect(); });
}
void Backend::engineAction(const QString &operation, const QString &folder,
                           const QString &device, const QString &path) {
  call(
      "engine.action",
      {{"Action", operation},
       {"Folder", folder},
       {"Device", device},
       {"Path", path}},
      [this](auto, auto) { refresh(); }, true, 120000);
}
void Backend::changeMode(const QString &mode) {
  QString explanation =
      mode == "files"
          ? "Synchronization stops. Local folders stay here. Pending edits and "
            "gateway pairings are retained."
          : "Synctrain runs Syncthing here. An existing installation is taken "
            "over with the same identity and a backup of its configuration and "
            "database. Files-mode edits must finish sending first.";
  const auto check = host["Check"].toObject();
  if (mode == "synchronize" && !str(check, "Refusal").isEmpty()) {
    fail(str(check, "Refusal"));
    return;
  }
  if (check["Tray"].toBool())
    explanation += " Turn off Syncthing Tray’s own engine launcher to avoid "
                   "competing services.";
  confirm("Switch to " + mode + "?", explanation, [this, mode] {
    helper(
        "desktop.switch", {{"Mode", mode}},
        [this](QJsonValue v, QString) {
          if (!v.toObject()["Backup"].toString().isEmpty())
            m_error =
                "Previous configuration: " + v.toObject()["Backup"].toString();
          inspect();
        },
        720000);
  });
}
QString Backend::chooseSave(const QString &name) {
  return QFileDialog::getSaveFileName(nullptr, "Save a Copy",
                                      QFileInfo(name).fileName());
}
QString Backend::chooseDirectory(const QString &title) {
  return QFileDialog::getExistingDirectory(nullptr, title);
}
QString Backend::chooseOpen(const QString &title) {
  return QFileDialog::getOpenFileName(nullptr, title);
}
void Backend::arguments(const QStringList &args) {
  if (args.size() < 2)
    return;
  if (args[1] == "--background")
    return;
  emit present();
  if (!inspected || m_mode.isEmpty() || m_busy || !m_dialog.isEmpty()) {
    queuedArguments = args;
    if (inspected && m_mode.isEmpty())
      fail(
          "Choose a mode to continue with the requested file or pairing link.");
    return;
  }
  if (args.size() == 3 &&
      (args[1] == "versions" || args[1] == "open" || args[1] == "status")) {
    openPath(args[2], args[1]);
  } else if (args[1].startsWith("synctrain+pair://")) {
    if (m_mode.isEmpty()) {
      fail("Choose a mode before pairing.");
      return;
    }
    const auto method = m_mode == "files" ? "pair" : "history.pair";
    form(
        "Pairing with a Gateway",
        {field("notice",
               "Approve this machine on the gateway. Cancel stops waiting.", {},
               "notice")},
        [](auto) {}, "Waiting for Approval");
    call(
        method,
        {{"Link", args[1]},
         {"ID", QUuid::createUuid().toString(QUuid::WithoutBraces)}},
        [this](auto, auto) { refresh(); }, true, 660000);
  }
}
void Backend::setupTray() {
  const bool enabled = preferences["Tray"].toString() == "true" ||
                       QApplication::arguments().contains("--background");
  QApplication::setQuitOnLastWindowClosed(!enabled);
  if (!enabled) {
    tray.reset();
    return;
  }
  if (tray) {
    pauseTrayAction->setEnabled(m_mode == "synchronize");
    resumeTrayAction->setEnabled(m_mode == "synchronize");
    return;
  }
  tray = std::make_unique<KStatusNotifierItem>("Synctrain", this);
  tray->setTitle("Synctrain");
  tray->setIconByName("com.gordonbyrnes.Synctrain");
  tray->setStatus(KStatusNotifierItem::Active);
  tray->setStandardActionsEnabled(false);
  auto *menu = new QMenu;
  menu->addAction("Show Synctrain", this, [this] { emit present(); });
  pauseTrayAction = menu->addAction("Pause All", this, [this] {
    if (m_mode == "synchronize")
      engineAction("pauseAll");
  });
  resumeTrayAction = menu->addAction("Resume All", this, [this] {
    if (m_mode == "synchronize")
      engineAction("resumeAll");
  });
  pauseTrayAction->setEnabled(m_mode == "synchronize");
  resumeTrayAction->setEnabled(m_mode == "synchronize");
  menu->addAction("Settings", this, [this] {
    emit present();
    navigate("Settings");
  });
  menu->addAction("Quit and Stop…", this, [this] {
    emit present();
    confirm("Quit and Stop?",
            "Synchronization and on-demand access stop. Pending edits stay "
            "saved here.",
            [this] {
              helper("desktop.stop", {},
                     [](auto, auto) { QApplication::quit(); });
            });
  });
  tray->setContextMenu(menu);
  connect(tray.get(), &KStatusNotifierItem::activateRequested, this,
          [this](bool, const QPoint &) { emit present(); });
}

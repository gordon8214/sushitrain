// Copyright (C) 2026 Gordon. SPDX-License-Identifier: MPL-2.0
#pragma once
#include "serviceclient.h"
#include <KStatusNotifierItem>
#include <QHash>
#include <QJsonArray>
#include <QVariantList>

// QML receives presentation data and opaque action keys; protocol identifiers
// and integer timestamps remain in C++ QJsonValue objects without JS rounding.
class Backend : public QObject {
  Q_OBJECT
  Q_PROPERTY(QString mode READ mode NOTIFY changed)
  Q_PROPERTY(QString title READ title NOTIFY changed)
  Q_PROPERTY(QString page READ page NOTIFY changed)
  Q_PROPERTY(QString error READ error NOTIFY changed)
  Q_PROPERTY(QVariantList rows READ rows NOTIFY changed)
  Q_PROPERTY(QVariantList commands READ commands NOTIFY changed)
  Q_PROPERTY(bool cancelable READ cancelable NOTIFY changed)
  Q_PROPERTY(bool busy READ busy NOTIFY changed)
  Q_PROPERTY(bool grid READ gridView NOTIFY changed)
  Q_PROPERTY(bool dirty READ dirty NOTIFY changed)
  Q_PROPERTY(QVariantMap dialog READ dialog NOTIFY dialogChanged)
public:
  explicit Backend(QObject *parent = nullptr);
  QString mode() const { return m_mode; }
  QString title() const { return m_title; }
  QString page() const { return m_page; }
  QString error() const { return m_error; }
  QVariantList rows() const { return m_rows; }
  QVariantList commands() const { return m_commands; }
  bool busy() const { return m_busy; }
  bool cancelable() const { return !activePairing.isEmpty(); }
  bool gridView() const { return grid; }
  bool dirty() const { return m_dirty; }
  QVariantMap dialog() const { return m_dialog; }
  Q_INVOKABLE void navigate(const QString &page);
  Q_INVOKABLE void activate(const QString &key);
  Q_INVOKABLE void submit(const QVariantMap &values);
  Q_INVOKABLE void cancelDialog();
  Q_INVOKABLE void fieldChanged();
  Q_INVOKABLE void openDefault(const QString &location, const QString &name);
  Q_INVOKABLE void refresh();
  void arguments(const QStringList &args);
  void inspect();
signals:
  void changed();
  void dialogChanged();
  void preview(const QString &file, const QString &kind, const QString &text,
               const QString &location, const QString &name);
  void present();

private:
  ServiceClient client;
  QString m_mode, m_socket, m_title = "Synctrain", m_error, m_page = "Overview",
                            m_folder, m_directory;
  QVariantList m_rows, m_commands;
  QHash<QString, std::function<void()>> actions;
  QJsonObject host, preferences;
  QObject *reads = nullptr;
  QObject *mutationOwner = nullptr;
  QObject *initializationOwner = nullptr;
  QString activePairing, activeCancelMethod;
  QStringList queuedArguments;
  bool inspected = false;
  quint64 serial = 0;
  bool m_busy = false, m_dirty = false;
  QVariantMap m_dialog;
  std::function<void(QJsonObject)> acceptDialog;
  QString editorToken;
  bool preferenceEditor = false;
  bool editorLoading = false;
  QList<qint64> parents;
  bool grid = false;
  qint64 parentItem = 0;
  int nextOffset = 0;
  int activeReads = 0;
  QString moreKey;
  std::unique_ptr<KStatusNotifierItem> tray;
  QAction *pauseTrayAction = nullptr, *resumeTrayAction = nullptr;
  QString action(const QString &label, std::function<void()> fn,
                 QVariantList *target = nullptr);
  void command(const QString &label, std::function<void()> fn);
  void
  row(const QString &title, const QString &subtitle,
      const QList<std::pair<QString, std::function<void()>>> &buttons = {});
  void clear(const QString &title);
  void call(const QString &method, const QJsonObject &p, Reply reply,
            bool mutation = false, int timeout = 15000);
  void helper(const QString &method, const QJsonObject &p, Reply reply,
              int timeout = 15000);
  void fail(const QString &error);
  void form(const QString &title, const QJsonArray &fields,
            std::function<void(QJsonObject)> done,
            const QString &accept = "Save");
  void confirm(const QString &title, const QString &text,
               std::function<void()> done);
  void overview();
  void folders();
  void devices();
  void settings();
  void edit(const QString &section);
  void remote();
  void filesService();
  void historySources();
  void pending();
  void browse(int offset = 0, bool append = false, const QString &search = {});
  void versions(const QJsonObject &entry, const QString &path);
  void remoteVersions(const QString &folder, const QString &path,
                      const QJsonObject &entry, const QString &exclude = {});
  void showPreview(const QString &path, const QString &name, qint64 size);
  void engineAction(const QString &operation, const QString &folder = {},
                    const QString &device = {}, const QString &path = {});
  void chooseFolder();
  void chooseDevice();
  void share(const QJsonObject &folder);
  void openPath(const QString &path, const QString &operation);
  void changeMode(const QString &mode);
  void drainArguments();
  void setupTray();
  void closeEditor();
  void advanced();
  void policy();
  void logs();
  QString chooseSave(const QString &name);
  QString chooseDirectory(const QString &title);
  QString chooseOpen(const QString &title);
};

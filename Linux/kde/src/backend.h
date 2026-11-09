// Copyright (C) 2026 Gordon. SPDX-License-Identifier: MPL-2.0
#pragma once
#include "serviceclient.h"
#include <KStatusNotifierItem>
#include <QAbstractListModel>
#include <QHash>
#include <QJsonArray>
#include <QTimer>
#include <QVariantList>
#include <optional>

// The rows QML shows. Each change is applied as row insertions, removals and
// data changes by row identity, so a page that refreshes itself keeps its
// delegates (focus, open menus, scroll position) when its rows stay.
class RowModel : public QAbstractListModel {
  Q_OBJECT
public:
  using QAbstractListModel::QAbstractListModel;
  int rowCount(const QModelIndex &parent = {}) const override;
  QVariant data(const QModelIndex &index, int role) const override;
  QHash<int, QByteArray> roleNames() const override;
  void sync(const QVariantList &rows);
  // A row's identity: its "id", or its title and section, and which of the
  // rows with those it is
  static QStringList keys(const QVariantList &rows);

private:
  QVariantList current;
  QStringList currentKeys;
};

// What a row shows besides its title, subtitle and actions
struct RowDetail {
  std::optional<double> progress = std::nullopt;
  // A stable identity, for rows whose title changes as they refresh
  QString id = {};
  // Shown in the negative text color
  QString error = {};
  // Orders rows from separate requests within their section
  int order = 0;
  // A section heading instead of a row
  bool heading = false;
};

// QML receives presentation data and opaque action keys; protocol identifiers
// and integer timestamps remain in C++ QJsonValue objects without JS rounding.
class Backend : public QObject {
  Q_OBJECT
  Q_PROPERTY(QString settingsQuery READ settingsQuery WRITE setSettingsQuery NOTIFY settingsSearchChanged)
  Q_PROPERTY(QVariantList settingsResults READ settingsResults NOTIFY settingsSearchChanged)
  Q_PROPERTY(QString settingsSearchError READ settingsSearchError NOTIFY settingsSearchChanged)
  Q_PROPERTY(QString settingsField READ settingsField NOTIFY dialogChanged)
  Q_PROPERTY(QString settingsAnchor READ settingsAnchor NOTIFY dialogChanged)
  Q_PROPERTY(QVariantList navigation READ navigation NOTIFY navigationChanged)
  Q_PROPERTY(QString mode READ mode NOTIFY changed)
  Q_PROPERTY(QString title READ title NOTIFY changed)
  Q_PROPERTY(QString page READ page NOTIFY changed)
  Q_PROPERTY(QString error READ error NOTIFY changed)
  Q_PROPERTY(QVariantList rows READ rows NOTIFY changed)
  Q_PROPERTY(QAbstractItemModel *rowModel READ rowModel CONSTANT)
  // Pages refresh themselves only while the window is on screen
  Q_PROPERTY(bool shown READ shown WRITE setShown NOTIFY shownChanged)
  // Download progress of the file whose information is open
  Q_PROPERTY(QVariantMap dialogStatus READ dialogStatus NOTIFY dialogStatusChanged)
  Q_PROPERTY(QVariantList commands READ commands NOTIFY changed)
  Q_PROPERTY(bool cancelable READ cancelable NOTIFY changed)
  Q_PROPERTY(bool busy READ busy NOTIFY changed)
  Q_PROPERTY(bool grid READ gridView NOTIFY changed)
  Q_PROPERTY(bool dirty READ dirty NOTIFY changed)
  Q_PROPERTY(QVariantMap dialog READ dialog NOTIFY dialogChanged)
public:
  explicit Backend(QObject *parent = nullptr);
  QVariantList navigation() const;
  QString settingsQuery() const { return searchQuery; }
  QString settingsSearchError() const { return searchError; }
  QString settingsField() const { return searchTargetField; }
  QString settingsAnchor() const { return searchTargetAnchor; }
  QVariantList settingsResults() const;
  void setSettingsQuery(const QString &value);
  Q_INVOKABLE void openSettingsResult(const QVariantMap &entry);
  QString mode() const { return m_mode; }
  QString title() const { return m_title; }
  QString page() const { return m_page; }
  QString error() const { return m_error; }
  QVariantList rows() const { return m_rows; }
  QAbstractItemModel *rowModel() { return &model; }
  bool shown() const { return m_shown; }
  void setShown(bool value);
  QVariantMap dialogStatus() const { return m_dialogStatus; }
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
  Q_INVOKABLE void keepEditing();
  Q_INVOKABLE bool requestClose();
  Q_INVOKABLE void fieldChanged();
  Q_INVOKABLE void openDefault(const QString &location, const QString &name);
  Q_INVOKABLE void refresh();
  void arguments(const QStringList &args);
  void inspect();
  // The live page and the open file information ask again now, instead of
  // at their next interval
  void liveRefresh();
  void pollFileStatus();
signals:
  void settingsSearchChanged();
  void changed();
  void navigationChanged();
  void closeReady();
  void dialogChanged();
  void shownChanged();
  void dialogStatusChanged();
  void settingsSaveFailed();
  void preview(const QString &file, const QString &kind, const QString &text,
               const QString &location, const QString &name);
  void present();

private:
  ServiceClient client;
  QString searchQuery, searchTargetAnchor, searchTargetField, searchError;
  QJsonArray searchIndex;
  QJsonObject pendingSearchEntry;
  bool searchLoading = false;
  quint64 searchGeneration = 0;
  void loadSettingsIndex();
  QString m_mode, m_socket, m_title = "Synctrain", m_error, m_page = "Overview",
                            m_folder, m_directory;
  QVariantList m_rows, m_commands, folderShortcuts;
  QHash<QString, std::function<void()>> actions;
  QJsonObject host, preferences, catalog;
  QObject *reads = nullptr;
  QObject *mutationOwner = nullptr;
  QObject *initializationOwner = nullptr;
  QString activePairing, activeCancelMethod, pendingNavigation;
  QStringList queuedArguments;
  bool inspected = false;
  quint64 serial = 0;
  bool m_busy = false, m_dirty = false, closePending = false;
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
      const QList<std::pair<QString, std::function<void()>>> &buttons = {}, const QString &section = {}, const RowDetail &detail = {});
  void toggleRow(const QString &title, const QString &subtitle, bool checked, std::function<void()> toggle);
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
  void buildPage();
  void overview();
  void folders();
  void devices();
  void loadShortcuts();
  void settings();
  void openSetting(const QString &action, const QString &title);
  void historyPage();
  void activityPage();
  // Live pages: a builder that runs again every interval while its page is
  // shown, and whose rows are merged into the page in place
  RowModel model{this};
  bool m_shown = true;
  QTimer liveTimer{this};
  QTimer statusTimer{this};
  std::function<void()> livePage;
  QObject *liveReads = nullptr;
  int liveOutstanding = 0;
  bool liveBuilding = false;
  // Reads made while a page builds; their errors go once a refresh succeeds
  bool buildingPage = false;
  // m_error came from such a read (not from a mutation or a user's own read)
  bool readFailure = false;
  quint64 shortcutsSerial = 0;
  bool shortcutsPending = false;
  struct LiveStaging {
    QVariantList rows, commands;
    QString error;
  } staging;
  void live(std::function<void()> builder, int interval);
  void stopLive();
  void liveCall(const QString &method, const QJsonObject &p, Reply reply,
                int timeout);
  void settleLive();
  void mergeLive();
  bool adoptActions(QVariantMap &fresh, const QVariantMap &old);
  void dropActions(const QVariantMap &row);
  friend struct LiveScope;
  void overviewTransfers();
  void transferList(bool uploads);
  void transferPage(bool uploads);
  void transfersSummary();
  void filesActivity();
  QVariantMap m_dialogStatus;
  struct FileStatus {
    quint64 serial = 0;
    QString folder, path;
    // Here, or left out by the ignore patterns: nothing to wait for
    bool settled = true, pending = false;
  } fileStatus;
  quint64 fileStatusSerial = 0;
  void fileInformation(const QJsonObject &entry, const QString &name);
  void setDialogStatus(const QVariantMap &status);
  void edit(const QString &section);
  void remote(bool revealSettings = false);
  void filesService();
  void historySources();
  void pending();
  struct BrowserSnapshot {
    bool valid = false;
    QString folder, directory, search, title, error, moreKey;
    qint64 parent = 0;
    int next = 0;
    QList<qint64> parents;
    QVariantList rows, commands;
    QHash<QString, std::function<void()>> actions;
  } browserSnapshot;
  QString browserSearch, lastAppPage = "Overview";
  void openFiles(const QString &folder);
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

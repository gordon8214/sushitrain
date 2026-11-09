// Copyright (C) 2026 Gordon. SPDX-License-Identifier: MPL-2.0
// Pages that refresh themselves while they are shown, and the transfer pages.
#include "backend.h"
#include "modelutil.h"
#include <QSet>
#include <QSignalBlocker>

int RowModel::rowCount(const QModelIndex &parent) const {
  return parent.isValid() ? 0 : int(current.size());
}
QVariant RowModel::data(const QModelIndex &index, int role) const {
  if (!index.isValid() || index.row() >= current.size() || role != Qt::UserRole)
    return {};
  return current[index.row()];
}
QHash<int, QByteArray> RowModel::roleNames() const {
  return {{Qt::UserRole, "row"}};
}
QStringList RowModel::keys(const QVariantList &rows) {
  QStringList out;
  QHash<QString, int> seen;
  for (const auto &value : rows) {
    const auto row = value.toMap();
    const auto id = row.value("id").toString();
    const auto key = id.isEmpty() ? "row\x1f" + row.value("title").toString() + '\x1f' +
                                        row.value("section").toString()
                                  : "id\x1f" + id;
    const int repeat = seen[key]++;
    out << (repeat ? key + '\x1e' + QString::number(repeat) : key);
  }
  return out;
}
void RowModel::sync(const QVariantList &rows) {
  const auto next = keys(rows);
  const QSet<QString> wanted(next.begin(), next.end());
  for (qsizetype last = current.size() - 1; last >= 0; --last) {
    if (wanted.contains(currentKeys[last]))
      continue;
    auto first = last;
    while (first > 0 && !wanted.contains(currentKeys[first - 1]))
      --first;
    beginRemoveRows({}, int(first), int(last));
    current.remove(first, last - first + 1);
    currentKeys.remove(first, last - first + 1);
    endRemoveRows();
    last = first;
  }
  // The rows that stay must keep their order; anything else starts over.
  QHash<QString, qsizetype> position;
  for (qsizetype i = 0; i < next.size(); ++i)
    position.insert(next[i], i);
  qsizetype previous = -1;
  for (const auto &key : std::as_const(currentKeys)) {
    if (position.value(key) < previous) {
      beginResetModel();
      current = rows;
      currentKeys = next;
      endResetModel();
      return;
    }
    previous = position.value(key);
  }
  for (qsizetype i = 0; i < next.size(); ++i) {
    if (i < currentKeys.size() && currentKeys[i] == next[i]) {
      if (current[i] != rows[i]) {
        current[i] = rows[i];
        emit dataChanged(index(int(i)), index(int(i)), {Qt::UserRole});
      }
      continue;
    }
    // New rows up to the next row that stays
    auto end = i;
    while (end + 1 < next.size() &&
           (i >= currentKeys.size() || next[end + 1] != currentKeys[i]))
      ++end;
    beginInsertRows({}, int(i), int(end));
    for (auto j = i; j <= end; ++j) {
      current.insert(j, rows[j]);
      currentKeys.insert(j, next[j]);
    }
    endInsertRows();
    i = end;
  }
}

// While a live page builds, its rows and commands go to the staging lists,
// and the window sees nothing until they are merged.
struct LiveScope {
  Backend *backend;
  QSignalBlocker blocker;
  bool nested;
  explicit LiveScope(Backend *b)
      : backend(b), blocker(b), nested(b->liveBuilding) {
    if (nested)
      return;
    backend->liveBuilding = true;
    std::swap(backend->m_rows, backend->staging.rows);
    std::swap(backend->m_commands, backend->staging.commands);
  }
  ~LiveScope() {
    if (nested)
      return;
    std::swap(backend->m_rows, backend->staging.rows);
    std::swap(backend->m_commands, backend->staging.commands);
    backend->liveBuilding = false;
  }
  LiveScope(const LiveScope &) = delete;
  LiveScope &operator=(const LiveScope &) = delete;
};

void Backend::live(std::function<void()> builder, int interval) {
  livePage = std::move(builder);
  liveTimer.setInterval(interval);
  if (m_shown)
    liveTimer.start();
}
void Backend::stopLive() {
  livePage = {};
  liveTimer.stop();
  delete liveReads;
  liveReads = nullptr;
  liveOutstanding = 0;
  staging = {};
}
// While the window is hidden nothing asks: the timers stop, and showing it
// asks at once.
void Backend::setShown(bool value) {
  if (m_shown == value)
    return;
  m_shown = value;
  emit shownChanged();
  if (!m_shown) {
    liveTimer.stop();
    statusTimer.stop();
    return;
  }
  if (livePage)
    liveTimer.start();
  if (fileStatus.serial)
    statusTimer.start();
  liveRefresh();
  pollFileStatus();
}
void Backend::liveRefresh() {
  // A page that is still loading, saving, or editing is left alone.
  if (!livePage || !m_shown || liveOutstanding > 0 || activeReads > 0 ||
      m_busy || !m_dialog.isEmpty() || !editorToken.isEmpty())
    return;
  delete liveReads;
  liveReads = new QObject(this);
  staging = {};
  ++liveOutstanding;
  {
    LiveScope scope(this);
    livePage();
  }
  if (!shortcutsPending)
    loadShortcuts();
  settleLive();
}
void Backend::liveCall(const QString &method, const QJsonObject &p,
                       Reply reply, int timeout) {
  ++liveOutstanding;
  // A slow service delays the next refresh only this long
  timeout = qMin(timeout, 5000);
  client.call(
      m_socket, method, p, liveReads,
      [this, reply](QJsonValue value, QString error) {
        {
          LiveScope scope(this);
          if (error.isEmpty())
            reply(value, {});
          else if (staging.error.isEmpty())
            staging.error = error;
        }
        settleLive();
      },
      timeout);
}
void Backend::settleLive() {
  if (liveOutstanding > 0 && --liveOutstanding == 0)
    mergeLive();
}
// A refreshed row with the same actions keeps the keys QML already holds,
// which now run the new closures.
bool Backend::adoptActions(QVariantMap &fresh, const QVariantMap &old) {
  const auto labels = [](const QVariantMap &row) {
    QStringList out;
    for (const auto &value : row.value("actions").toList())
      out << value.toMap().value("label").toString();
    out << QString::number(row.contains("toggleKey"))
        << QString::number(row.contains("navigationKey"));
    return out;
  };
  if (labels(fresh) != labels(old))
    return false;
  QHash<QString, QString> moved;
  auto list = fresh.value("actions").toList();
  const auto previous = old.value("actions").toList();
  for (qsizetype i = 0; i < list.size(); ++i) {
    auto action = list[i].toMap();
    const auto key = previous[i].toMap().value("key");
    moved.insert(action["key"].toString(), key.toString());
    action["key"] = key;
    list[i] = action;
  }
  if (fresh.contains("actions"))
    fresh["actions"] = list;
  for (const auto *name : {"toggleKey", "navigationKey"})
    if (fresh.contains(name)) {
      moved.insert(fresh[name].toString(), old[name].toString());
      fresh[name] = old[name];
    }
  for (auto it = moved.cbegin(); it != moved.cend(); ++it)
    if (it.key() != it.value())
      actions.insert(it.value(), actions.take(it.key()));
  return true;
}
void Backend::dropActions(const QVariantMap &row) {
  for (const auto &value : row.value("actions").toList())
    actions.remove(value.toMap().value("key").toString());
  for (const auto *name : {"toggleKey", "navigationKey"})
    if (row.contains(name))
      actions.remove(row[name].toString());
}
// A read error shows instead of what the failed refresh built, and leaves
// the page as it was. It doesn't replace an error from a mutation or from a
// read the user started; it goes once a refresh succeeds.
void Backend::mergeLive() {
  if (!staging.error.isEmpty()) {
    for (const auto &row : std::as_const(staging.rows))
      dropActions(row.toMap());
    dropActions({{"actions", staging.commands}});
    if (m_error.isEmpty() || readFailure) {
      m_error = staging.error;
      readFailure = true;
    }
    staging = {};
    emit changed();
    return;
  }
  auto next = std::exchange(staging.rows, {});
  const auto before = RowModel::keys(m_rows), after = RowModel::keys(next);
  QHash<QString, qsizetype> previous;
  for (qsizetype i = 0; i < before.size(); ++i)
    previous.insert(before[i], i);
  QList<bool> kept(m_rows.size(), false);
  for (qsizetype i = 0; i < next.size(); ++i) {
    const auto old = previous.value(after[i], -1);
    if (old < 0)
      continue;
    auto fresh = next[i].toMap();
    const auto row = m_rows[old].toMap();
    if (!adoptActions(fresh, row))
      continue;
    kept[old] = true;
    // An unchanged row stays the same value, which the model skips quickly.
    next[i] = fresh == row ? m_rows[old] : QVariant(fresh);
  }
  for (qsizetype i = 0; i < m_rows.size(); ++i)
    if (!kept[i])
      dropActions(m_rows[i].toMap());
  m_rows = next;
  QVariantMap commands{{"actions", std::exchange(staging.commands, {})}};
  const QVariantMap current{{"actions", m_commands}};
  if (!adoptActions(commands, current))
    dropActions(current);
  m_commands = commands["actions"].toList();
  if (readFailure) {
    m_error.clear();
    readFailure = false;
  }
  staging = {};
  emit changed();
}

void Backend::overviewTransfers() {
  call("engine.devices", {}, [this](QJsonValue devices, QString) {
    qint64 connected = 0, total = 0;
    for (const auto &device : devices.toArray()) {
      ++total;
      connected += device.toObject()["Connected"].toBool();
    }
    call("engine.transfers", {}, [this, connected, total](QJsonValue v, QString) {
      const auto transfers = v.toObject();
      const auto download = transfers["Download"].toObject(),
                 upload = transfers["Upload"].toObject();
      if (!download.isEmpty())
        row("Receiving " + fileCount(download["FilesTotal"].toInteger()) + "…",
            transferTotals(download),
            {{"View Downloads", [this] { transferPage(false); }}}, "summary",
            {.progress = download["Percentage"].toDouble(), .id = "transfers:download"});
      if (!upload.isEmpty())
        row("Sending " + fileCount(upload["FilesTotal"].toInteger()) + "…",
            transferTotals(upload),
            {{"View Uploads", [this] { transferPage(true); }}}, "summary",
            {.progress = upload["Percentage"].toDouble(), .id = "transfers:upload"});
      if (download.isEmpty() && upload.isEmpty()) {
        const QLocale locale;
        row(connected > 0 ? "Connected" : "Not connected",
            total == 0 ? QString("No other devices have been added.")
                       : locale.toString(connected) + " of " + locale.toString(total) +
                             (total == 1 ? " device" : " devices") + " connected",
            {}, "summary", {.id = "transfers:connection"});
      }
    });
  });
}
void Backend::transfersSummary() {
  call("engine.transfers", {}, [this](QJsonValue v, QString) {
    const auto transfers = v.toObject();
    QStringList parts;
    for (const auto &[name, verb] : {std::pair{"Download", "Receiving "}, std::pair{"Upload", "Sending "}}) {
      const auto totals = transfers[name].toObject();
      if (!totals.isEmpty())
        parts << verb + fileCount(totals["FilesTotal"].toInteger()) + " (" +
                     percentage(totals) + ")";
    }
    row("Transfers", parts.isEmpty() ? QString("No files are being received or sent.") : parts.join(" · "),
        {{"View Downloads", [this] { transferPage(false); }},
         {"View Uploads", [this] { transferPage(true); }}},
        {}, {.id = "transfers"});
  });
}
void Backend::transferPage(bool uploads) {
  clear(uploads ? "Uploads" : "Downloads");
  const auto build = [this, uploads] {
    command("Back", [this] { refresh(); });
    transferList(uploads);
  };
  build();
  live(build, 1000);
}
// Downloads in sections by folder, uploads by device, one row a file
void Backend::transferList(bool uploads) {
  call("engine.transfers", {}, [this, uploads](QJsonValue v, QString) {
    const auto files = v.toObject()[uploads ? "Uploads" : "Downloads"].toArray();
    if (files.isEmpty()) {
      row(uploads ? "Not uploading" : "Not downloading",
          uploads ? "Currently no files are being sent to other devices."
                  : "Currently no files are being downloaded from other devices.",
          {}, {}, {.id = "transfers:none"});
      return;
    }
    std::optional<QString> group;
    for (const auto &value : files) {
      const auto file = value.toObject();
      const auto id = str(file, uploads ? "Device" : "Folder");
      if (group != id) {
        group = id;
        const auto name = str(file, uploads ? "DeviceName" : "FolderLabel");
        row(name.isEmpty() ? id : name, {}, {}, {},
            {.id = "section:" + id, .heading = true});
      }
      const auto folder = str(file, "FolderLabel").isEmpty() ? str(file, "Folder") : str(file, "FolderLabel");
      row(uploads ? folder + ": " + str(file, "Path") : str(file, "Path"),
          fileTransfer(file), {}, {},
          {.progress = file["Percentage"].toDouble(),
           .id = QStringList{uploads ? "upload" : "download", str(file, "Device"),
                             str(file, "Folder"), str(file, "Path")}.join('\x1f')});
    }
  });
}
// Files mode: the gateway downloads in progress or interrupted, then the
// edits waiting to be sent
void Backend::filesActivity() {
  row("Transfers", {}, {}, {}, {.id = "section:transfers", .heading = true});
  call("transfers", {}, [this](QJsonValue v, QString) {
    const auto transfers = v.toArray();
    if (transfers.isEmpty())
      row("No recent transfers", {}, {}, {}, {.id = "transfers:none"});
    // As many as the Mac lists
    constexpr qsizetype shown = 100;
    for (qsizetype i = 0; i < qMin(transfers.size(), shown); ++i) {
      const auto t = transfers[i].toObject();
      const auto blocks = t["totalBlocks"].toInteger();
      row(str(t, "path").isEmpty() ? str(t, "id") : str(t, "path"),
          "Download · " + str(t, "state"), {}, {},
          {.progress = blocks > 0 ? std::optional(double(t["verifiedBlocks"].toInteger()) / double(blocks))
                                  : std::nullopt,
           .id = "transfer:" + str(t, "id"), .error = str(t, "error")});
    }
    if (const auto more = transfers.size() - shown; more > 0)
      row(QLocale().toString(more) + (more == 1 ? " more transfer" : " more transfers"), {}, {}, {},
          {.id = "transfers:more"});
  });
  row("Pending Changes", {}, {}, {}, {.id = "section:pending", .order = 1, .heading = true});
  pending();
}

// Synchronize mode's file information follows the file's download.
void Backend::fileInformation(const QJsonObject &entry, const QString &name) {
  const bool available = m_dialog.isEmpty();
  QJsonArray fields{field("notice", name + "\n" + str(entry, "Path") + "\n" + display(entry["Size"]) + " bytes", {}, "notice")};
  const bool follow = m_mode == "synchronize" && !entry["Directory"].toBool();
  if (follow)
    fields.append(field("transferStatus", "Synchronization", {}, "status"));
  form("Information", fields, [this](auto) { cancelDialog(); }, "Done");
  if (!available || !follow)
    return;
  const bool settled = entry["Local"].toBool() || entry["Ignored"].toBool();
  fileStatus = {++fileStatusSerial, m_folder, str(entry, "Path"), settled, false};
  m_dialog["status"] = fileStatus.serial;
  setDialogStatus(settled ? QVariantMap{} : QVariantMap{{"title", "Waiting to synchronize…"}});
  pollFileStatus();
  statusTimer.setInterval(1000);
  if (m_shown)
    statusTimer.start();
}
void Backend::setDialogStatus(const QVariantMap &status) {
  if (status == m_dialogStatus)
    return;
  m_dialogStatus = status;
  emit dialogStatusChanged();
}
void Backend::pollFileStatus() {
  if (!fileStatus.serial || m_dialog.value("status").toULongLong() != fileStatus.serial) {
    fileStatus = {};
    statusTimer.stop();
    setDialogStatus({});
    return;
  }
  if (!m_shown || fileStatus.pending)
    return;
  fileStatus.pending = true;
  const auto serial = fileStatus.serial;
  client.call(m_socket, "engine.transfers", {}, this, [this, serial](QJsonValue v, QString error) {
    if (serial != fileStatus.serial)
      return;
    if (!error.isEmpty()) {
      fileStatus.pending = false;
      return;
    }
    for (const auto &value : v.toObject()["Downloads"].toArray()) {
      const auto file = value.toObject();
      if (str(file, "Folder") != fileStatus.folder || str(file, "Path") != fileStatus.path)
        continue;
      // Once it finishes, whether it is here is asked again.
      fileStatus.settled = false;
      fileStatus.pending = false;
      setDialogStatus({{"title", "Downloading file…"},
                       {"detail", joined({percentage(file), transferBytes(file),
                                          transferPace(file["BytesPerSecond"].toInteger(), fileSecondsRemaining(file))})},
                       {"progress", file["Percentage"].toDouble()}});
      return;
    }
    if (fileStatus.settled) {
      fileStatus.pending = false;
      setDialogStatus({});
      return;
    }
    client.call(m_socket, "engine.inspect", {{"Folder", fileStatus.folder}, {"Path", fileStatus.path}}, this,
                [this, serial](QJsonValue v, QString error) {
                  if (serial != fileStatus.serial)
                    return;
                  fileStatus.pending = false;
                  // A file that can't be looked at any more (deleted, say) isn't waited for.
                  const auto entry = v.toObject()["entry"].toObject();
                  fileStatus.settled = !error.isEmpty() || entry["Local"].toBool() || entry["Ignored"].toBool();
                  setDialogStatus(fileStatus.settled ? QVariantMap{} : QVariantMap{{"title", "Waiting to synchronize…"}});
                }, 5000);
  }, 5000);
}

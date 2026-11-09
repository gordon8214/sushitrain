// Copyright (C) 2026 Gordon. SPDX-License-Identifier: MPL-2.0
#pragma once
#include <QJsonArray>
#include <QJsonObject>
#include <QLocale>
#include <QStringList>
#include <optional>
#include <utility>
inline QJsonObject field(QString path, QString label, QJsonValue value = {},
                         QString kind = "text", QJsonArray options = {}) {
  return {{"Path", path},
          {"Label", label},
          {"Value", value},
          {"Kind", kind},
          {"Options", options}};
}
inline QString str(const QJsonObject &o, const char *name) {
  return o.value(name).toString();
}
inline bool catalogAvailable(const QJsonObject &item, const QString &mode) {
  const auto platforms = item["platforms"].toArray();
  const auto capability = str(item, "capability");
  return (platforms.isEmpty() || platforms.contains("kde")) &&
         (capability.isEmpty() || capability == "always" || capability == mode);
}
inline QString display(const QJsonValue &v) {
  if (v.isString())
    return v.toString();
  if (v.isBool())
    return v.toBool() ? "Yes" : "No";
  if (v.isDouble())
    return QString::number(v.toInteger());
  return {};
}

// The parts of a status line that have something to say
inline QString joined(const QStringList &parts) {
  QStringList present;
  for (const auto &part : parts)
    if (!part.isEmpty())
      present << part;
  return present.join(" · ");
}
inline QString fileCount(qint64 files) {
  return QLocale().toString(files) + (files == 1 ? " file" : " files");
}
// From the bytes, in whole numbers: rounded down, so a transfer that isn't
// done never reads 100%, and an exact share doesn't read one point low
inline QString percentage(qint64 done, qint64 total) {
  const qint64 whole = total > 0 ? qBound<qint64>(0, done, total) * 100 / total : 0;
  return QLocale().toString(whole) + QLocale().percent();
}
inline QString percentage(const QJsonObject &progress) {
  return percentage(progress["BytesDone"].toInteger(), progress["BytesTotal"].toInteger());
}
inline QString transferSpeed(qint64 bytesPerSecond) {
  return QLocale().formattedDataSize(bytesPerSecond) + "/s";
}
// h:mm:ss, as the Mac shows the time a transfer has left
inline QString remainingTime(qint64 seconds) {
  seconds = qMax<qint64>(0, seconds);
  return QString("%1:%2:%3").arg(seconds / 3600).arg(seconds / 60 % 60, 2, 10, QChar('0')).arg(seconds % 60, 2, 10, QChar('0'));
}
// Speed and time left at that speed, or that the speed isn't known yet
inline QString transferPace(qint64 bytesPerSecond, qint64 secondsRemaining) {
  if (bytesPerSecond <= 0)
    return "(Transfer speed unknown)";
  return joined({transferSpeed(bytesPerSecond),
                 secondsRemaining >= 0 ? remainingTime(secondsRemaining) + " remaining" : QString()});
}
inline QString transferBytes(const QJsonObject &progress) {
  const QLocale locale;
  return locale.formattedDataSize(progress["BytesDone"].toInteger()) + " of " +
         locale.formattedDataSize(progress["BytesTotal"].toInteger());
}
// engine.transfers' Download or Upload totals
inline QString transferTotals(const QJsonObject &totals) {
  return joined({percentage(totals), transferBytes(totals),
                 transferPace(totals["BytesPerSecond"].toInteger(), totals["SecondsRemaining"].toInteger(-1))});
}
// One file of engine.transfers' Downloads or Uploads
inline QString fileTransfer(const QJsonObject &file) {
  const auto rate = file["BytesPerSecond"].toInteger();
  return joined({percentage(file), transferBytes(file),
                 rate > 0 ? transferSpeed(rate) : QString()});
}
// Time left for one file at its own speed (-1 when its speed isn't known)
inline qint64 fileSecondsRemaining(const QJsonObject &file) {
  const auto rate = file["BytesPerSecond"].toInteger();
  if (rate <= 0)
    return -1;
  const auto left = qMax<qint64>(0, file["BytesTotal"].toInteger() - file["BytesDone"].toInteger());
  return (left + rate - 1) / rate;
}
// What a syncing folder shows (engine.folders' Sync): how much is left, and
// how much of it is here. Nothing for a folder in any other state.
inline std::optional<std::pair<QString, double>> folderSync(const QJsonObject &folder) {
  const auto sync = folder["Sync"].toObject();
  const auto global = sync["GlobalBytes"].toInteger();
  if (folder["State"].toString() != "syncing" || global <= 0)
    return std::nullopt;
  const auto left = qMax<qint64>(0, global - sync["LocalBytes"].toInteger());
  return std::pair(joined({"Synchronizing…", QLocale().formattedDataSize(left) + " remaining"}),
                   qBound(0.0, sync["Fraction"].toDouble(), 1.0));
}

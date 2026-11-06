// Copyright (C) 2026 Gordon. SPDX-License-Identifier: MPL-2.0
#pragma once
#include <QJsonArray>
#include <QJsonObject>
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

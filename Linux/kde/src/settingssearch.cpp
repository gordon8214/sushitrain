// Copyright (C) 2026 Gordon. SPDX-License-Identifier: MPL-2.0
#include "backend.h"
#include "modelutil.h"
#include <QPointer>
#include <QRegularExpression>
#include <QTimer>
#include <algorithm>
#include <utility>

static QString searchText(const QString &value) {
  QString result;
  for (auto c : value.normalized(QString::NormalizationForm_D))
    if (c.category() != QChar::Mark_NonSpacing) result += c;
  return result.toCaseFolded();
}
void Backend::setSettingsQuery(const QString &value) {
  if (searchQuery == value) return;
  searchQuery = value;
  emit settingsSearchChanged();
}
QVariantList Backend::settingsResults() const {
  QJsonArray entries;
  for (auto item : catalog["settings"].toArray()) {
    auto category = item.toObject();
    for (auto item : category["sections"].toArray()) {
      auto entry = item.toObject();
      if (!catalogAvailable(entry, m_mode)) continue;
      entry["id"] = str(category,"id") + "." + str(entry,"id");
      entry["category"] = category["id"];
      entry["breadcrumb"] = category["title"];
      entries.append(entry);
    }
  }
  for (auto item : catalog["search"].toArray()) {
    auto entry = item.toObject();
    const auto platforms = entry["platforms"].toArray();
    if (str(entry,"section").isEmpty() && (platforms.isEmpty() || platforms.contains("kde"))) entries.append(entry);
  }
  for (auto entry : searchIndex) entries.append(entry);
  QList<std::pair<int,QJsonObject>> matches;
  const auto query = searchText(searchQuery).trimmed();
  if (query.isEmpty()) return {};
  const auto words = query.split(QRegularExpression("\\s+"),Qt::SkipEmptyParts);
  for (auto item : entries) {
    auto entry = item.toObject();
    if (!catalogAvailable(entry, m_mode)) continue;
    const auto title = searchText(str(entry,"title"));
    auto corpus = title + " " + searchText(str(entry,"breadcrumb") + " " + str(entry,"field"));
    for (auto keyword : entry["keywords"].toArray()) corpus += " " + searchText(keyword.toString());
    if (!std::all_of(words.begin(),words.end(),[&](auto word){return corpus.contains(word);})) continue;
    const int score = title==query ? 0 : title.startsWith(query) ? 1 : title.contains(query) ? 2 : 3;
    matches.append({score,entry});
  }
  std::stable_sort(matches.begin(),matches.end(),[](const auto &a,const auto &b){return a.first==b.first ? str(a.second,"id")<str(b.second,"id") : a.first<b.first;});
  QVariantList results;
  for (const auto &match : matches) results.append(match.second.toVariantMap());
  return results;
}
void Backend::loadSettingsIndex() {
  if (m_mode!="synchronize" || searchLoading) return;
  searchLoading=true;
  const auto generation=searchGeneration;
  const auto socket=m_socket;
  const auto mode=m_mode;
  client.call(socket,"settings.index",{},this,[this,socket,mode,generation](QJsonValue data,QString error){
    if(generation!=searchGeneration) return;
    searchLoading=false;
    if (m_socket!=socket || m_mode!=mode) return;
    if (searchError==error && (!error.isEmpty() || searchIndex==data.toArray())) return;
    searchError=error;
    if(error.isEmpty()) searchIndex=data.toArray();
    emit settingsSearchChanged();
  });
}
void Backend::openSettingsResult(const QVariantMap &value) {
  if (m_busy || editorLoading) return;
  const auto entry=QJsonObject::fromVariantMap(value);
  if (!m_dialog.isEmpty()) {
    if (!m_dialog.value("page").toBool()) return;
    pendingSearchEntry=entry;
    cancelDialog();
    return;
  }
  searchTargetAnchor=str(entry,"anchor");
  searchTargetField=str(entry,"field");
  const auto capability=str(entry,"capability");
  if(capability!="always" && capability!=m_mode) {
    clear(str(entry,"title")); command("Back",[this]{refresh();});
    row(str(entry,"title"),"Available in "+capability+" mode. Change the operating mode in General.");
    return;
  }
  const auto category=str(entry,"category");
  if(!category.isEmpty() && m_page!="settings:"+category) navigate("settings:"+category);
  if(!str(entry,"section").isEmpty()) { edit(str(entry,"action"));return; }
  const auto action=str(entry,"action");
  // Selecting a search result opens a setting; destructive or immediate actions
  // remain behind their normal controls and confirmation.
  if(action=="mode" || action=="name" || action=="configuration") {
    navigate("settings:"+str(entry,"category")); return;
  }
  if(action=="remoteService") {clear("Remote File Service");command("Back",[this]{refresh();});remote(true);}
  else if(action=="gateways") {clear("Gateways");command("Back",[this]{refresh();});filesService();}
  else if(action=="historySources") {clear("History Sources");command("Back",[this]{refresh();});historySources();}
  else if(action=="app") edit("app");
  else openSetting(action,str(entry,"title"));
}

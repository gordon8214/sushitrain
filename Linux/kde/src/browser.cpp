// Copyright (C) 2026 Gordon. SPDX-License-Identifier: MPL-2.0
#include "backend.h"
#include "modelutil.h"
#include <QDateTime>
#include <QDesktopServices>
#include <QDir>
#include <QFile>
#include <QFileInfo>
#include <QLocale>
#include <QStringDecoder>
#include <QUrl>

void Backend::browse(int offset, bool append, const QString &search) {
  if (append && activeReads > 0)
    return;
  if (append && !moreKey.isEmpty()) {
    for (auto it = m_commands.begin(); it != m_commands.end();) {
      if (it->toMap()["key"].toString() == moreKey)
        it = m_commands.erase(it);
      else
        ++it;
    }
    actions.remove(moreKey);
    moreKey.clear();
    emit changed();
  }
  if (!append) {
    clear("Files");
  }
  if (m_mode == "synchronize" && m_folder.isEmpty()) {
    folders();
    return;
  }
  if (!append) {
    command("Up", [this] {
      if (m_mode == "files") {
        if (parents.isEmpty())
          parentItem = 0;
        else
          parentItem = parents.takeLast();
      } else {
        m_directory = QFileInfo(m_directory).path();
        if (m_directory == ".")
          m_directory.clear();
      }
      browse();
    });
    command("Search…", [this] {
      form(
          "Search", {field("Name", "File name contains")},
          [this](auto p) {
            m_dialog.clear();
            m_dirty = false;
            emit dialogChanged();
            browse(0, false, p["Name"].toString());
          },
          "Search");
    });
    command("List / Grid", [this] {
      call("mcp.preferences.get", {{"Section", "app"}},
           [this](QJsonValue v, QString) {
             const auto token = v.toObject()["Token"];
             call(
                 "mcp.preferences.save",
                 {{"Token", token},
                  {"Values", QJsonObject{{"Grid", grid ? "false" : "true"}}}},
                 [this](auto, auto) {
                   grid = !grid;
                   preferences["Grid"] = grid ? "true" : "false";
                   emit changed();
                 },
                 true);
           });
    });
    command("New Folder…", [this] {
      form("New Folder", {field("Name", "Folder name")}, [this](auto p) {
        if (m_mode == "files") {
          p["Parent"] = parentItem;
          p["Directory"] = true;
        } else {
          p["Folder"] = m_folder;
          p["Path"] = m_directory;
        }
        call(
            m_mode == "files" ? "files.create" : "engine.mkdir", p,
            [this](auto, auto) { browse(); }, true);
      });
    });
    command("Import…", [this] {
      auto path = chooseOpen("Import File");
      if (path.isEmpty())
        return;
      QJsonObject p{{"Path", path}, {"Parent", parentItem}};
      if (m_mode != "files")
        p = {{"Folder", m_folder},
             {"Path", m_directory},
             {"ID", path},
             {"Name", QFileInfo(path).fileName()}};
      call(
          m_mode == "files" ? "files.import" : "engine.import", p,
          [this](auto, auto) { browse(); }, true, 120000);
    });
  }
  const auto method =
      search.isEmpty()
          ? (m_mode == "files" ? "files.childrenPage" : "engine.browsePage")
          : (m_mode == "files" ? "files.search" : "engine.search");
  QJsonObject p{{"Folder", m_folder},
                {"Path", m_directory},
                {"Parent", parentItem},
                {"Offset", offset},
                {"Name", search}};
  call(method, p, [this, search](QJsonValue v, QString) {
    auto list =
        search.isEmpty() ? v.toObject()["Entries"].toArray() : v.toArray();
    nextOffset = v.toObject()["Next"].toInt();
    for (const auto value : list) {
      auto e = value.toObject();
      auto name = str(e, "Name");
      if (preferences["HideDotfiles"].toString() != "false" &&
          name.startsWith('.'))
        continue;
      const bool directory = m_mode == "files" ? (e["Kind"].toInt() != 0)
                                               : e["Directory"].toBool();
      const auto folder = m_folder;
      QList<std::pair<QString, std::function<void()>>> buttons;
      buttons.append(
          {directory ? "Open Folder" : "Open", [this, e, directory] {
             if (directory) {
               if (m_mode == "files") {
                 parents.append(parentItem);
                 parentItem = e["Item"].toInteger();
               } else
                 m_directory = str(e, "Path");
               browse();
               return;
             }
             if (m_mode == "files") {
               call("files.path", {{"Item", e["Item"]}},
                    [this, e](QJsonValue v, QString) {
                      auto location = v.toString();
                      const auto name = str(e, "Name");
                      const auto ext = QFileInfo(name).suffix().toLower();
                      const bool imageEnabled =
                          preferences["PreviewImages"].toString() != "false";
                      const bool image =
                          QStringList{"png",  "jpg",  "jpeg", "gif",
                                      "webp", "avif", "bmp",  "tiff"}
                              .contains(ext);
                      const bool bounded =
                          (!image || imageEnabled) &&
                          QStringList{"png",  "jpg",  "jpeg", "gif", "webp",
                                      "avif", "bmp",  "tiff", "pdf", "txt",
                                      "md",   "json", "xml",  "go",  "swift",
                                      "c",    "cpp",  "h",    "rs",  "py",
                                      "yaml", "toml", "log",  "csv", "sh",
                                      "css"}
                              .contains(ext);
                      if (!bounded) {
                        showPreview(location, name, e["Size"].toInteger());
                        return;
                      }
                      qint64 limit =
                          ext == "pdf"
                              ? 512LL << 20
                              : (QStringList{"png", "jpg", "jpeg", "gif",
                                             "webp", "avif", "bmp", "tiff"}
                                         .contains(ext)
                                     ? qMax(1, preferences["ImageLimitMiB"]
                                                   .toString()
                                                   .toInt()) *
                                           qint64(1 << 20)
                                     : 1 << 20);
                      call(
                          "files.preview",
                          {{"Item", e["Item"]},
                           {"Node", e["Node"]},
                           {"Limit", limit}},
                          [this, e](QJsonValue v, QString) {
                            showPreview(v.toString(), str(e, "Name"),
                                        e["Size"].toInteger());
                          },
                          false, 120000);
                    });
             } else
               showPreview(str(e, "NativePath").isEmpty()
                               ? str(e, "URL")
                               : str(e, "NativePath"),
                           str(e, "Name"), e["Size"].toInteger());
           }});
      if (!directory)
        buttons.append(
            {"Previous Versions", [this, e] { versions(e, str(e, "Path")); }});
      if (m_mode == "files")
        buttons.append(
            {e["Keep"].toBool() ? "Stop Keeping" : "Keep on This Device",
             [this, e] {
               call(
                   "files.keep",
                   {{"Item", e["Item"]},
                    {"Node", e["Node"]},
                    {"Keep", !e["Keep"].toBool()}},
                   [this](auto, auto) { browse(); }, true, 120000);
             }});
      if (!directory)
        buttons.append({"Save a Copy…", [this, e] {
                          auto path = chooseSave(str(e, "Name"));
                          if (path.isEmpty())
                            return;
                          auto p = m_mode == "files"
                                       ? QJsonObject{{"Item", e["Item"]},
                                                     {"Node", e["Node"]},
                                                     {"Path", path}}
                                       : QJsonObject{{"Folder", m_folder},
                                                     {"Path", e["Path"]},
                                                     {"Name", path},
                                                     {"Token", e["Identity"]}};
                          call(
                              m_mode == "files" ? "files.download"
                                                : "engine.exportCurrent",
                              p, [this](auto, auto) {}, true, 120000);
                        }});
      const bool writable =
          m_mode == "files" ? e["Writable"].toBool() : e["Local"].toBool();
      if (writable) {
        buttons.append(
            {"Rename…", [this, e] {
               form("Rename", {field("Name", "New name", e["Name"])},
                    [this, e](auto p) {
                      QJsonObject r =
                          m_mode == "files"
                              ? QJsonObject{{"Parent", parentItem},
                                            {"NewParent", parentItem},
                                            {"Item", e["Item"]},
                                            {"Node", e["Node"]},
                                            {"NewName", p["Name"]}}
                              : QJsonObject{{"Folder", m_folder},
                                            {"Path", e["Path"]},
                                            {"Name", p["Name"]},
                                            {"Token", e["Identity"]}};
                      call(
                          m_mode == "files" ? "files.rename" : "engine.rename",
                          r, [this](auto, auto) { browse(); }, true);
                    });
             }});
        buttons.append(
            {"Move…", [this, e] {
               form("Move",
                    {field("Path",
                           m_mode == "files"
                               ? "Destination relative to mounted folders"
                               : "Destination relative to this synchronized "
                                 "folder")},
                    [this, e](auto p) {
                      auto r = m_mode == "files"
                                   ? QJsonObject{{"Item", e["Item"]},
                                                 {"Node", e["Node"]},
                                                 {"Path", p["Path"]}}
                                   : QJsonObject{{"Folder", m_folder},
                                                 {"Path", e["Path"]},
                                                 {"Name", p["Path"]},
                                                 {"Token", e["Identity"]}};
                      call(
                          m_mode == "files" ? "files.move" : "engine.move", r,
                          [this](auto, auto) { browse(); }, true);
                    });
             }});
        buttons.append(
            {m_mode == "files" ? "Delete…" : "Move to Trash…", [this, e] {
               confirm(
                   "Remove “" + str(e, "Name") + "”?",
                   m_mode == "files" ? "This also removes it on its gateway."
                                     : "This moves the local item to Trash and "
                                       "synchronizes the deletion.",
                   [this, e] {
                     auto p = m_mode == "files"
                                  ? QJsonObject{{"Parent", parentItem},
                                                {"Item", e["Item"]},
                                                {"Node", e["Node"]}}
                                  : QJsonObject{{"Action", "trashFile"},
                                                {"Folder", m_folder},
                                                {"Path", e["Path"]},
                                                {"Token", e["Identity"]}};
                     call(
                         m_mode == "files" ? "files.delete" : "engine.action",
                         p, [this](auto, auto) { browse(); }, true);
                   });
             }});
      }
      row(name, directory ? "Folder" : display(e["Size"]) + " bytes", buttons);
      if (!directory) {
        const auto index = m_rows.size() - 1;
        auto params = m_mode == "files" ? QJsonObject{{"Item", e["Item"]}}
                                        : QJsonObject{{"Folder", folder},
                                                      {"Path", e["Path"]}};
        // Thumbnail failures leave the native file icon; they do not hide
        // entries.
        client.call(
            m_socket,
            m_mode == "files" ? "files.thumbnail" : "engine.thumbnail", params,
            reads,
            [this, index](QJsonValue v, QString error) {
              if (error.isEmpty() && index < m_rows.size() && v.isString()) {
                auto r = m_rows[index].toMap();
                r["thumbnail"] = "data:image/jpeg;base64," + v.toString();
                m_rows[index] = r;
                emit changed();
              }
            },
            15000);
      }
    }
    if (nextOffset > 0) {
      moreKey = action(
          "Load More", [this, offset = nextOffset] { browse(offset, true); },
          &m_commands);
      emit changed();
    }
    if (m_rows.isEmpty())
      row("No files", "This folder is empty or no files match the search.");
  });
}
void Backend::openPath(const QString &path, const QString &operation) {
  clear(operation == "status" ? "Sync Status" : "Files");
  if (operation == "status") {
    call("desktop.items", {{"Paths", QJsonArray{path}}},
         [this](QJsonValue v, QString) {
           for (auto e : v.toArray()) {
             auto o = e.toObject();
             row(str(o, "Path"), str(o, "State") + " · " + str(o, "Error"));
           }
         });
    return;
  }
  call(m_mode == "files" ? "files.resolvePath" : "engine.resolvePath",
       {{"Path", path}}, [this, path, operation](QJsonValue v, QString) {
         auto e = v.toObject();
         if (m_mode == "files") {
           if (operation == "versions") {
             versions(e, {});
             return;
           }
           if (e["Kind"].toInt() != 0) {
             parents.append(parentItem);
             parentItem = e["Item"].toInteger();
             m_page = "Files";
             browse();
           } else {
             call("files.path", {{"Item", e["Item"]}},
                  [this, e](QJsonValue v, QString) {
                    showPreview(v.toString(), str(e, "Name"),
                                e["Size"].toInteger());
                  });
           }
         } else {
           m_folder = str(e, "Folder");
           m_directory = QFileInfo(str(e, "Path")).path();
           if (m_directory == ".")
             m_directory.clear();
           if (operation == "versions") {
             versions({{"Name", QFileInfo(path).fileName()}}, str(e, "Path"));
             return;
           }
           m_page = "Files";
           browse();
         }
       });
}
void Backend::openDefault(const QString &location, const QString &name) {
  const auto launch = [this](const QString &path) {
    if (!QDesktopServices::openUrl(QUrl::fromLocalFile(path)))
      fail("Could not open the file in its default application.");
  };
  if (QFileInfo(location).isAbsolute()) {
    launch(location);
    return;
  }
  helper("desktop.preview",
         {{"Location", location}, {"Name", name}, {"Limit", qint64(512LL << 20)},
          {"External", true}},
         [launch](QJsonValue value, QString) { launch(value.toString()); },
         120000);
}
void Backend::showPreview(const QString &path, const QString &name,
                          qint64 size) {
  const auto ext = QFileInfo(name).suffix().toLower();
  QString kind;
  if (QStringList{"png", "jpg", "jpeg", "gif", "webp", "avif", "bmp", "tiff"}
          .contains(ext) &&
      preferences["PreviewImages"].toString() != "false")
    kind = "image";
  else if (QStringList{"mp4", "mov", "m4v", "webm", "mkv", "mp3", "m4a", "ogg",
                       "wav", "flac", "opus"}
               .contains(ext) &&
           preferences["PreviewMedia"].toString() != "false")
    kind = "media";
  else if (ext == "pdf")
    kind = "pdf";
  else if (QStringList{"txt", "md", "json", "xml", "go", "swift", "c", "cpp",
                       "h", "rs", "py", "yaml", "toml", "log", "csv", "sh",
                       "css"}
               .contains(ext))
    kind = "text";
  const auto limit =
      kind == "image"
          ? qMax(1, preferences["ImageLimitMiB"].toString().toInt()) *
                qint64(1 << 20)
      : kind == "text" ? 1 << 20
                       : 512LL << 20;
  if (kind.isEmpty()) {
    openDefault(path, name);
    return;
  }
  if (size > limit) {
    fail("This file exceeds the configured preview size limit. Save a Copy or "
         "open it in its default application.");
    return;
  }
  auto done = [this, kind, path, name](QString local) {
    QString text;
    if (kind == "text") {
      QFile file(local);
      if (!file.open(QIODevice::ReadOnly)) {
        fail(file.errorString());
        return;
      }
      auto bytes = file.read((1 << 20) + 1);
      if (bytes.size() > (1 << 20) || !bytes.isValidUtf8()) {
        fail("This text preview is too large.");
        return;
      }
      text = QString::fromUtf8(bytes);
    }
    emit preview(QUrl::fromLocalFile(local).toString(), kind, text, path, name);
  };
  if (kind == "media" && QFileInfo(path).isAbsolute()) {
    done(path);
    return;
  }
  helper(
      "desktop.preview", {{"Location", path}, {"Name", name}, {"Limit", limit}},
      [done](QJsonValue v, QString) { done(v.toString()); }, 120000);
}
void Backend::versions(const QJsonObject &entry, const QString &path) {
  clear("Previous Versions — " + str(entry, "Name"));
  command("Back", [this] { browse(); });
  const auto files = m_mode == "files";
  const auto folder = m_folder;
  auto p = files ? QJsonObject{{"Item", entry["Item"]}}
                 : QJsonObject{{"Folder", folder}, {"Path", path}};
  if (files)
    call("files.historyTarget", p, [this, entry](QJsonValue v, QString) {
      auto t = v.toObject();
      auto target = entry;
      target["Writable"] = t["Writable"];
      remoteVersions(str(t, "Folder"), str(t, "Path"), target,
                     str(t, "ServiceID"));
    });
  else
    remoteVersions(folder, path, entry);
  // Local and remote results are independent requests, so a failed source
  // cannot suppress others.
  client.call(
      m_socket, files ? "files.versions" : "engine.versions", p, reads,
      [this, entry, path, p, files](QJsonValue v, QString error) {
        if (!error.isEmpty()) {
          row(files ? "Folder Gateway" : "This Device", error);
          return;
        }
        auto list = files ? v.toObject()["versions"].toArray() : v.toArray();
        if (list.isEmpty())
          row(files ? "No versions on the folder gateway" : "No local versions",
              "Other paired sources are listed separately.");
        for (auto value : list) {
          auto version = value.toObject();
          auto params = p;
          params[files ? "Version" : "ID"] = version[files ? "id" : "ID"];
          params["Token"] = version[files ? "etag" : "Revision"];
          auto name = str(entry, "Name");
          auto label = files ? str(version, "modTime")
                             : QDateTime::fromMSecsSinceEpoch(
                                   version["Modified"].toInteger())
                                   .toString(Qt::TextDate);
          QList<std::pair<QString, std::function<void()>>> buttons{
              {"Preview",
               [this, params, files, name, version] {
                 call(
                     files ? "files.versionPreview" : "engine.previewVersion",
                     params,
                     [this, name, version](QJsonValue v, QString) {
                       showPreview(v.toString(), name,
                                   version[m_mode == "files" ? "size" : "Size"]
                                       .toInteger());
                     },
                     true, 120000);
               }},
              {"Save a Copy…",
               [this, params, files, name] {
                 auto dest = chooseSave(name);
                 if (dest.isEmpty())
                   return;
                 auto p = params;
                 p[files ? "Path" : "Name"] = dest;
                 call(
                     files ? "files.versionExport" : "engine.exportVersion", p,
                     [this](auto, auto) {}, true, 120000);
               }},
              {"Restore…", [this, params, files, entry, path] {
                 confirm("Restore This Version?",
                         "The destination’s current content is preserved. "
                         "Unsaved local edits prevent replacement.",
                         [this, params, files, entry, path] {
                           call(
                               files ? "files.versionRestore"
                                     : "engine.restoreVersion",
                               params,
                               [this, entry, path](auto, auto) {
                                 versions(entry, path);
                               },
                               true, 120000);
                         });
               }}};
          if (files && !v.toObject()["writable"].toBool())
            buttons.removeLast();
          row(label, files ? "Folder Gateway" : "This Device", buttons);
          if (files && v.toObject()["writable"].toBool())
            row("Restore as Copy", "Create a separate file on the gateway",
                {{"Restore as Copy", [this, params] {
                    call(
                        "files.versionCopy", params, [this](auto, auto) {},
                        true, 120000);
                  }}});
        }
      },
      15000);
}
void Backend::remoteVersions(const QString &folder, const QString &path,
                             const QJsonObject &entry, const QString &exclude) {
  client.call(
      m_socket, "history.list", {{"Folder", folder}, {"Path", path}}, reads,
      [this, folder, path, entry, exclude](QJsonValue v, QString error) {
        if (!error.isEmpty()) {
          row("Other History Sources", error);
          return;
        }
        for (auto value : v.toArray()) {
          auto listing = value.toObject();
          auto source = listing["Source"].toObject();
          auto endpoint = source["Endpoint"].toObject();
          auto id = endpoint["serviceID"];
          if (!exclude.isEmpty() && id.toString() == exclude)
            continue;
          if (!str(listing, "Error").isEmpty()) {
            row(str(source, "Name"), str(listing, "Error"));
            continue;
          }
          if (listing["Versions"].toArray().isEmpty())
            row(str(source, "Name"), "No archived versions");
          for (auto value : listing["Versions"].toArray()) {
            auto version = value.toObject();
            QJsonObject request{{"ID", id},
                                {"Folder", folder},
                                {"Path", path},
                                {"Version", version["id"]},
                                {"ExpectedTag", version["etag"]}};
            const auto name = str(entry, "Name");
            QList<std::pair<QString, std::function<void()>>> buttons{
                {"Preview",
                 [this, request, name, version] {
                   call(
                       "history.download", request,
                       [this, name, version](QJsonValue v, QString) {
                         showPreview(v.toString(), name,
                                     version["size"].toInteger());
                       },
                       true, 120000);
                 }},
                {"Save a Copy…",
                 [this, request, name] {
                   auto dest = chooseSave(name);
                   if (dest.isEmpty())
                     return;
                   auto p = request;
                   p["Destination"] = dest;
                   call("history.export", p, [](auto, auto) {}, true, 120000);
                 }},
                {"Restore…", [this, request, folder, path, entry, version] {
                   confirm(
                       "Restore This Version?",
                       "The destination is checked again after downloading. "
                       "Its current contents are preserved.",
                       [this, request, folder, path, entry, version] {
                         const bool files = m_mode == "files";
                         auto p = files ? QJsonObject{{"Item", entry["Item"]}}
                                        : QJsonObject{{"Folder", folder},
                                                      {"Path", path}};
                         call(
                             files ? "files.captureRestore"
                                   : "engine.captureRestore",
                             p,
                             [this, request, folder, path, entry, version,
                              files](QJsonValue token, QString) {
                               call(
                                   "history.download", request,
                                   [this, folder, path, entry, version, files,
                                    token](QJsonValue local, QString) {
                                     auto p =
                                         files ? QJsonObject{{"Token", token},
                                                             {"Path", local},
                                                             {"Modified",
                                                              version["modifie"
                                                                      "dNs"]}}
                                               : QJsonObject{
                                                     {"Folder", folder},
                                                     {"Path", path},
                                                     {"Name", local},
                                                     {"Token", token},
                                                     {"Offset",
                                                      version["modifiedNs"]}};
                                     call(
                                         files ? "files.restoreDownloaded"
                                               : "engine.restoreDownloaded",
                                         p,
                                         [this, entry, path, files](auto,
                                                                    auto) {
                                           if (files)
                                             call(
                                                 "files.wait",
                                                 {{"Item", entry["Item"]}},
                                                 [this, entry, path](auto,
                                                                     auto) {
                                                   versions(entry, path);
                                                 },
                                                 false, 120000);
                                           else
                                             versions(entry, path);
                                         },
                                         true, 120000);
                                   },
                                   true, 120000);
                             },
                             true);
                       });
                 }}};
            if (m_mode == "files" && !entry["Writable"].toBool())
              buttons.removeLast();
            row(str(source, "Name"),
                QDateTime::fromMSecsSinceEpoch(
                    version["modifiedNs"].toInteger() / 1000000)
                    .toString(Qt::TextDate),
                buttons);
          }
        }
      },
      120000);
}

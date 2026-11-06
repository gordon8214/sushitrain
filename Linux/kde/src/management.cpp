// Copyright (C) 2026 Gordon. SPDX-License-Identifier: MPL-2.0
#include "backend.h"
#include "modelutil.h"
#include <QDesktopServices>
#include <KAboutData>
#include <QStandardPaths>
#include <QJsonDocument>
#include <QPointer>
#include <QProcess>
#include <QUrl>
#include <QUuid>

void Backend::chooseFolder() {
  const auto path = chooseDirectory("Choose a Folder to Synchronize");
  if (path.isEmpty())
    return;
  form("Add Folder",
       {field("ID", "Folder ID",
              QUuid::createUuid().toString(QUuid::WithoutBraces)),
        field("Name", "Label"), field("Path", "Folder path", path),
        field("Action", "Folder type", "sendreceive", "enum",
              {"sendreceive", "sendonly", "receiveonly", "receiveencrypted"})},
       [this](auto p) {
         call("engine.addFolder", p, [this](auto, auto) { refresh(); }, true);
       });
}
void Backend::chooseDevice() {
  form("Add Device", {field("ID", "Syncthing device ID")}, [this](auto p) {
    call("engine.addDevice", p, [this](auto, auto) { refresh(); }, true);
  });
}
void Backend::folders() {
  command("Add Folder…", [this] { chooseFolder(); });
  call("engine.folders", {}, [this](QJsonValue v, QString) {
    for (const auto value : v.toArray()) {
      auto f = value.toObject();
      auto id = str(f, "ID");
      QList<std::pair<QString, std::function<void()>>> buttons = {
          {"Browse",
           [this, f] {
             openFiles(str(f, "ID"));
           }},
          {f["Paused"].toBool() ? "Resume" : "Pause",
           [this, f, id] {
             engineAction(f["Paused"].toBool() ? "resume" : "pause", id);
           }},
          {"Overview", [this, id] { edit("basic/folder/" + id); }},
          {"Synchronization", [this, id] { edit("folder/" + id); }},
          {"Sharing", [this, f] { share(f); }},
          {"Versioning", [this, id] { edit("versioning/" + id); }},
          {"Ignore Patterns",
           [this, id] {
             call("engine.action", {{"Action", "ignores"}, {"Folder", id}},
                  [this, id](QJsonValue v, QString) {
                    QStringList lines;
                    for (auto l : v.toArray())
                      lines << l.toString();
                    form("Ignore Patterns",
                         {field("Patterns", "One pattern per line",
                                lines.join('\n'), "list")},
                         [this, id](auto p) {
                           QJsonArray a;
                           for (auto l : p["Patterns"].toString().split('\n'))
                             a.append(l);
                           call(
                               "engine.action",
                               {{"Action", "saveIgnores"},
                                {"Folder", id},
                                {"Path", p["Patterns"]}},
                               [this](auto, auto) { refresh(); }, true);
                         });
                  });
           }},
          {"Advanced", [this, id] { edit("advanced/folder/" + id); }},
          {"Reset Synchronization Settings…", [this, id] { edit("reset/folder/" + id); }},
          {"Failed Items",
           [this, id] {
             clear("Failed Items");
             command("Back", [this] { refresh(); });
             call("engine.action", {{"Action", "failed"}, {"Folder", id}},
                  [this](QJsonValue v, QString) {
                    if (v.toArray().isEmpty())
                      row("No failed items", {});
                    for (auto e : v.toArray()) {
                      auto o = e.toObject();
                      row(str(o, "Path"), str(o, "Error"));
                    }
                  });
           }},
          {"Deleted Files",
           [this, id] {
             clear("Deleted Files");
             command("Back", [this] { refresh(); });
             call("engine.action", {{"Action", "deleted"}, {"Folder", id}},
                  [this, id](QJsonValue v, QString) {
                    if (v.toArray().isEmpty())
                      row("No archived deleted files", {});
                    for (auto e : v.toArray()) {
                      auto o = e.toObject();
                      row(str(o, "Path"), display(o["Versions"]) + " versions",
                          {{"Restore…", [this, id, o] {
                              confirm(
                                  "Restore Deleted File?",
                                  "The latest archived version is restored.",
                                  [this, id, o] {
                                    engineAction("restoreDeleted", id, {},
                                                 str(o, "Path"));
                                  });
                            }}});
                    }
                  });
           }},
          {"Rescan", [this, id] { engineAction("rescan", id); }},
          {"Open in Dolphin",
           [f] { QProcess::startDetached("dolphin", {str(f, "Path")}); }},
          {"Move Folder…",
           [this, id] {
             auto p = chooseDirectory("Move Folder");
             if (!p.isEmpty())
               engineAction("relocate", id, {}, p);
           }},
          {"Remove…", [this, id] {
             confirm("Remove Folder?",
                     "Synchronization stops. Local files stay here.",
                     [this, id] { engineAction("remove", id); });
           }}};
      if (str(f, "Type") == "sendonly")
        buttons.append(
            {"Override Remote Changes…", [this, id] {
               confirm(
                   "Override Remote Changes?",
                   "Other devices will match this folder, including deletions.",
                   [this, id] { engineAction("override", id); });
             }});
      if (str(f, "Type").startsWith("receive"))
        buttons.append(
            {"Revert Local Changes…", [this, id] {
               confirm(
                   "Revert Local Changes?",
                   "Local changes are replaced by the synchronized versions.",
                   [this, id] { engineAction("revert", id); });
             }});
      row(str(f, "Label").isEmpty() ? id : str(f, "Label"),
          str(f, "Path") + " · " + str(f, "Type") + " · " + str(f, "State") +
              " · " + str(f, "Error"),
          buttons);
    }
    if (v.toArray().isEmpty())
      row("No folders yet", "Add a folder to start synchronizing.");
  });
}
void Backend::share(const QJsonObject &folder) {
  clear("Sharing — " + str(folder, "Label"));
  command("Back", [this] { refresh(); });
  call("engine.devices", {}, [this, folder](QJsonValue v, QString) {
    for (auto e : v.toArray()) {
      auto d = e.toObject();
      bool shared = false;
      for (auto id : folder["Devices"].toArray())
        shared |= id == d["ID"];
      row(str(d, "Name"), str(d, "ID"),
          {{shared ? "Stop Sharing" : "Share…", [this, folder, d, shared] {
              if (shared) {
                call(
                    "engine.share",
                    {{"Folder", folder["ID"]},
                     {"Device", d["ID"]},
                     {"Enabled", false}},
                    [this](auto, auto) { refresh(); }, true);
                return;
              }
              form("Share Folder",
                   {field("Password",
                          "Encryption password (empty for trusted devices)", {},
                          "password")},
                   [this, folder, d](auto p) {
                     p["Folder"] = folder["ID"];
                     p["Device"] = d["ID"];
                     p["Enabled"] = true;
                     call(
                         "engine.share", p, [this](auto, auto) { refresh(); },
                         true);
                   });
            }}});
    }
  });
}
void Backend::devices() {
  command("Add Device…", [this] { chooseDevice(); });
  for (auto method : {"engine.pending", "engine.ignored"})
    call(method, {}, [this, method](QJsonValue v, QString) {
      for (auto e : v.toArray()) {
        auto d = e.toObject();
        const bool pending = QString(method) == "engine.pending";
        row(str(d, "Name"), str(d, "DeviceID"),
            {{pending ? "Add" : "Allow Requests",
              [this, d, pending] {
                call(
                    pending ? "engine.addDevice" : "engine.action",
                    {{"ID", d["DeviceID"]}, {"Action", "unignore"}},
                    [this](auto, auto) { refresh(); }, true);
              }},
             {pending ? "Ignore" : "Dismiss", [this, d, pending] {
                call(
                    "engine.action",
                    {{"ID", d["DeviceID"]},
                     {"Action", pending ? "ignore" : "dismiss"}},
                    [this](auto, auto) { refresh(); }, true);
              }}});
      }
    });
  call("engine.devices", {}, [this](QJsonValue v, QString) {
    for (auto e : v.toArray()) {
      auto d = e.toObject();
      auto id = str(d, "ID");
      row(str(d, "Name"),
          id + " · " +
              (d["Connected"].toBool() ? "Connected" : "Disconnected") + " · " +
              str(d, "Address"),
          {{d["Paused"].toBool() ? "Resume" : "Pause",
            [this, d, id] {
              engineAction(d["Paused"].toBool() ? "resume" : "pause", {}, id);
            }},
           {"Overview", [this, id] { edit("basic/device/" + id); }},
           {"Shared Folders", [this, id] {
             clear("Shared Folders"); command("Back", [this] { refresh(); });
             call("engine.folders", {}, [this, id](QJsonValue v, QString) {
               for (auto entry : v.toArray()) { auto folder = entry.toObject(); if (folder["Devices"].toArray().contains(id)) row(str(folder, "Label"), str(folder, "Path"), {{"Sharing", [this, folder] { share(folder); }}}); }
             });
           }},
           {"Connection Settings", [this, id] { edit("device/" + id); }},
           {"Advanced", [this, id] { edit("advanced/device/" + id); }},
           {"Offered Folders",
            [this, id] {
              clear("Offered Folders");
              command("Back", [this] { refresh(); });
              call("engine.pendingFolders", {{"Device", id}},
                   [this, id](QJsonValue v, QString) {
                     for (auto e : v.toArray()) {
                       auto f = e.toObject();
                       row(str(f, "Label"), str(f, "ID"),
                           {{"Accept…", [this, f, id] {
                               auto path =
                                   chooseDirectory("Choose Folder Location");
                               if (path.isEmpty())
                                 return;
                               call(
                                   "engine.addFolder",
                                   {{"ID", f["ID"]},
                                    {"Name", f["Label"]},
                                    {"Path", path},
                                    {"Enabled", f["Encrypted"]}},
                                   [this, f, id](auto, auto) {
                                     call(
                                         "engine.share",
                                         {{"Folder", f["ID"]},
                                          {"Device", id},
                                          {"Enabled", true}},
                                         [this](auto, auto) { refresh(); },
                                         true);
                                   },
                                   true);
                             }}});
                     }
                   });
            }},
           {"Remove…", [this, id] {
              confirm("Remove Device?",
                      "This device stops sharing folders with this machine.",
                      [this, id] { engineAction("remove", {}, id); });
            }}});
    }
  });
}
void Backend::edit(const QString &section) {
  if (editorLoading || !m_dialog.isEmpty())
    return;
  editorLoading = true;
  const bool isPreference = section == "app" || section == "files";
  const auto get = isPreference ? "mcp.preferences.get" : "settings.get";
  // The editor response must survive navigation so its token can be closed.
  const QPointer<QObject> route(reads);
  const auto endpoint = m_socket;
  client.call(endpoint, get, {{"Section", section}}, this,
              [this, section, route, endpoint, isPreference](QJsonValue v, QString error) {
    editorLoading = false;
    if (!error.isEmpty()) {
      fail(error);
      return;
    }
    auto editor = v.toObject();
    if (!m_dialog.isEmpty() || !route || m_socket != endpoint) {
      client.call(endpoint,
                  isPreference ? "mcp.preferences.close" : "settings.close",
                  {{"Token", editor["Token"]}}, this, [](auto, auto) {});
      return;
    }
    preferenceEditor = isPreference;
    editorToken = str(editor, "Token");
    auto fields = editor["Fields"].toArray();
    if (section.startsWith("advanced/")) fields.prepend(field("expertNotice", "These are expert settings. Incorrect values may damage your folder contents and stop synchronization.", {}, "notice"));
    if (section.startsWith("reset/"))
      fields.prepend(field("resetNotice", "Restore built-in synchronization defaults. Folder location, direction, sharing, ignore patterns, and versioning stay unchanged. Review the settings, then choose Save.", {}, "notice"));
    form(section.startsWith("reset/") ? "Reset Synchronization Settings" : "Settings — " + section, fields,
         [this, token = editorToken, isPreference](QJsonObject values) {
           QJsonObject strings;
           for (auto it = values.constBegin(); it != values.constEnd(); ++it)
             strings.insert(it.key(), it->isBool()
                                          ? (it->toBool() ? "true" : "false")
                                          : it->toString());
           call(
               isPreference ? "mcp.preferences.save" : "settings.save",
               {{"Token", token}, {"Values", strings}},
               [this](QJsonValue result, QString) {
                 editorToken.clear();
                 inspect();
                 if (result.toObject()["restartRequired"].toBool())
                   confirm("Restart Required",
                           "Restart the engine to apply the saved settings?",
                           [this] { engineAction("restart"); });
               },
               true);
         });
  });
}
QVariantList Backend::navigation() const {
  QVariantList list;
  const bool settings = m_page.startsWith("settings:");
  if (settings) list.append(QVariantMap{{"title", "Back to App"}, {"route", lastAppPage}, {"icon", "go-previous"}});
  for (auto value : catalog[settings ? "settings" : "destinations"].toArray()) {
    auto item = value.toObject();
    if (!settings && str(item, "id") == "settings") continue;
    auto route = settings ? "settings:" + str(item, "id") : str(item, "title");
    QVariantMap nav{{"title", item["title"].toString()}, {"route", route}, {"icon", item["icon"].toString()}};
    if (!settings && str(item, "id") == "files") nav["children"] = folderShortcuts;
    list.append(nav);
  }
  return list;
}
void Backend::settings() {
  loadSettingsIndex();
  for (auto value : catalog["settings"].toArray()) {
    auto category = value.toObject();
    if ("settings:" + str(category, "id") != m_page) continue;
    m_title = str(category, "title");
    for (auto item : category["sections"].toArray()) {
      const auto section = item.toObject();
      const auto capability = str(section, "capability");
      if (capability != "always" && capability != m_mode) {
        row(str(section, "title"), "Available in " + capability + " mode. Change the operating mode in General.");
      } else {
        row(str(section, "title"), {}, {{"Configure", [this, section] { openSetting(str(section, "action"), str(section, "title")); }}});
        auto item = m_rows.last().toMap();
        item["navigationKey"] = item["actions"].toList().first().toMap()["key"];
        m_rows.last() = item;
      }
    }
    emit changed();
  }
}
void Backend::openSetting(const QString &action, const QString &title) {
  if (action == "name") {
    form("Device Name", {field("Name", "Name")}, [this](auto p) { p["Action"] = "name"; call("engine.action", p, [this](auto, auto) { refresh(); }, true); });
  } else if (action == "mode") {
    changeMode(m_mode == "files" ? "synchronize" : "files");
  } else if (action == "startup") {
    clear(title); command("Back", [this] { refresh(); });
    const auto enabled = host["Autostart"].toBool();
    toggleRow("Launch at Login", "Start Synctrain when you sign in.", enabled, [this, enabled] {
      helper("desktop.autostart", {{"Enabled", !enabled}}, [this](auto, auto) { inspect(); });
    });
  } else if (action == "presence") {
    clear(title); command("Back", [this] { refresh(); });
    row("Background Service", "Closing the window leaves the service running.", {
      {"Start", [this] { helper("desktop.service", {{"Command", "start"}}, [this](auto, auto) { inspect(); }); }},
      {"Stop", [this] { confirm("Stop Background Service?", "Synchronization or mounted-file access stops.", [this] { helper("desktop.service", {{"Command", "stop"}}, [this](auto, auto) { refresh(); }); }); }}
    });
    row("Tray", "Show or hide the optional tray icon", {{"Configure", [this] { edit("app"); }}});
  } else if (action == "previews" || action == "menuAccess") {
    edit("app");
  } else if (action == "storage") {
    clear(title); command("Back", [this] { refresh(); });
    call("storage", {}, [this](QJsonValue v, QString) {
      auto storage = v.toObject();
      row("Download Cache", "Cached: " + display(storage["used"]) + " bytes • Kept: " + display(storage["kept"]) + " bytes");
      row("Cache Budget", display(storage["budget"]) + " bytes", {{"Change…", [this, storage] {
        form("Cache Budget", {field("bytes", "Bytes", display(storage["budget"]))}, [this](auto p) {
          bool valid = false; auto bytes = p["bytes"].toString().toLongLong(&valid);
          if (!valid || bytes < 100*1024*1024) { fail("Enter at least 100 MiB."); return; }
          call("storage.budget", {{"bytes", bytes}}, [this](auto, auto) { refresh(); }, true);
        });
      }}});
      row("Offline Storage", "Kept files and pending edits are retained independently of the disposable cache.");
    });
  } else if (action == "locations") {
    navigate("Folders");
  } else if (action == "fileManager") {
    clear(title); command("Back", [this] { refresh(); });
    row("Dolphin Integration", "Sync status, offline availability, and Previous Versions are available in Dolphin for Synctrain folders.");
  } else if (action == "mcp") {
    clear(title); command("Back", [this] { refresh(); });
    call("mcp.settings", {}, [this](QJsonValue v, QString) {
      const auto enabled = v.toObject()["Enabled"].toBool();
      toggleRow("Enable MCP", "Allow full management access through MCP.", enabled, [this, enabled] {
        call("mcp.settings", {{"Enabled", !enabled}}, [this](auto, auto) { openSetting("mcp", "MCP"); }, true);
      });
    });
  } else if (action == "gui") {
    clear(title); command("Back", [this] { refresh(); });
    call("engine.status", {}, [this](QJsonValue v, QString) {
      const auto enabled = v.toObject()["guiEnabled"].toBool();
      row("Web GUI and REST API", enabled ? "Enabled" : "Disabled", {{enabled ? "Disable" : "Enable", [this, enabled] {
        call("engine.action", {{"Action", "gui"}, {"Enabled", !enabled}}, [this](auto, auto) {
          refresh(); confirm("Restart Required", "Restart the engine to apply the access change?", [this] { engineAction("restart"); });
        }, true);
      }}});
      row("Authentication and Connection Settings", {}, {{"Configure", [this] { edit("gui"); }}});
    });
  } else if (action == "policy") {
    policy();
  } else if (action == "resources") {
    call("policy.get", {}, [this](QJsonValue v, QString) {
      auto priority = str(v.toObject(), "ResourcePriority");
      form("Resource Priority", {field("Priority", "Priority", priority.isEmpty() ? "normal" : priority, "enum", {"normal", "utility", "background"})}, [this](auto p) { call("resources.set", p, [this](auto, auto) { refresh(); }, true); });
    });
  } else if (action == "configuration") {
    auto path = chooseSave("config.xml");
    if (!path.isEmpty()) call("configuration.export", {{"Path", path}}, [this](auto, auto) { refresh(); }, true);
  } else if (action == "advanced") {
    advanced();
  } else if (action == "logs") {
    logs();
  } else if (action == "versions" || action == "support" || action == "licenses") {
    clear(title); command("Back", [this] { refresh(); });
    if (action == "versions") {
      row("Synctrain", KAboutData::applicationData().version());
      if (m_mode == "synchronize") call("engine.status", {}, [this](QJsonValue v, QString) { row("Syncthing", v.toObject()["engineVersion"].toString()); });
      else row("Engine", "Runs on the gateway in Files mode.");
    } else if (action == "licenses") {
      row("License", "Mozilla Public License 2.0", {{"Read License", [] { QDesktopServices::openUrl(QUrl("https://www.mozilla.org/MPL/2.0/")); }}});
      const auto notices = QStandardPaths::locate(QStandardPaths::GenericDataLocation, "doc/synctrain/GoNotices.html");
      if (!notices.isEmpty()) row("Open-Source Notices", notices, {{"Open", [notices] { QDesktopServices::openUrl(QUrl::fromLocalFile(notices)); }}});
      else row("Open-Source Notices", "Included in the synctrain package documentation (GoNotices.html).");
    } else row("Support and Source", "github.com/gordon8214/syncthing-tools", {{"Open", [] { QDesktopServices::openUrl(QUrl("https://github.com/gordon8214/syncthing-tools")); }}});
  } else {
    edit(action);
  }
}
void Backend::historyPage() {
  row("Previous Versions", "Choose a file, then Previous Versions to view its local and remote archives.", {{"Choose a File", [this] { navigate("Files"); }}});
  if (m_mode == "synchronize") row("Deleted Files", "Choose a synchronized folder to view its archive.", {{"Choose a Folder", [this] { navigate("Folders"); }}});
  historySources();
}
void Backend::activityPage() {
  if (m_mode == "files") { pending(); return; }
  row("Transfers", "Synchronization progress and current activity", {{"View", [this] { navigate("Overview"); }}});
  row("Activity Log", "Events, errors, and diagnostics", {{"Open", [this] { logs(); }}});
  row("Pending Changes and Recovery", "Available in Files mode.");
}
void Backend::policy() {
  call("policy.get", {}, [this](QJsonValue v, QString) {
    auto p = v.toObject();
    form("Automatic Pausing",
         {field("Metered", "On metered networks", p["Metered"], "bool"),
          field("Cellular", "On cellular networks", p["Cellular"], "bool"),
          field("PowerSaver", "In Power Saver", p["PowerSaver"], "bool"),
          field(
              "notice",
              str(p, "Reason") +
                  (!p["NetworkKnown"].toBool() || (p["Cellular"].toBool() &&
                                                   !p["CellularKnown"].toBool())
                       ? " Network status is unavailable."
                       : "") +
                  (!p["PowerKnown"].toBool() ? " Power status is unavailable."
                                             : ""),
              {}, "notice")},
         [this](auto p) {
           call("policy.save", p, [this](auto, auto) { refresh(); }, true);
         });
  });
}
void Backend::advanced() {
  clear("Advanced Configuration");
  command("Back", [this] { refresh(); });
  for (auto s : {"options", "gui", "ldap", "defaults/folder", "defaults/device",
                 "defaults/ignores"})
    row(s, {}, {{"Edit", [this, s] { edit("advanced/" + QString(s)); }}});
  call("engine.folders", {}, [this](QJsonValue v, QString) {
    for (auto e : v.toArray()) {
      auto o = e.toObject();
      row("Folder: " + str(o, "Label"), {},
          {{"Edit", [this, o] { edit("advanced/folder/" + str(o, "ID")); }}});
    }
  });
  call("engine.devices", {}, [this](QJsonValue v, QString) {
    for (auto e : v.toArray()) {
      auto o = e.toObject();
      row("Device: " + str(o, "Name"), {},
          {{"Edit", [this, o] { edit("advanced/device/" + str(o, "ID")); }}});
    }
  });
}
void Backend::logs() {
  clear("Activity Log");
  command("Back", [this] { refresh(); });
  command("Refresh", [this] { logs(); });
  command("Database Maintenance…", [this] {
    confirm("Database Maintenance?", "Run the engine’s database maintenance.",
            [this] { engineAction("maintenance"); });
  });
  command("Diagnostics", [this] {
    call("engine.diagnostics", {}, [this](QJsonValue v, QString) {
      clear("Engine Diagnostics");
      command("Back", [this] { logs(); });
      row("Engine Diagnostics",
          QString::fromUtf8(QJsonDocument(v.toObject()).toJson()));
    });
  });
  command("Logging Levels", [this] {
    call("engine.action", {{"Action", "logFacilities"}},
         [this](QJsonValue v, QString) {
           clear("Logging Levels");
           command("Back", [this] { logs(); });
           command("Reset", [this] { engineAction("resetLogLevels"); });
           for (auto e : v.toArray()) {
             auto o = e.toObject();
             row(str(o, "Name"), display(o["Level"]),
                 {{"Change", [this, o] {
                     form("Logging Level",
                          {field("Level", "Level", "0", "enum",
                                 {"-4", "0", "4", "8"})},
                          [this, o](auto p) {
                            call(
                                "engine.action",
                                {{"Action", "logLevel"},
                                 {"Name", o["Name"]},
                                 {"Offset", p["Level"].toString().toInt()}},
                                [this](auto, auto) { logs(); }, true);
                          });
                   }}});
           }
         });
  });
  call("engine.logs", {}, [this](QJsonValue v, QString) {
    for (auto e : v.toArray()) {
      auto o = e.toObject();
      row(str(o, "Facility"), str(o, "Message"));
    }
  });
}

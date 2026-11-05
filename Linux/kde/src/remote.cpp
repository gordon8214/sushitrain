// Copyright (C) 2026 Gordon. SPDX-License-Identifier: MPL-2.0
#include "backend.h"
#include "modelutil.h"
#include <QApplication>
#include <QClipboard>
#include <QTimer>
#include <QUuid>

void Backend::filesService() {
  const bool foldersPage = m_page == "Folders";
  clear(foldersPage ? "Folders" : "Remote Access");
  if (foldersPage) command("Add Folder…", [this] { navigate("Remote Access"); });
  else command("Pair…", [this] {
    form(
        "Pair with a Gateway", {field("Link", "Pairing link from the gateway")},
        [this](auto p) {
          p["ID"] = QUuid::createUuid().toString(QUuid::WithoutBraces);
          call("pair", p, [this](auto, auto) { refresh(); }, true, 660000);
        },
        "Pair");
  });
  call("status", {}, [this, foldersPage](QJsonValue v, QString) {
    auto s = v.toObject();
    if (!foldersPage && s["gateways"].toArray().isEmpty())
      row("No Gateways", "Pair with a gateway to choose its available folders.");
    if (!foldersPage) for (auto e : s["gateways"].toArray()) {
      auto g = e.toObject();
      row(str(g, "name"), str(g, "state"),
          {{"Add Folders",
            [this, g] {
              clear("Available Folders");
              command("Back", [this] { filesService(); });
              call("folders.remote", {{"ServiceID", g["serviceID"]}},
                   [this, g](QJsonValue v, QString) {
                     for (auto e : v.toArray()) {
                       auto f = e.toObject();
                       row(str(f, "label"),
                           f["write"].toBool() ? "Read and write" : "Read only",
                           {{f["added"].toBool() ? "Already Added" : "Add",
                             [this, g, f] {
                               if (f["added"].toBool())
                                 return;
                               call(
                                   "folders.add",
                                   {{"ServiceID", g["serviceID"]},
                                    {"FolderID", f["id"]}},
                                   [this](auto, auto) { filesService(); },
                                   true);
                             }}});
                     }
                   });
            }},
           {"Forget…", [this, g] {
              confirm("Forget Gateway?",
                      "Remove its mounted folders first. Files remain on the "
                      "gateway.",
                      [this, g] {
                        call(
                            "gateways.forget", {{"ServiceID", g["serviceID"]}},
                            [this](auto, auto) { filesService(); }, true);
                      });
            }}});
    }
    if (foldersPage && s["folders"].toArray().isEmpty())
      row("No Folders", "Choose Add Folder to select a folder from a paired gateway.");
    if (foldersPage) for (auto e : s["folders"].toArray()) {
      auto f = e.toObject();
      row(str(f, "label"), str(f, "gateway") + " · " + str(f, "state"),
          {{"Browse",
            [this, f, s] {
              openPath(str(s, "mount") + "/" + str(f, "dirName"), "open");
            }},
           {"Remove from This Device…", [this, f] {
              confirm("Remove Folder from This Device?",
                      "Pending edits must be sent first. Local-only files are "
                      "moved aside; the gateway’s files stay there.",
                      [this, f] {
                        call(
                            "folders.remove", {{"ID", f["id"]}},
                            [this](QJsonValue v, QString) {
                              auto o = v.toObject();
                              refresh();
                              if (!str(o, "leftovers").isEmpty())
                                row("Local-only files moved to",
                                    str(o, "leftovers"));
                            },
                            true, 120000);
                      });
            }}});
    }
  });

}
void Backend::pending() {
  if (m_mode != "files") {
    row("Pending changes are available in Files mode",
        "Synchronization errors are shown under Folders → Failed Items.");
    return;
  }
  call("pending.list", {}, [this](QJsonValue v, QString) {
    if (v.toArray().isEmpty())
      row("No pending changes", "All edits have reached their gateways.");
    for (auto e : v.toArray()) {
      auto p = e.toObject();
      row(str(p, "path"), str(p, "state") + " · " + str(p, "error"),
          {{"Export…",
            [this, p] {
              auto path = chooseSave(str(p, "path"));
              if (!path.isEmpty())
                call(
                    "pending.export", {{"ID", p["id"]}, {"Dest", path}},
                    [this](auto, auto) { refresh(); }, true);
            }},
           {"Discard…", [this, p] {
              confirm("Discard Pending Change?",
                      "This edit is permanently discarded. The gateway’s "
                      "version is shown instead.",
                      [this, p] {
                        call(
                            "pending.discard", {{"ID", p["id"]}},
                            [this](auto, auto) { refresh(); }, true);
                      });
            }}});
    }
  });
}
void Backend::historySources() {
  command("Pair with a History Source…", [this] {
    form(
        "Pair with a History Source",
        {field("Link", "Pairing link from the gateway")},
        [this](auto p) {
          p["ID"] = QUuid::createUuid().toString(QUuid::WithoutBraces);
          call(
              "history.pair", p, [this](auto, auto) { refresh(); }, true,
              660000);
        },
        "Pair");
  });
  row("Independent History Sources",
      "Sources contribute archived versions by Syncthing folder ID and file "
      "path without mounting their folders.");
  call("history.sources", {}, [this](QJsonValue v, QString) {
    for (auto e : v.toArray()) {
      auto s = e.toObject();
      auto id = s["Endpoint"].toObject()["serviceID"];
      row(str(s, "Name"), s["Enabled"].toBool() ? "Enabled" : "Disabled",
          {{s["Enabled"].toBool() ? "Disable" : "Enable",
            [this, s, id] {
              call(
                  "history.enable",
                  {{"ID", id}, {"Enabled", !s["Enabled"].toBool()}},
                  [this](auto, auto) { refresh(); }, true);
            }},
           {"Forget…", [this, id] {
              confirm("Forget History Source?",
                      "Its archived files stay on the gateway.", [this, id] {
                        call(
                            "history.forget", {{"ID", id}},
                            [this](auto, auto) { refresh(); }, true);
                      });
            }}});
    }
  });
}
void Backend::remote() {
  call("service.status", {}, [this](QJsonValue v, QString) {
    auto s = v.toObject();
    row("Remote File Service",
        s["running"].toBool() ? "Running" : str(s, "error"),
        {{"Configure", [this, s] {
            form("Remote File Service",
                 {field("enabled", "Enable remote file access", s["enabled"],
                        "bool"),
                  field("publicHost", "Host name clients reach",
                        s["publicHost"]),
                  field("publicPort", "Public port", display(s["publicPort"]))},
                 [this, s](auto p) {
                   QJsonObject changed = s;
                   for (auto it = p.constBegin(); it != p.constEnd(); ++it)
                     changed[it.key()] =
                         it.key() == "publicPort"
                             ? QJsonValue(it->toString().toInt())
                             : it.value();
                   call(
                       "service.set", changed,
                       [this](auto, auto) { refresh(); }, true);
                 });
          }}});
    if (!s["enabled"].toBool())
      return;
    call("exports.list", {}, [this](QJsonValue exports, QString) {
      call("engine.folders", {}, [this, exports](QJsonValue v, QString) {
        for (auto e : v.toArray()) {
          auto f = e.toObject();
          QString access = "none";
          for (auto a : exports.toArray()) {
            auto o = a.toObject();
            if (o["folderID"] == f["ID"])
              access = o["allowWrite"].toBool() ? "write" : "read";
          }
          row(str(f, "Label"), "Clients may be granted: " + access,
              {{"Change", [this, f, access] {
                  form("Shared Folder",
                       {field("Access", "Access", access, "enum",
                              {"none", "read", "write"})},
                       [this, f](auto p) {
                         auto access = p["Access"].toString();
                         call(
                             access == "none" ? "exports.remove"
                                              : "exports.set",
                             {{"folderID", f["ID"]},
                              {"allowWrite", access == "write"}},
                             [this](auto, auto) { refresh(); }, true);
                       });
                }}});
        }
      });
    });
    command("Pair a Client…", [this] {
      call(
          "pairing.start", {},
          [this](QJsonValue v, QString) {
            auto session = v.toObject();
            clear("Pair a Client");
            row("Scan or paste this pairing link", str(session, "url"),
                {{"Copy Link", [session] {
                    QApplication::clipboard()->setText(str(session, "url"));
                  }}});
            auto entry = m_rows.takeLast().toMap();
            entry["qr"] = str(session, "url");
            m_rows.append(entry);
            emit changed();
            command("Back", [this] { refresh(); });
            auto *timer = new QTimer(reads);
            timer->setInterval(1500);
            connect(timer, &QTimer::timeout, reads, [this] {
              call("pairing.pending", {}, [this](QJsonValue v, QString) {
                if (v.toArray().isEmpty())
                  return;
                auto request = v.toArray().first().toObject();
                clear("Approve Client");
                row(str(request, "clientName"), str(request, "platform"));
                call("exports.list", {},
                     [this, request](QJsonValue v, QString) {
                       QJsonArray fields;
                       for (auto e : v.toArray()) {
                         auto o = e.toObject();
                         fields.append(
                             field(str(o, "folderID"), str(o, "folderID"),
                                   "none", "enum",
                                   o["allowWrite"].toBool()
                                       ? QJsonArray{"none", "read", "write"}
                                       : QJsonArray{"none", "read"}));
                       }
                       form(
                           "Approve “" + str(request, "clientName") + "”",
                           fields,
                           [this, request](auto grants) {
                             for (const auto &key : grants.keys())
                               if (grants[key].toString() == "none")
                                 grants.remove(key);
                             call(
                                 "pairing.approve",
                                 {{"requestID", request["requestID"]},
                                  {"grants", grants}},
                                 [this](auto, auto) { refresh(); }, true);
                           },
                           "Approve");
                     });
                command("Deny", [this, request] {
                  call(
                      "pairing.deny", {{"requestID", request["requestID"]}},
                      [this](auto, auto) { refresh(); }, true);
                });
              });
            });
            timer->start();
          },
          true);
    });
    call("clients.list", {}, [this](QJsonValue v, QString) {
      for (auto e : v.toArray()) {
        auto c = e.toObject();
        row(str(c, "name"),
            str(c, "platform") + (c["revoked"].toBool() ? " · Revoked" : ""),
            {{"Permissions",
              [this, c] {
                call("exports.list", {}, [this, c](QJsonValue v, QString) {
                  QJsonArray fields;
                  for (auto e : v.toArray()) {
                    auto o = e.toObject();
                    auto id = str(o, "folderID");
                    fields.append(field(
                        id, id,
                        c["permissions"].toObject().value(id).toString("none"),
                        "enum",
                        o["allowWrite"].toBool()
                            ? QJsonArray{"none", "read", "write"}
                            : QJsonArray{"none", "read"}));
                  }
                  form("Client Permissions", fields, [this, c](auto values) {
                    auto *owner = new QObject(this);
                    auto remaining = std::make_shared<int>(values.size());
                    auto errors = std::make_shared<QStringList>();
                    m_busy = true;
                    emit changed();
                    if (values.isEmpty()) {
                      owner->deleteLater();
                      m_busy = false;
                      m_dirty = false;
                      cancelDialog();
                      return;
                    }
                    for (auto it = values.constBegin(); it != values.constEnd();
                         ++it)
                      client.call(
                          m_socket, "clients.permission",
                          {{"clientID", c["clientID"]},
                           {"folderID", it.key()},
                           {"access", it->toString() == "none" ? QJsonValue("")
                                                               : it.value()}},
                          owner,
                          [this, owner, remaining, errors](auto,
                                                           QString error) {
                            if (!error.isEmpty())
                              errors->append(error);
                            if (--*remaining == 0) {
                              owner->deleteLater();
                              m_busy = false;
                              emit changed();
                              if (!errors->isEmpty()) {
                                fail(errors->join("\n"));
                                return;
                              }
                              m_dirty = false;
                              cancelDialog();
                              refresh();
                            }
                          });
                  });
                });
              }},
             {"Revoke…", [this, c] {
                confirm("Revoke Client?",
                        "This client loses access to every folder.", [this, c] {
                          call(
                              "clients.revoke", {{"clientID", c["clientID"]}},
                              [this](auto, auto) { refresh(); }, true);
                        });
              }}});
      }
    });
  });
  call("operations.pending", {}, [this](QJsonValue v, QString) {
    for (auto value : v.toArray()) {
      auto op = value.toObject();
      row("Waiting Remote Operation", str(op, "folderID") + " · " +
                                          str(op, "kind") + " · " +
                                          str(op, "operation"));
    }
  });
  row("Keep Running After Logging Out",
      host["Linger"].toBool() ? "Enabled" : "Disabled",
      {{"Change", [this] {
          form("Background Availability",
               {field("Enabled", "Keep running after logging out",
                      host["Linger"], "bool")},
               [this](auto p) {
                 helper("desktop.linger", p, [this](auto, auto) {
                   m_dialog.clear();
                   emit dialogChanged();
                   inspect();
                 });
               });
        }}});
}

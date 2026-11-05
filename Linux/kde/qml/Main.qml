// Copyright (C) 2026 Gordon. SPDX-License-Identifier: MPL-2.0
import QtQuick
import QtQuick.Controls as QQC2
import QtQuick.Layouts
import org.kde.kirigami as Kirigami

Kirigami.ApplicationWindow {
 id: root
 width: 1100; height: 780; minimumWidth: 640; minimumHeight: 480
 title: backend.title + " — Synctrain"
 onClosing: function(close) {
  if (backend.busy || Object.keys(backend.dialog).length > 0) { close.accepted = false; return }
 }
 globalDrawer: Kirigami.GlobalDrawer {
  title: "Synctrain"; titleIcon: "com.gordonbyrnes.Synctrain"
  isMenu: false; modal: false; collapsible: true
  actions: [
   Kirigami.Action { text: "Overview"; icon.name: "view-dashboard"; onTriggered: backend.navigate(text) },
   Kirigami.Action { text: "Folders"; icon.name: "folder-sync"; onTriggered: backend.navigate(text) },
   Kirigami.Action { text: "Devices"; icon.name: "network-workgroup"; onTriggered: backend.navigate(text) },
   Kirigami.Action { text: "Files"; icon.name: "folder"; onTriggered: backend.navigate(text) },
   Kirigami.Action { text: "Remote Files"; icon.name: "folder-remote"; onTriggered: backend.navigate(text) },
   Kirigami.Action { text: "History Sources"; icon.name: "document-open-recent"; onTriggered: backend.navigate(text) },
   Kirigami.Action { text: "Pending Changes"; icon.name: "document-send"; onTriggered: backend.navigate(text) },
   Kirigami.Action { text: "Settings"; icon.name: "configure"; onTriggered: backend.navigate(text) }
  ]
 }
 pageStack.initialPage: Kirigami.ScrollablePage {
  title: backend.title
  ColumnLayout {
   spacing: Kirigami.Units.largeSpacing
   Kirigami.InlineMessage { Layout.fillWidth: true; visible: backend.error.length > 0; text: backend.error; type: Kirigami.MessageType.Error }
   QQC2.ProgressBar { Layout.fillWidth: true; visible: backend.busy; indeterminate: true; Accessible.name: "Operation in progress" }
   Flow {
    Layout.fillWidth: true; spacing: Kirigami.Units.smallSpacing
    Repeater { model: backend.commands; QQC2.Button { required property var modelData; text: modelData.label; enabled: !backend.busy; onClicked: backend.activate(modelData.key) } }
   }
   GridLayout {
    Layout.fillWidth: true
    columns: backend.grid ? Math.max(1, Math.floor(width / 320)) : 1
    Repeater {
     model: backend.rows
     QQC2.Frame {
      required property var modelData
      Layout.fillWidth: true; Layout.alignment: Qt.AlignTop
      ColumnLayout {
       width: parent.width
       Image { visible: !!modelData.thumbnail; source: modelData.thumbnail || ""; Layout.preferredWidth: 96; Layout.preferredHeight: visible ? 72 : 0; fillMode: Image.PreserveAspectFit; asynchronous: true; Accessible.ignored: true }
       Kirigami.Heading { Layout.fillWidth: true; level: 3; text: modelData.title; wrapMode: Text.Wrap; textFormat: Text.PlainText }
       QQC2.Label { Layout.fillWidth: true; text: modelData.subtitle; wrapMode: Text.Wrap; textFormat: Text.PlainText }
       Image { visible: !!modelData.qr; source: visible ? "image://qr/" + encodeURIComponent(modelData.qr) : ""; Layout.preferredWidth: 300; Layout.preferredHeight: visible ? 300 : 0; Accessible.name: "Pairing QR code" }
       QQC2.ProgressBar { Layout.fillWidth: true; visible: modelData.progress !== undefined; value: modelData.progress || 0; Accessible.name: modelData.title }
       Flow {
        Layout.fillWidth: true; spacing: Kirigami.Units.smallSpacing
        Repeater { model: modelData.actions; QQC2.Button { required property var modelData; text: modelData.label; enabled: !backend.busy; onClicked: backend.activate(modelData.key) } }
       }
      }
     }
    }
   }
  }
 }
 QQC2.Dialog {
  id: editor
  objectName: "settingsEditor"
  parent: root.overlay; anchors.centerIn: parent
  width: Math.min(700, root.width - 40); height: Math.min(650, implicitHeight)
  title: backend.dialog.discard ? "Discard Changes?" : backend.dialog.title || ""
  modal: true; closePolicy: QQC2.Popup.NoAutoClose
  property var draft: ({})
  property var fields: []
  contentItem: QQC2.ScrollView {
   id: formScroll
   clip: true
   contentWidth: availableWidth
   ColumnLayout {
    width: formScroll.availableWidth
    Kirigami.InlineMessage { Layout.fillWidth: true; visible: backend.error.length > 0; text: backend.error; type: Kirigami.MessageType.Error }
    Repeater {
     model: editor.fields
     ColumnLayout {
      required property var modelData
      Layout.fillWidth: true
      QQC2.Label { Layout.fillWidth: true; text: modelData.Label; wrapMode: Text.Wrap; textFormat: Text.PlainText }
      Loader {
       Layout.fillWidth: true
       property var field: parent.modelData
       sourceComponent: field.Kind === "notice" ? notice : field.Kind === "bool" ? check : field.Kind === "enum" ? combo : field.Kind === "list" || field.Kind === "json" ? multiline : singleline
      }
     }
    }
   }
  }
  footer: QQC2.DialogButtonBox {
   QQC2.Button { objectName: "editorCancel"; text: backend.dialog.discard ? "Discard Changes" : "Cancel"; QQC2.DialogButtonBox.buttonRole: QQC2.DialogButtonBox.ActionRole; enabled: !backend.busy || backend.cancelable; onClicked: backend.cancelDialog() }
   QQC2.Button { objectName: "editorSave"; text: backend.dialog.accept || "Save"; QQC2.DialogButtonBox.buttonRole: QQC2.DialogButtonBox.ActionRole; enabled: !backend.busy; onClicked: backend.submit(editor.draft) }
  }
 }
 function update(field, value) { editor.draft[field.Path] = value; backend.fieldChanged() }
 Component { id: notice; Item { implicitHeight: 0 } }
 Component { id: check; QQC2.CheckBox { checked: field.Value === true || field.Value === "true"; enabled: !field.IsReadOnly && !backend.busy; onToggled: root.update(field, checked); Accessible.name: field.Label } }
 Component { id: combo; QQC2.ComboBox { model: field.Options || []; currentIndex: Math.max(0, model.indexOf(field.Value)); enabled: !field.IsReadOnly && !backend.busy; onActivated: root.update(field, currentText); Accessible.name: field.Label } }
 Component { id: singleline; QQC2.TextField { text: field.IsSecret || field.Value === undefined || field.Value === null ? "" : String(field.Value); echoMode: field.IsSecret || field.Kind === "password" ? TextInput.Password : TextInput.Normal; enabled: !field.IsReadOnly && !backend.busy; onTextEdited: root.update(field, text); selectByMouse: true; Accessible.name: field.Label; placeholderText: field.IsSecret ? "Leave empty to keep" : "" } }
 Component { id: multiline; QQC2.TextArea { text: field.Value || ""; enabled: !field.IsReadOnly && !backend.busy; implicitHeight: 130; wrapMode: TextEdit.Wrap; onTextChanged: if (activeFocus) root.update(field, text); Accessible.name: field.Label; selectByMouse: true } }
 Connections {
  target: backend
  function onDialogChanged() {
   if (!backend.dialog.title) { editor.close(); return }
   if (!editor.opened) {
    editor.draft = {}; editor.fields = backend.dialog.fields
    for (let f of editor.fields) if (f.Kind !== "notice") editor.draft[f.Path] = f.IsSecret ? "" : f.Value
    editor.open()
   }
  }
 }
}

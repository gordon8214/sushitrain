// Copyright (C) 2026 Gordon. SPDX-License-Identifier: MPL-2.0
import QtQuick
import QtQuick.Controls as QQC2
import QtQuick.Layouts
import org.kde.kirigami as Kirigami

ColumnLayout {
 id: form
 property var fields: []
 property string targetField: ""
 property string targetAnchor: ""
 signal revealRequested(Item item)
 property bool busy: false
 property bool expanded: false
 // A file's download, which the backend refreshes while the information is open
 property var status: ({})
 readonly property bool hasAdvanced: fields.some(field => field.IsAdvanced === true)
 function reveal(loader) {
  if ((!targetAnchor.length && !targetField.length) || ![targetAnchor.toLowerCase(), targetField.toLowerCase()].includes(String(loader.field.Path).toLowerCase())) return
  if (loader.field.IsAdvanced && !expanded) expansionRequested(true)
  Qt.callLater(() => { if (loader.item) { loader.item.forceActiveFocus(); revealRequested(loader.item) } })
 }
 signal edited(string path, var value)
 signal expansionRequested(bool value)
 spacing: Kirigami.Units.largeSpacing
 Kirigami.FormLayout {
  objectName: "editorForm"
  Layout.fillWidth: true
  Repeater { model: form.fields.filter(field => !field.IsAdvanced); delegate: settingsField }
 }
 QQC2.ToolButton {
  objectName: "editorAdvanced"
  visible: form.hasAdvanced
  text: "Advanced"; checkable: true; checked: form.expanded
  icon.name: checked ? "arrow-down" : "arrow-right"
  onToggled: form.expansionRequested(checked)
  Accessible.description: checked ? "Advanced settings expanded" : "Advanced settings collapsed"
 }
 Kirigami.FormLayout {
  objectName: "editorAdvancedForm"
  Layout.fillWidth: true
  visible: form.expanded && form.hasAdvanced
  Repeater { model: form.fields.filter(field => field.IsAdvanced === true); delegate: settingsField }
 }
 Component {
  id: settingsField
  Loader {
   required property var modelData
   property var field: modelData
   onLoaded: form.reveal(this)
   objectName: "editorField_" + field.Path
   // A file status shows only while there is one, without an empty row.
   visible: field.Kind !== "status" || !!form.status.title
   Layout.fillWidth: true; Layout.minimumWidth: 0
   Kirigami.FormData.label: field.Kind === "bool" || field.Kind === "notice" || field.Kind === "status" ? "" : field.Label + ":"
   Kirigami.FormData.buddyFor: item
   Kirigami.FormData.isSection: field.Kind === "notice"
   sourceComponent: field.Kind === "notice" ? notice : field.Kind === "status" ? transferStatus : field.Kind === "bool" ? check : field.Kind === "enum" ? combo : field.Kind === "list" || field.Kind === "json" ? multiline : singleline
  }
 }
 Component { id: notice; QQC2.Label { text: field.Label; wrapMode: Text.Wrap; textFormat: Text.PlainText } }
 Component {
  id: transferStatus
  ColumnLayout {
   objectName: "fileTransferStatus"
   QQC2.Label { text: form.status.title || ""; wrapMode: Text.Wrap; textFormat: Text.PlainText; Layout.fillWidth: true }
   QQC2.ProgressBar { visible: form.status.progress !== undefined; from: 0; to: 1; value: form.status.progress || 0; Layout.fillWidth: true; Accessible.name: form.status.title || ""; Accessible.description: form.status.detail || "" }
   QQC2.Label { visible: !!form.status.detail; text: form.status.detail || ""; wrapMode: Text.Wrap; textFormat: Text.PlainText; Layout.fillWidth: true }
  }
 }
 Component { id: check; QQC2.CheckBox { text: field.Label; checked: field.Value === true || field.Value === "true"; enabled: !field.IsReadOnly && !form.busy; onToggled: form.edited(field.Path, checked); Accessible.name: field.Label } }
 Component { id: combo; QQC2.ComboBox { model: field.Options || []; currentIndex: Math.max(0, model.indexOf(field.Value)); enabled: !field.IsReadOnly && !form.busy; onActivated: form.edited(field.Path, currentText); Accessible.name: field.Label } }
 Component { id: singleline; QQC2.TextField { text: field.IsSecret || field.Value === undefined || field.Value === null ? "" : String(field.Value); echoMode: field.IsSecret || field.Kind === "password" ? TextInput.Password : TextInput.Normal; enabled: !field.IsReadOnly && !form.busy; onTextEdited: form.edited(field.Path, text); selectByMouse: true; Accessible.name: field.Label; placeholderText: field.IsSecret ? "Leave empty to keep" : "" } }
 Component { id: multiline; QQC2.TextArea { text: field.Value || ""; enabled: !field.IsReadOnly && !form.busy; implicitHeight: 130; wrapMode: TextEdit.Wrap; onTextChanged: if (activeFocus) form.edited(field.Path, text); Accessible.name: field.Label; selectByMouse: true } }
}

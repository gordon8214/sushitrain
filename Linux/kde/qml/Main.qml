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
  if (!backend.requestClose()) close.accepted = false
 }
 globalDrawer: Kirigami.GlobalDrawer {
  objectName: "navigationSidebar"
  isMenu: false; modal: root.width < Kirigami.Units.gridUnit * 45; drawerOpen: !modal
  collapsible: false; collapseButtonVisible: false
  preferredSize: Kirigami.Units.gridUnit * 14
  id: drawer
  footer: QQC2.ToolButton {
   objectName: "settingsNavigation"
   visible: !backend.page.startsWith("settings:")
   text: "Settings"; icon.name: "configure"
   enabled: !backend.busy && Object.keys(backend.dialog).length === 0
   onClicked: backend.navigate("Settings")
  }
 }
 property var pageScrollPositions: ({})
 property string previousPage: "Overview"
 property var navigationObjects: ({})
 function actionFor(item) {
  let action = navigationObjects[item.route]
  if (!action) {
   action = navigationAction.createObject(drawer, { route: item.route })
   navigationObjects[item.route] = action
  }
  action.text = item.title
  action.iconName = item.icon
  action.children = (item.children || []).map(child => actionFor(child))
  return action
 }
 function updateNavigation() {
  let next = backend.navigation.map(item => actionFor(item))
  let wanted = new Set()
  function visit(action) { wanted.add(action.route); for (let child of action.children) visit(child) }
  for (let action of next) visit(action)
  drawer.actions = next
  for (let route of Object.keys(navigationObjects)) {
   if (route.startsWith("folder:") && !wanted.has(route)) {
    navigationObjects[route].destroy()
    delete navigationObjects[route]
   }
  }
 }
 Component.onCompleted: updateNavigation()
 Component {
  id: navigationAction
  Kirigami.Action {
   property string route
   property string iconName
   icon.name: iconName
   checked: backend.page === route
   enabled: !backend.busy && Object.keys(backend.dialog).length === 0
   onTriggered: backend.navigate(route)
  }
 }
 Connections { target: backend; function onNavigationChanged() {
  if (root.pageStack.currentItem && root.pageStack.currentItem.flickable) root.pageScrollPositions[root.previousPage] = root.pageStack.currentItem.flickable.contentY
  root.previousPage = backend.page
  root.updateNavigation()
  Qt.callLater(() => { if (root.pageStack.currentItem && root.pageStack.currentItem.flickable) root.pageStack.currentItem.flickable.contentY = root.pageScrollPositions[backend.page] || 0 })
 } function onCloseReady() { root.close() } }
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
       QQC2.Switch {
        objectName: "immediatePreferenceToggle"
        visible: !!modelData.toggleKey
        text: modelData.title
        checked: modelData.checked === true
        enabled: !backend.busy
        onToggled: {
         if (checked === modelData.checked) return
         const action = modelData.toggleKey
         checked = Qt.binding(() => modelData.checked === true)
         backend.activate(action)
        }
        Accessible.name: modelData.title
       }
       RowLayout {
        Layout.fillWidth: true
        Repeater { model: modelData.actions.slice(0, 2); QQC2.Button { required property var modelData; text: modelData.label; enabled: !backend.busy; onClicked: backend.activate(modelData.key) } }
        QQC2.ToolButton {
         visible: modelData.actions.length > 2
         text: "More"; icon.name: "overflow-menu"
         onClicked: overflow.open()
         QQC2.Menu {
          id: overflow
          Repeater { model: modelData.actions.slice(2); QQC2.MenuItem { required property var modelData; text: modelData.label; enabled: !backend.busy; onTriggered: backend.activate(modelData.key) } }
         }
        }
       }
      }
     }
    }
   }
  }
 }
 Kirigami.Dialog {
  id: editor
  objectName: "settingsEditor"
  parent: root.overlay
  anchors.centerIn: parent
  preferredWidth: Kirigami.Units.gridUnit * 32
  padding: Kirigami.Units.largeSpacing
  title: backend.dialog.discard ? "Discard Changes?" : backend.dialog.title || ""
  modal: true; closePolicy: QQC2.Popup.NoAutoClose
  showCloseButton: false
  property var draft: ({})
  property var fields: []
  property bool hasAdvanced: fields.some(f => f.IsAdvanced === true)
  ColumnLayout {
   spacing: Kirigami.Units.largeSpacing
   Kirigami.InlineMessage { Layout.fillWidth: true; visible: backend.error.length > 0; text: backend.error; type: Kirigami.MessageType.Error }
   Kirigami.FormLayout {
    objectName: "editorForm"
    Layout.fillWidth: true
    Repeater {
     model: editor.fields.filter(f => !f.IsAdvanced)
     delegate: settingsField
    }
   }
   QQC2.ToolButton {
    id: advancedDisclosure
    objectName: "editorAdvanced"
    visible: editor.hasAdvanced
    text: "Advanced"
    checkable: true
    icon.name: checked ? "arrow-down" : "arrow-right"
    Accessible.description: checked ? "Advanced settings expanded" : "Advanced settings collapsed"
   }
   Kirigami.FormLayout {
    objectName: "editorAdvancedForm"
    Layout.fillWidth: true
    visible: advancedDisclosure.checked && editor.hasAdvanced
    Repeater {
     model: editor.fields.filter(f => f.IsAdvanced === true)
     delegate: settingsField
    }
   }
  }
  footer: QQC2.DialogButtonBox {
   padding: Kirigami.Units.largeSpacing
   QQC2.Button { objectName: "editorCancel"; text: backend.dialog.discard ? "Discard Changes" : "Cancel"; QQC2.DialogButtonBox.buttonRole: QQC2.DialogButtonBox.ActionRole; enabled: !backend.busy || backend.cancelable; onClicked: backend.cancelDialog() }
   QQC2.Button { objectName: "editorSave"; text: backend.dialog.discard ? "Keep Editing" : backend.dialog.accept || "Save"; QQC2.DialogButtonBox.buttonRole: QQC2.DialogButtonBox.ActionRole; enabled: !backend.busy; onClicked: backend.dialog.discard ? backend.keepEditing() : backend.submit(editor.draft) }
  }
 }
 Component {
  id: settingsField
  Loader {
   required property var modelData
   property var field: modelData
   objectName: "editorField_" + field.Path
   Layout.fillWidth: true; Layout.minimumWidth: 0
   Kirigami.FormData.label: field.Kind === "bool" || field.Kind === "notice" ? "" : field.Label + ":"
   Kirigami.FormData.buddyFor: item
   Kirigami.FormData.isSection: field.Kind === "notice"
   sourceComponent: field.Kind === "notice" ? notice : field.Kind === "bool" ? check : field.Kind === "enum" ? combo : field.Kind === "list" || field.Kind === "json" ? multiline : singleline
  }
 }
 function update(field, value) { editor.draft[field.Path] = value; backend.fieldChanged() }
 Component { id: notice; QQC2.Label { text: field.Label; wrapMode: Text.Wrap; textFormat: Text.PlainText } }
 Component { id: check; QQC2.CheckBox { text: field.Label; checked: field.Value === true || field.Value === "true"; enabled: !field.IsReadOnly && !backend.busy; onToggled: root.update(field, checked); Accessible.name: field.Label } }
 Component { id: combo; QQC2.ComboBox { model: field.Options || []; currentIndex: Math.max(0, model.indexOf(field.Value)); enabled: !field.IsReadOnly && !backend.busy; onActivated: root.update(field, currentText); Accessible.name: field.Label } }
 Component { id: singleline; QQC2.TextField { text: field.IsSecret || field.Value === undefined || field.Value === null ? "" : String(field.Value); echoMode: field.IsSecret || field.Kind === "password" ? TextInput.Password : TextInput.Normal; enabled: !field.IsReadOnly && !backend.busy; onTextEdited: root.update(field, text); selectByMouse: true; Accessible.name: field.Label; placeholderText: field.IsSecret ? "Leave empty to keep" : "" } }
 Component { id: multiline; QQC2.TextArea { text: field.Value || ""; enabled: !field.IsReadOnly && !backend.busy; implicitHeight: 130; wrapMode: TextEdit.Wrap; onTextChanged: if (activeFocus) root.update(field, text); Accessible.name: field.Label; selectByMouse: true } }
 Connections {
  target: backend
  function onSettingsSaveFailed() {
   if (editor.opened && editor.hasAdvanced) advancedDisclosure.checked = true
  }
  function onDialogChanged() {
   if (!backend.dialog.title) { editor.close(); return }
   if (!editor.opened) {
    advancedDisclosure.checked = false
    editor.draft = {}; editor.fields = backend.dialog.fields
    for (let f of editor.fields) if (f.Kind !== "notice") editor.draft[f.Path] = f.IsSecret ? "" : f.Value
    editor.open()
   }
  }
 }
}

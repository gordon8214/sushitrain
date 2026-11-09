// Copyright (C) 2026 Gordon. SPDX-License-Identifier: MPL-2.0
import QtQuick
import QtQuick.Controls as QQC2
import QtQuick.Layouts
import org.kde.kirigami as Kirigami

Kirigami.ApplicationWindow {
 id: root
 width: 1100; height: 780; minimumWidth: 640; minimumHeight: 480
 title: (backend.dialog.page ? backend.dialog.title : backend.title) + " — Synctrain"
 onClosing: function(close) {
  if (!backend.requestClose()) close.accepted = false
 }
 globalDrawer: Kirigami.GlobalDrawer {
  objectName: "navigationSidebar"
  isMenu: false; modal: root.width < Kirigami.Units.gridUnit * 45; drawerOpen: !modal
  collapsible: false; collapseButtonVisible: false
  preferredSize: Kirigami.Units.gridUnit * 14
  id: drawer
  header: ColumnLayout {
   visible: true
   spacing: Kirigami.Units.smallSpacing
   Kirigami.SearchField {
    id: settingsSearch
    objectName: "settingsSearch"
    Layout.fillWidth: true
    placeholderText: "Search Settings"
    autoAccept: false
    onAccepted: { if (backend.settingsResults.length > 0) backend.openSettingsResult(backend.settingsResults[0]) }
    text: backend.settingsQuery
    onTextChanged: backend.settingsQuery = text
    Keys.onEscapePressed: backend.settingsQuery = ""
    Keys.onDownPressed: settingsResults.forceActiveFocus()
   }
   QQC2.Label { visible: backend.settingsSearchError.length > 0; text: backend.settingsSearchError; wrapMode: Text.Wrap; Layout.fillWidth: true }
   QQC2.Label { visible: settingsSearch.text.trim().length > 0 && backend.settingsResults.length === 0; text: "No Results"; Layout.fillWidth: true }
   ListView {
    id: settingsResults
    objectName: "settingsResults"
    visible: settingsSearch.text.trim().length > 0
    Layout.fillWidth: true
    Layout.preferredHeight: Math.min(contentHeight, root.height * 0.7)
    clip: true
    model: backend.settingsResults
    activeFocusOnTab: true
    keyNavigationWraps: true
    QQC2.ScrollBar.vertical: QQC2.ScrollBar {}
    delegate: QQC2.ItemDelegate {
     required property var modelData
     width: ListView.view.width
     objectName: "settingsResult_" + modelData.id
     highlighted: ListView.isCurrentItem
     contentItem: ColumnLayout {
      QQC2.Label { text: modelData.title; wrapMode: Text.Wrap; Layout.fillWidth: true }
      QQC2.Label { text: modelData.breadcrumb; wrapMode: Text.Wrap; opacity: 0.7; font: Kirigami.Theme.smallFont; Layout.fillWidth: true }
     }
     onClicked: backend.openSettingsResult(modelData)
     Keys.onReturnPressed: clicked()
     Keys.onEnterPressed: clicked()
     Accessible.name: modelData.title + ", " + modelData.breadcrumb
    }
    Keys.onEscapePressed: { backend.settingsQuery = ""; settingsSearch.forceActiveFocus() }
   }
  }

 }
 readonly property bool hasSettingsBack: editor.isPage || backend.commands.some(command => command.label === "Back")
 function goSettingsBack() {
  if (editor.isPage) backend.cancelDialog()
  else { const back = backend.commands.find(command => command.label === "Back"); if (back) backend.activate(back.key) }
 }
 Shortcut { sequences: [StandardKey.Back]; enabled: root.hasSettingsBack && !backend.busy; onActivated: root.goSettingsBack() }
 Shortcut { sequences: [StandardKey.Find]; enabled: true; onActivated: { drawer.drawerOpen = true; settingsSearch.forceActiveFocus() } }
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
  action.iconName = item.icon || ""
  action.children = (item.children || []).map(child => actionFor(child))
  return action
 }
 function updateNavigation() {
  let next = backend.navigation.map(item => actionFor(item))
  let wanted = new Set()
  function visit(action) { wanted.add(action.route); for (let child of action.children) visit(child) }
  for (let action of next) visit(action)
  drawer.actions = backend.settingsQuery.trim().length > 0 ? [] : next
  for (let route of Object.keys(navigationObjects)) {
   if (route.startsWith("folder:") && !wanted.has(route)) {
    navigationObjects[route].destroy()
    delete navigationObjects[route]
   }
  }
 }
 Component.onCompleted: updateNavigation()
 // Live pages stop asking while the window is hidden or minimized.
 Binding { target: backend; property: "shown"; value: root.visible && root.visibility !== Window.Minimized }
 Component {
  id: navigationAction
  Kirigami.Action {
   property string route
   property string iconName
   icon.name: iconName
   checked: backend.page === route
   enabled: !route.startsWith("group:") && !backend.busy && (Object.keys(backend.dialog).length === 0 || backend.dialog.page === true)
   onTriggered: backend.navigate(route)
  }
 }
 Connections { target: backend; function onSettingsSearchChanged() { root.updateNavigation() } function onNavigationChanged() {
  if (root.pageStack.currentItem && root.pageStack.currentItem.flickable) root.pageScrollPositions[root.previousPage] = root.pageStack.currentItem.flickable.contentY
  root.previousPage = backend.page
  root.updateNavigation()
  Qt.callLater(() => { if (root.pageStack.currentItem && root.pageStack.currentItem.flickable) root.pageStack.currentItem.flickable.contentY = root.pageScrollPositions[backend.page] || 0 })
 } function onCloseReady() { root.close() } }
 pageStack.initialPage: Kirigami.ScrollablePage {
  id: mainPage
  objectName: "settingsStackPage"
  title: editor.isPage ? backend.dialog.title : backend.title
  onBackRequested: event => {
   if (root.hasSettingsBack) { event.accepted = true; root.goSettingsBack() }
  }
  titleDelegate: RowLayout {
   QQC2.ToolButton { objectName: "settingsPageBack"; text: "Back"; icon.name: "go-previous"; visible: root.hasSettingsBack; enabled: !backend.busy; display: QQC2.AbstractButton.IconOnly; onClicked: root.goSettingsBack(); Accessible.name: "Back" }
   Kirigami.Heading { text: mainPage.title; level: 2; Layout.fillWidth: true; elide: Text.ElideRight }
  }
  actions: [Kirigami.Action { objectName: "settingsPageSave"; text: "Save"; icon.name: "document-save"; visible: editor.isPage; enabled: !backend.busy; onTriggered: backend.submit(editor.draft) }]
  ColumnLayout {
   spacing: Kirigami.Units.largeSpacing
   Kirigami.InlineMessage { Layout.fillWidth: true; visible: backend.error.length > 0; text: backend.error; type: Kirigami.MessageType.Error }
   QQC2.ProgressBar { Layout.fillWidth: true; visible: backend.busy; indeterminate: true; Accessible.name: "Operation in progress" }
   Loader { Layout.fillWidth: true; active: editor.isPage; sourceComponent: editorBody }
   Flow {
    id: commandBar
    visible: !editor.isPage
    Layout.fillWidth: true; spacing: Kirigami.Units.smallSpacing
    // Made again only when the commands change, so a refreshing page keeps their focus and presses.
    readonly property string commandsJson: JSON.stringify(backend.commands.filter(command => command.label !== "Back"))
    Repeater { model: JSON.parse(commandBar.commandsJson); QQC2.Button { required property var modelData; text: modelData.label; enabled: !backend.busy; onClicked: backend.activate(modelData.key) } }
   }
   GridLayout {
    id: rowGrid
    visible: !editor.isPage
    Layout.fillWidth: true
    columns: backend.grid ? Math.max(1, Math.floor(width / 320)) : 1
    Repeater {
     // Rows that a live page refreshes are changed in place, keeping their delegates.
     model: backend.rowModel
     ColumnLayout {
      id: rowItem
      required property var row
      readonly property var modelData: row
      // Buttons are made again only when the actions change, so a refreshing row keeps focus and open menus.
      readonly property string actionsJson: JSON.stringify(row.actions || [])
      readonly property var rowActions: JSON.parse(actionsJson)
      objectName: "row:" + (row.id || row.title)
      spacing: 0
      Layout.fillWidth: true; Layout.alignment: Qt.AlignTop
      // A section heading takes a line of its own in a grid.
      Layout.columnSpan: row.heading === true ? rowGrid.columns : 1
      Kirigami.Heading {
       visible: rowItem.row.heading === true
       Layout.fillWidth: true; Layout.topMargin: Kirigami.Units.largeSpacing
       level: 3; type: Kirigami.Heading.Type.Primary
       text: rowItem.row.title; wrapMode: Text.Wrap; textFormat: Text.PlainText
      }
      QQC2.Frame {
       visible: rowItem.row.heading !== true
       padding: modelData.navigationKey ? 0 : Kirigami.Units.largeSpacing
       Layout.fillWidth: true
       ColumnLayout {
        width: parent.width
        Image { visible: !!modelData.thumbnail; source: modelData.thumbnail || ""; Layout.preferredWidth: 96; Layout.preferredHeight: visible ? 72 : 0; fillMode: Image.PreserveAspectFit; asynchronous: true; Accessible.ignored: true }
        QQC2.ItemDelegate {
         Layout.fillWidth: true; visible: !!modelData.navigationKey
         text: modelData.title
         enabled: !backend.busy
         onClicked: backend.activate(modelData.navigationKey)
         contentItem: RowLayout {
          QQC2.Label { text: modelData.title; Layout.fillWidth: true; wrapMode: Text.Wrap }
          Kirigami.Icon { source: "go-next"; Layout.preferredWidth: Kirigami.Units.iconSizes.small; Layout.preferredHeight: width; Accessible.ignored: true }
         }
        }
        Kirigami.Heading { visible: !modelData.navigationKey; Layout.fillWidth: true; level: 3; text: modelData.title; wrapMode: Text.Wrap; textFormat: Text.PlainText }
        QQC2.Label { visible: !modelData.navigationKey; Layout.fillWidth: true; text: modelData.subtitle; wrapMode: Text.Wrap; textFormat: Text.PlainText }
        Image { visible: !!modelData.qr; source: visible ? "image://qr/" + encodeURIComponent(modelData.qr) : ""; Layout.preferredWidth: 300; Layout.preferredHeight: visible ? 300 : 0; Accessible.name: "Pairing QR code" }
        QQC2.Label { visible: !modelData.navigationKey && !!modelData.error; Layout.fillWidth: true; text: modelData.error || ""; color: Kirigami.Theme.negativeTextColor; wrapMode: Text.Wrap; textFormat: Text.PlainText }
        QQC2.ProgressBar { objectName: "rowProgress"; Layout.fillWidth: true; visible: modelData.progress !== undefined; from: 0; to: 1; value: modelData.progress || 0; Accessible.name: modelData.title; Accessible.description: modelData.subtitle }
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
         visible: !modelData.navigationKey
         Layout.fillWidth: true
         Repeater { model: rowItem.rowActions.slice(0, 2); QQC2.Button { required property var modelData; text: modelData.label; enabled: !backend.busy; onClicked: backend.activate(modelData.key) } }
         QQC2.ToolButton {
          visible: rowItem.rowActions.length > 2
          text: "More"; icon.name: "overflow-menu"
          onClicked: overflow.open()
          QQC2.Menu {
           id: overflow
           Repeater { model: rowItem.rowActions.slice(2); QQC2.MenuItem { required property var modelData; text: modelData.label; enabled: !backend.busy; onTriggered: backend.activate(modelData.key) } }
          }
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
  property bool initialized: false
  property bool hasAdvanced: fields.some(f => f.IsAdvanced === true)
  property bool isPage: backend.dialog.page === true
  property bool presented: opened || isPage
  property bool advancedExpanded: false
  property real parentOffset: 0
  Loader { active: !editor.isPage; sourceComponent: editorBody }

  footer: QQC2.DialogButtonBox {
   padding: Kirigami.Units.largeSpacing
   QQC2.Button { objectName: "editorCancel"; text: backend.dialog.discard ? "Discard Changes" : "Cancel"; QQC2.DialogButtonBox.buttonRole: QQC2.DialogButtonBox.ActionRole; enabled: !backend.busy || backend.cancelable; onClicked: backend.cancelDialog() }
   QQC2.Button { objectName: "editorSave"; text: backend.dialog.discard ? "Keep Editing" : backend.dialog.accept || "Save"; QQC2.DialogButtonBox.buttonRole: QQC2.DialogButtonBox.ActionRole; enabled: !backend.busy; onClicked: backend.dialog.discard ? backend.keepEditing() : backend.submit(editor.draft) }
  }
 }
 Component {
  id: editorBody
  SettingsEditorContent {
   fields: editor.fields; busy: backend.busy; expanded: editor.advancedExpanded; status: backend.dialogStatus
   targetAnchor: backend.settingsAnchor; targetField: backend.settingsField
   onRevealRequested: item => Qt.callLater(() => {
    if (editor.isPage && mainPage.flickable) {
     const position = item.mapToItem(mainPage.flickable.contentItem, 0, 0)
     mainPage.flickable.contentY = Math.max(0, Math.min(mainPage.flickable.contentHeight - mainPage.flickable.height, position.y - mainPage.flickable.height / 2))
    }
   })
   onExpansionRequested: value => editor.advancedExpanded = value
   onEdited: (path, value) => { editor.draft[path] = value; backend.fieldChanged() }
  }
 }
 Kirigami.Dialog {
  id: discardPage
  objectName: "settingsPageDiscard"
  parent: root.overlay; anchors.centerIn: parent
  title: "Discard unsaved changes?"
  modal: true; closePolicy: QQC2.Popup.NoAutoClose; showCloseButton: false
  QQC2.Label { text: "The changes on this page have not been saved."; wrapMode: Text.Wrap }
  footer: QQC2.DialogButtonBox {
   QQC2.Button { objectName: "settingsPageKeepEditing"; text: "Keep Editing"; onClicked: backend.keepEditing() }
   QQC2.Button { objectName: "settingsPageDiscardChanges"; text: "Discard Changes"; onClicked: backend.cancelDialog() }
  }
 }
 Connections {
  target: backend
  function onSettingsSaveFailed() {
   if (editor.presented && editor.hasAdvanced) editor.advancedExpanded = true
  }
  function onDialogChanged() {
   if (!backend.dialog.title) {
    editor.close(); discardPage.close()
    const owner = backend.page
    Qt.callLater(() => { if (backend.page === owner) mainPage.flickable.contentY = editor.parentOffset })
    editor.initialized = false; editor.fields = []; return
   }
   if (editor.isPage && backend.dialog.discard) { discardPage.open(); return }
   discardPage.close()
   if (!editor.initialized) {
    editor.initialized = true
    editor.parentOffset = mainPage.flickable.contentY
    editor.advancedExpanded = false
    editor.draft = {}; editor.fields = backend.dialog.fields
    for (let f of editor.fields) if (f.Kind !== "notice" && f.Kind !== "status") editor.draft[f.Path] = f.IsSecret ? "" : f.Value
    if (editor.isPage) Qt.callLater(() => mainPage.flickable.contentY = 0)
    else editor.open()
   }
  }
 }
}

// Copyright (C) 2026 Gordon. SPDX-License-Identifier: MPL-2.0
import QtQuick
import QtQuick.Controls as QQC2
import QtQuick.Layouts
import QtMultimedia
import QtQuick.Pdf
import org.kde.kirigami as Kirigami
Kirigami.ApplicationWindow {
 id: root
 required property string file
 required property string kind
 required property string text
 required property string location
 required property string name
 title: "Preview — Synctrain"; width: 850; height: 650; visible: true
 onClosing: player.stop()
 pageStack.initialPage: Kirigami.Page {
  padding: 0
  ColumnLayout {
   anchors.fill: parent
   QQC2.Button { text: "Open in Default App"; onClicked: backend.openDefault(root.location, root.name) }
   Image { Layout.fillWidth: true; Layout.fillHeight: true; visible: root.kind === "image"; source: visible ? root.file : ""; fillMode: Image.PreserveAspectFit; asynchronous: true; Accessible.name: "Image preview" }
   QQC2.ScrollView { Layout.fillWidth: true; Layout.fillHeight: true; visible: root.kind === "text"; QQC2.TextArea { text: root.text; readOnly: true; selectByMouse: true; textFormat: TextEdit.PlainText; wrapMode: TextEdit.Wrap } }
   PdfMultiPageView { Layout.fillWidth: true; Layout.fillHeight: true; visible: root.kind === "pdf"; document: PdfDocument { source: root.kind === "pdf" ? root.file : "" } }
   VideoOutput { id: video; Layout.fillWidth: true; Layout.fillHeight: true; visible: root.kind === "media"; Accessible.name: "Media preview" }
   RowLayout {
    visible: root.kind === "media"
    QQC2.Button { text: player.playbackState === MediaPlayer.PlayingState ? "Pause" : "Play"; onClicked: player.playbackState === MediaPlayer.PlayingState ? player.pause() : player.play() }
    QQC2.Slider { Layout.fillWidth: true; from: 0; to: player.duration; value: player.position; onMoved: player.position = value; Accessible.name: "Playback position" }
   }
   QQC2.Label { visible: player.error !== MediaPlayer.NoError; text: player.errorString; wrapMode: Text.Wrap }
  }
 }
 MediaPlayer { id: player; source: root.kind === "media" ? root.file : ""; videoOutput: video; audioOutput: AudioOutput {} }
}

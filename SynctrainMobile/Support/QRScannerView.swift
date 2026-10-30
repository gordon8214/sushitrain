// Copyright (C) 2024 Tommy van der Vorst
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.
import SwiftUI
import VisionKit

struct QRScannerViewRepresentable: UIViewControllerRepresentable {
	@Binding var scannedText: String
	@Binding var shouldStartScanning: Bool
	var dataToScanFor: Set<DataScannerViewController.RecognizedDataType>

	class Coordinator: NSObject, DataScannerViewControllerDelegate {
		var parent: QRScannerViewRepresentable

		init(_ parent: QRScannerViewRepresentable) {
			self.parent = parent
		}

		func dataScanner(
			_ dataScanner: DataScannerViewController, didAdd addedItems: [RecognizedItem],
			allItems: [RecognizedItem]
		) {
			for item in allItems {
				switch item {
				case .barcode(let barcode):
					if let text = barcode.payloadStringValue, !text.isEmpty {
						parent.scannedText = text
						parent.shouldStartScanning = false
						return
					}

				default:
					break
				}
			}
		}
	}

	func makeUIViewController(context: Context) -> DataScannerViewController {
		let dataScannerVC = DataScannerViewController(
			recognizedDataTypes: dataToScanFor,
			qualityLevel: .accurate,
			recognizesMultipleItems: false,
			isHighFrameRateTrackingEnabled: true,
			isPinchToZoomEnabled: true,
			isGuidanceEnabled: true,
			isHighlightingEnabled: true
		)

		dataScannerVC.delegate = context.coordinator
		return dataScannerVC
	}

	func updateUIViewController(_ uiViewController: DataScannerViewController, context: Context) {
		if shouldStartScanning {
			try? uiViewController.startScanning()
		}
		else {
			uiViewController.stopScanning()
		}
	}

	func makeCoordinator() -> Coordinator {
		Coordinator(self)
	}
}

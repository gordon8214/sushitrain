// Copyright (C) 2026 Gordon. SPDX-License-Identifier: MPL-2.0
#include "statuscache.h"
#include <KOverlayIconPlugin>
class SynctrainOverlays : public KOverlayIconPlugin {
  Q_OBJECT
  Q_PLUGIN_METADATA(IID "org.kde.overlayicon.synctrain")
public:
  SynctrainOverlays() {
    connect(&cache, &StatusCache::updated, this,
            [this](const QUrl &url, const QJsonObject &item) {
              emit overlaysChanged(
                  url, StatusCache::overlays(item["State"].toString()));
            });
  }
  QStringList getOverlays(const QUrl &url) override {
    return StatusCache::overlays(cache.status(url)["State"].toString());
  }

private:
  StatusCache cache;
};
#include "overlays.moc"

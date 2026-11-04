/* Copyright (C) 2026 Gordon. SPDX-License-Identifier: MPL-2.0 */
#pragma once

#include <gio/gio.h>

#define SYNCTRAIN_STATE "user.synctrain.state"
#define SYNCTRAIN_KEEP "user.synctrain.keep"
#define SYNCTRAIN_CACHE_US (5 * G_USEC_PER_SEC)
#define SYNCTRAIN_RESPONSE_LIMIT (1024 * 1024)

typedef struct {
    GPtrArray *mounts;
    GPtrArray *folders;
} SynctrainRoots;

char *synctrain_local_path(const char *uri);
gboolean synctrain_under(const char *path, GPtrArray *roots);
GPtrArray *synctrain_parse_mounts(const char *text);
GPtrArray *synctrain_parse_folders(const char *text);
SynctrainRoots *synctrain_discover(void);
void synctrain_roots_free(SynctrainRoots *roots);
const char *synctrain_state_label(const char *state);
char *synctrain_read_attribute(const char *path, const char *name);

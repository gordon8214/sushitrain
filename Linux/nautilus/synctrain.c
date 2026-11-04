/* Copyright (C) 2026 Gordon. SPDX-License-Identifier: MPL-2.0 */
#include "support.h"

#include <errno.h>
#include <gmodule.h>
#include <nautilus-extension.h>
#include <sys/xattr.h>

#define METADATA_KEY "synctrain-native-metadata"

typedef struct {
    GObject parent;
    SynctrainRoots *roots;
    gint64 discovered_at;
    gboolean discovering;
    GHashTable *waiting;
    GQueue keep_queue;
    gboolean keeping;
} Synctrain;

typedef struct { GObjectClass parent; } SynctrainClass;

typedef struct {
    gint64 at;
    guint generation;
    gboolean loading;
    gboolean kept;
    char *state;
    char *path;
    gboolean publication_pending;
} Metadata;

typedef struct {
    NautilusFileInfo *file;
    char *path;
    guint generation;
    gboolean kept;
    char *state;
} MetadataRequest;

typedef struct {
    GList *files;
    GPtrArray *paths;
    gboolean keep;
} Action;

GType synctrain_get_type(void);

static void action_free(gpointer data);
static void start_keep(Synctrain *self);

static void menu_init(NautilusMenuProviderInterface *interface);
static void info_init(NautilusInfoProviderInterface *interface);
static void column_init(NautilusColumnProviderInterface *interface);

G_DEFINE_DYNAMIC_TYPE_EXTENDED(Synctrain, synctrain, G_TYPE_OBJECT, 0,
    G_IMPLEMENT_INTERFACE_DYNAMIC(NAUTILUS_TYPE_MENU_PROVIDER, menu_init)
    G_IMPLEMENT_INTERFACE_DYNAMIC(NAUTILUS_TYPE_INFO_PROVIDER, info_init)
    G_IMPLEMENT_INTERFACE_DYNAMIC(NAUTILUS_TYPE_COLUMN_PROVIDER, column_init))

static void synctrain_finalize(GObject *object)
{
    Synctrain *self = (Synctrain *)object;
    g_clear_pointer(&self->roots, synctrain_roots_free);
    g_hash_table_unref(self->waiting);
    g_queue_clear_full(&self->keep_queue, action_free);
    G_OBJECT_CLASS(synctrain_parent_class)->finalize(object);
}

static void synctrain_class_init(SynctrainClass *class)
{
    G_OBJECT_CLASS(class)->finalize = synctrain_finalize;
}

static void synctrain_class_finalize(SynctrainClass *class)
{
    (void)class;
}

static void synctrain_init(Synctrain *self)
{
    g_queue_init(&self->keep_queue);
    self->waiting = g_hash_table_new_full(g_direct_hash, g_direct_equal, g_object_unref, NULL);
}

static void discover_worker(GTask *task, gpointer source, gpointer data, GCancellable *cancel)
{
    (void)source;
    (void)data;
    (void)cancel;
    g_task_return_pointer(task, synctrain_discover(), (GDestroyNotify)synctrain_roots_free);
}

static void discover_done(GObject *object, GAsyncResult *result, gpointer data)
{
    (void)data;
    Synctrain *self = (Synctrain *)object;
    g_clear_pointer(&self->roots, synctrain_roots_free);
    self->roots = g_task_propagate_pointer(G_TASK(result), NULL);
    self->discovering = FALSE;
    self->discovered_at = g_get_monotonic_time();
    /* Swap before invalidating: callbacks may reenter the provider. */
    GHashTable *waiting = self->waiting;
    self->waiting = g_hash_table_new_full(g_direct_hash, g_direct_equal, g_object_unref, NULL);
    GHashTableIter iter;
    gpointer file;
    g_hash_table_iter_init(&iter, waiting);
    while (g_hash_table_iter_next(&iter, &file, NULL))
        nautilus_file_info_invalidate_extension_info(file);
    g_hash_table_unref(waiting);
    nautilus_menu_provider_emit_items_updated_signal(NAUTILUS_MENU_PROVIDER(self));
}

static void ensure_roots(Synctrain *self, NautilusFileInfo *file)
{
    if (!self->discovering &&
        (!self->roots || g_get_monotonic_time() - self->discovered_at > SYNCTRAIN_CACHE_US)) {
        self->discovering = TRUE;
        g_autoptr(GTask) task = g_task_new(self, NULL, discover_done, NULL);
        g_task_run_in_thread(task, discover_worker);
    }
    if (self->discovering && file)
        g_hash_table_add(self->waiting, g_object_ref(file));
}

static void metadata_free(gpointer data)
{
    Metadata *metadata = data;
    g_free(metadata->state);
    g_free(metadata->path);
    g_free(metadata);
}

static Metadata *metadata_for(NautilusFileInfo *file)
{
    Metadata *metadata = g_object_get_data(G_OBJECT(file), METADATA_KEY);
    if (!metadata) {
        metadata = g_new0(Metadata, 1);
        g_object_set_data_full(G_OBJECT(file), METADATA_KEY, metadata, metadata_free);
    }
    return metadata;
}

/* Nautilus can preserve the same file object across a rename or move. */
static Metadata *metadata_for_path(NautilusFileInfo *file, const char *path)
{
    Metadata *metadata = metadata_for(file);
    if (g_strcmp0(metadata->path, path) != 0) {
        g_free(metadata->path);
        metadata->path = g_strdup(path);
        g_clear_pointer(&metadata->state, g_free);
        metadata->kept = FALSE;
        metadata->at = 0;
        metadata->generation++;
        metadata->publication_pending = FALSE;
    }
    return metadata;
}

static void metadata_request_free(gpointer data)
{
    MetadataRequest *request = data;
    g_object_unref(request->file);
    g_free(request->path);
    g_free(request->state);
    g_free(request);
}

static void metadata_worker(GTask *task, gpointer source, gpointer data, GCancellable *cancel)
{
    (void)source;
    (void)cancel;
    MetadataRequest *request = data;
    request->state = synctrain_read_attribute(request->path, SYNCTRAIN_STATE);
    g_autofree char *keep = synctrain_read_attribute(request->path, SYNCTRAIN_KEEP);
    request->kept = g_strcmp0(keep, "1") == 0;
    g_task_return_boolean(task, TRUE);
}

static void metadata_done(GObject *object, GAsyncResult *result, gpointer data)
{
    (void)data;
    GTask *task = G_TASK(result);
    MetadataRequest *request = g_task_get_task_data(task);
    g_autofree char *uri = nautilus_file_info_get_uri(request->file);
    g_autofree char *path = synctrain_local_path(uri);
    Metadata *metadata = metadata_for_path(request->file, path);
    metadata->loading = FALSE;
    if (request->generation == metadata->generation && g_strcmp0(request->path, path) == 0) {
        g_free(metadata->state);
        metadata->state = g_steal_pointer(&request->state);
        metadata->kept = request->kept;
        metadata->at = g_get_monotonic_time();
        metadata->publication_pending = TRUE;
    }
    nautilus_file_info_invalidate_extension_info(request->file);
    nautilus_menu_provider_emit_items_updated_signal(NAUTILUS_MENU_PROVIDER(object));
}

static Metadata *ensure_metadata(Synctrain *self, NautilusFileInfo *file, const char *path)
{
    Metadata *metadata = metadata_for_path(file, path);
    if (!metadata->loading &&
        (!metadata->at || g_get_monotonic_time() - metadata->at > SYNCTRAIN_CACHE_US)) {
        metadata->loading = TRUE;
        MetadataRequest *request = g_new0(MetadataRequest, 1);
        request->file = g_object_ref(file);
        request->path = g_strdup(path);
        request->generation = metadata->generation;
        g_autoptr(GTask) task = g_task_new(self, NULL, metadata_done, NULL);
        g_task_set_task_data(task, request, metadata_request_free);
        g_task_run_in_thread(task, metadata_worker);
    }
    return metadata;
}

static void action_free(gpointer data)
{
    Action *action = data;
    nautilus_file_info_list_free(action->files);
    g_ptr_array_unref(action->paths);
    g_free(action);
}

static void action_closure_free(gpointer data, GClosure *closure)
{
    (void)closure;
    action_free(data);
}

static void keep_worker(GTask *task, gpointer source, gpointer data, GCancellable *cancel)
{
    (void)source;
    (void)cancel;
    Action *action = data;
    for (guint i = 0; i < action->paths->len; i++) {
        const char *path = g_ptr_array_index(action->paths, i);
        if (setxattr(path, SYNCTRAIN_KEEP, action->keep ? "1" : "0", 1, 0) != 0)
            g_printerr("Synctrain: %s: %s\n", path, g_strerror(errno));
    }
    g_task_return_boolean(task, TRUE);
}

static void keep_done(GObject *object, GAsyncResult *result, gpointer data)
{
    (void)data;
    Action *action = g_task_get_task_data(G_TASK(result));
    for (GList *item = action->files; item; item = item->next) {
        Metadata *metadata = metadata_for(item->data);
        metadata->at = 0;
        metadata->generation++;
        metadata->publication_pending = FALSE;
        nautilus_file_info_invalidate_extension_info(item->data);
    }
    nautilus_menu_provider_emit_items_updated_signal(NAUTILUS_MENU_PROVIDER(object));
    Synctrain *self = (Synctrain *)object;
    self->keeping = FALSE;
    start_keep(self);
}

static void start_keep(Synctrain *self)
{
    if (self->keeping || g_queue_is_empty(&self->keep_queue))
        return;
    self->keeping = TRUE;
    Action *action = g_queue_pop_head(&self->keep_queue);
    g_autoptr(GTask) task = g_task_new(self, NULL, keep_done, NULL);
    g_task_set_task_data(task, action, action_free);
    g_task_run_in_thread(task, keep_worker);
}

static void keep_activate(NautilusMenuItem *item, gpointer data)
{
    Action *original = data;
    Action *action = g_new0(Action, 1);
    action->files = nautilus_file_info_list_copy(original->files);
    action->paths = g_ptr_array_ref(original->paths);
    action->keep = original->keep;
    Synctrain *self = g_object_get_data(G_OBJECT(item), "provider");
    g_queue_push_tail(&self->keep_queue, action);
    start_keep(self);
}

static NautilusMenuItem *keep_item(Synctrain *self, GList *files, GPtrArray *paths, gboolean keep)
{
    NautilusMenuItem *item = nautilus_menu_item_new(
        keep ? "Synctrain::keep" : "Synctrain::stop-keeping",
        keep ? "Keep on This Device" : "Stop Keeping on This Device", "", NULL);
    Action *action = g_new0(Action, 1);
    action->files = nautilus_file_info_list_copy(files);
    action->paths = g_ptr_array_ref(paths);
    action->keep = keep;
    g_object_set_data_full(G_OBJECT(item), "provider", g_object_ref(self), g_object_unref);
    g_signal_connect_data(item, "activate", G_CALLBACK(keep_activate), action, action_closure_free, 0);
    return item;
}

static void versions_activate(NautilusMenuItem *item, gpointer data)
{
    (void)item;
    g_autoptr(GError) error = NULL;
    g_autoptr(GSubprocess) process = g_subprocess_new(G_SUBPROCESS_FLAGS_NONE, &error,
                                                   "synctrain", "versions", (char *)data, NULL);
    if (!process)
        g_printerr("Synctrain: %s\n", error->message);
    else
        g_subprocess_wait_async(process, NULL, NULL, NULL);
}

static void path_closure_free(gpointer data, GClosure *closure)
{
    (void)closure;
    g_free(data);
}

static NautilusMenuItem *versions_item(const char *path)
{
    NautilusMenuItem *item = nautilus_menu_item_new("Synctrain::versions", "Previous Versions…", "", NULL);
    g_signal_connect_data(item, "activate", G_CALLBACK(versions_activate), g_strdup(path), path_closure_free, 0);
    return item;
}

static GList *get_file_items(NautilusMenuProvider *provider, GList *files)
{
    Synctrain *self = (Synctrain *)provider;
    ensure_roots(self, NULL);
    if (!files || !self->roots)
        return NULL;
    g_autoptr(GPtrArray) paths = g_ptr_array_new_with_free_func(g_free);
    gboolean all_mounted = TRUE, any_kept = FALSE, all_kept = TRUE, ready = TRUE;
    for (GList *item = files; item; item = item->next) {
        g_autofree char *uri = nautilus_file_info_get_uri(item->data);
        char *path = synctrain_local_path(uri);
        g_ptr_array_add(paths, path);
        if (!synctrain_under(path, self->roots->mounts)) {
            all_mounted = FALSE;
            continue;
        }
        Metadata *metadata = ensure_metadata(self, item->data, path);
        ready &= metadata->at != 0;
        any_kept |= metadata->kept;
        all_kept &= metadata->kept;
    }
    const char *path = g_ptr_array_index(paths, 0);
    if (!all_mounted) {
        if (!files->next && !nautilus_file_info_is_directory(files->data) &&
            synctrain_under(path, self->roots->folders))
            return g_list_append(NULL, versions_item(path));
        return NULL;
    }
    GList *items = NULL;
    if (ready && !all_kept)
        items = g_list_append(items, keep_item(self, files, paths, TRUE));
    if (ready && any_kept)
        items = g_list_append(items, keep_item(self, files, paths, FALSE));
    if (!files->next && !nautilus_file_info_is_directory(files->data))
        items = g_list_append(items, versions_item(path));
    return items;
}

static NautilusOperationResult update_file_info(NautilusInfoProvider *provider, NautilusFileInfo *file,
                                                GClosure *complete, NautilusOperationHandle **handle)
{
    (void)complete;
    if (handle)
        *handle = NULL;
    Synctrain *self = (Synctrain *)provider;
    ensure_roots(self, file);
    g_autofree char *uri = nautilus_file_info_get_uri(file);
    g_autofree char *path = synctrain_local_path(uri);
    const char *label = "";
    if (self->roots && synctrain_under(path, self->roots->mounts)) {
        Metadata *metadata = metadata_for_path(file, path);
        if (metadata->publication_pending) {
            /* Consume our own invalidation once, without starting a read loop. */
            metadata->publication_pending = FALSE;
        } else {
            /* Real file-change notifications must bypass the menu cache TTL. */
            metadata->at = 0;
            if (metadata->loading)
                metadata->generation++;
        }
        ensure_metadata(self, file, path);
        label = synctrain_state_label(metadata->state);
    }
    nautilus_file_info_add_string_attribute(file, "synctrain_state", label);
    /* Publish cached info now; the worker invalidates it when fresh info is ready. */
    return NAUTILUS_OPERATION_COMPLETE;
}

static GList *get_columns(NautilusColumnProvider *provider)
{
    (void)provider;
    return g_list_append(NULL, nautilus_column_new("SynctrainColumn::state", "synctrain_state",
                                                 "Synctrain", "Whether the item is on this device"));
}

static void menu_init(NautilusMenuProviderInterface *interface)
{
    interface->get_file_items = get_file_items;
}

static void info_init(NautilusInfoProviderInterface *interface)
{
    interface->update_file_info = update_file_info;
}

static void column_init(NautilusColumnProviderInterface *interface)
{
    interface->get_columns = get_columns;
}

G_MODULE_EXPORT void nautilus_module_initialize(GTypeModule *module);
G_MODULE_EXPORT void nautilus_module_shutdown(void);
G_MODULE_EXPORT void nautilus_module_list_types(const GType **types, int *count);

G_MODULE_EXPORT void nautilus_module_initialize(GTypeModule *module)
{
    synctrain_register_type(module);
}

G_MODULE_EXPORT void nautilus_module_shutdown(void)
{
}

G_MODULE_EXPORT void nautilus_module_list_types(const GType **types, int *count)
{
    static GType exported;
    exported = synctrain_get_type();
    *types = &exported;
    *count = 1;
}

/* Copyright (C) 2026 Gordon. SPDX-License-Identifier: MPL-2.0 */
/* Include the provider to seed mount/cache fixtures without a production test API.
 * The same suite also loads the separately built .so in a fresh process. */
#include <sys/xattr.h>
#include <poll.h>

/* Hold one retention syscall to exercise overlapping activations deterministically. */
static int test_setxattr(const char *path, const char *name, const void *value, size_t size, int flags);
#define setxattr test_setxattr
#include "../nautilus/synctrain.c"
#undef setxattr

static GMutex write_mutex;
static GCond write_condition;
static gboolean hold_first_write;
static gboolean write_started;
static gboolean release_write;
static guint write_count;
static char write_values[8];

static int test_setxattr(const char *path, const char *name, const void *value, size_t size, int flags)
{
    g_mutex_lock(&write_mutex);
    if (hold_first_write && g_strcmp0(name, SYNCTRAIN_KEEP) == 0) {
        g_assert_cmpuint(write_count, <, sizeof(write_values));
        write_values[write_count++] = *(const char *)value;
        if (write_count == 1) {
            write_started = TRUE;
            while (!release_write)
                g_cond_wait(&write_condition, &write_mutex);
        }
    }
    g_mutex_unlock(&write_mutex);
    return setxattr(path, name, value, size, flags);
}

#include <glib/gstdio.h>
#include <sys/stat.h>
#include <sys/socket.h>
#include <sys/un.h>
#include <unistd.h>

typedef struct {
    GObject parent;
    char *uri;
    char *label;
    gboolean directory;
    guint invalidated;
} TestFile;
typedef struct { GObjectClass parent; } TestFileClass;
static void test_file_interface_init(NautilusFileInfoInterface *interface);
GType test_file_get_type(void);
G_DEFINE_TYPE_WITH_CODE(TestFile, test_file, G_TYPE_OBJECT,
                       G_IMPLEMENT_INTERFACE(NAUTILUS_TYPE_FILE_INFO, test_file_interface_init))
static char *file_uri(NautilusFileInfo *file) { return g_strdup(((TestFile *)file)->uri); }
static gboolean file_directory(NautilusFileInfo *file) { return ((TestFile *)file)->directory; }
static void file_invalidate(NautilusFileInfo *file) { ((TestFile *)file)->invalidated++; }
static void file_attribute(NautilusFileInfo *file, const char *name, const char *value)
{
    g_assert_cmpstr(name, ==, "synctrain_state");
    TestFile *self = (TestFile *)file;
    g_free(self->label);
    self->label = g_strdup(value);
}
static void test_file_interface_init(NautilusFileInfoInterface *interface)
{
    interface->get_uri = file_uri;
    interface->is_directory = file_directory;
    interface->invalidate_extension_info = file_invalidate;
    interface->add_string_attribute = file_attribute;
}
static void test_file_finalize(GObject *object)
{
    TestFile *self = (TestFile *)object;
    g_free(self->uri);
    g_free(self->label);
    G_OBJECT_CLASS(test_file_parent_class)->finalize(object);
}
static void test_file_class_init(TestFileClass *class) { G_OBJECT_CLASS(class)->finalize = test_file_finalize; }
static void test_file_init(TestFile *self) { (void)self; }
static TestFile *new_file(const char *uri, gboolean directory)
{
    TestFile *file = g_object_new(test_file_get_type(), NULL);
    file->uri = g_strdup(uri);
    file->directory = directory;
    return file;
}

typedef struct { GTypeModule parent; } TestModule;
typedef struct { GTypeModuleClass parent; } TestModuleClass;
GType test_module_get_type(void);
G_DEFINE_TYPE(TestModule, test_module, G_TYPE_TYPE_MODULE)
static gboolean module_load(GTypeModule *module) { (void)module; return TRUE; }
static void module_unload(GTypeModule *module) { (void)module; }
static void test_module_class_init(TestModuleClass *class)
{
    G_TYPE_MODULE_CLASS(class)->load = module_load;
    G_TYPE_MODULE_CLASS(class)->unload = module_unload;
}
static void test_module_init(TestModule *self) { (void)self; }

static Synctrain *provider(void)
{
    Synctrain *self = g_object_new(synctrain_get_type(), NULL);
    self->roots = g_new0(SynctrainRoots, 1);
    self->roots->mounts = synctrain_parse_mounts("st /mount fuse.synctrain rw 0 0\n");
    self->roots->folders = synctrain_parse_folders("{\"result\":[{\"Path\":\"/sync\"}]}");
    self->discovered_at = g_get_monotonic_time();
    return self;
}

static void cached(TestFile *file, gboolean kept, const char *state)
{
    g_autofree char *path = synctrain_local_path(file->uri);
    Metadata *metadata = metadata_for_path(NAUTILUS_FILE_INFO(file), path);
    metadata->publication_pending = TRUE;
    metadata->at = g_get_monotonic_time();
    metadata->kept = kept;
    g_free(metadata->state);
    metadata->state = g_strdup(state);
}

static void assert_menu(Synctrain *self, GList *files, const char *names)
{
    GList *items = nautilus_menu_provider_get_file_items(NAUTILUS_MENU_PROVIDER(self), files);
    g_autoptr(GString) actual = g_string_new(NULL);
    for (GList *item = items; item; item = item->next) {
        g_autofree char *name = NULL;
        g_object_get(item->data, "name", &name, NULL);
        if (actual->len) g_string_append_c(actual, ',');
        g_string_append(actual, name);
    }
    g_assert_cmpstr(actual->str, ==, names);
    g_list_free_full(items, g_object_unref);
}

static void test_menus(void)
{
    Synctrain *self = provider();
    TestFile *first = new_file("file:///mount/a", FALSE);
    TestFile *second = new_file("file:///mount/b", FALSE);
    cached(first, FALSE, "remote");
    cached(second, TRUE, "kept");
    GList *files = g_list_append(NULL, first);
    assert_menu(self, NULL, "");
    assert_menu(self, files, "Synctrain::keep,Synctrain::versions");
    cached(first, TRUE, "kept");
    assert_menu(self, files, "Synctrain::stop-keeping,Synctrain::versions");
    first->directory = TRUE;
    assert_menu(self, files, "Synctrain::stop-keeping");
    first->directory = FALSE;
    cached(first, FALSE, "remote");
    files = g_list_append(files, second);
    assert_menu(self, files, "Synctrain::keep,Synctrain::stop-keeping");
    g_free(second->uri);
    second->uri = g_strdup("file:///outside/b");
    assert_menu(self, files, "");
    g_list_free_full(files, g_object_unref);
    g_object_unref(self);
}

static void test_synchronized_menus(void)
{
    Synctrain *self = provider();
    const char *uris[] = {"file:///sync/a%20b", "file://localhost/sync/a", "file://another/sync/a",
                          "file:///sync-other/a", "smb://server/sync/a", "file:///sync/../outside/a"};
    for (guint i = 0; i < G_N_ELEMENTS(uris); i++) {
        TestFile *file = new_file(uris[i], FALSE);
        GList *files = g_list_append(NULL, file);
        assert_menu(self, files, i < 2 ? "Synctrain::versions" : "");
        file->directory = TRUE;
        assert_menu(self, files, "");
        g_list_free_full(files, g_object_unref);
    }
    g_object_unref(self);
}

static void test_paths(void)
{
    g_autofree char *path = synctrain_local_path("file://localhost/a%20b/c/../d");
    g_assert_cmpstr(path, ==, "/a b/d");
    g_assert_null(synctrain_local_path("file://remote/a"));
    g_assert_null(synctrain_local_path("file:///a%00b"));
    g_autoptr(GPtrArray) roots = synctrain_parse_mounts(
        "st /a\\040b fuse.synctrain rw 0 0\nst /other fuse.other rw 0 0\nst /tab\\011x fuse.synctrain rw 0 0\n");
    g_assert_cmpuint(roots->len, ==, 2);
    g_assert_cmpstr(g_ptr_array_index(roots, 0), ==, "/a b");
    g_assert_cmpstr(g_ptr_array_index(roots, 1), ==, "/tab\tx");
    g_assert_true(synctrain_under("/a b/file", roots));
    g_assert_false(synctrain_under("/a b", roots));
    g_assert_false(synctrain_under("/a b-other/file", roots));
    g_ptr_array_add(roots, g_strdup("/"));
    g_assert_true(synctrain_under("/file", roots));
    g_assert_false(synctrain_under("/", roots));
}

static void test_json(void)
{
    const char *invalid[] = {"no", "[]", "null", "{\"result\":null}", "{\"result\":true}",
                             "{\"result\":[null,1,{}, {\"Path\":true},{\"Path\":\"relative\"}]}"};
    for (guint i = 0; i < G_N_ELEMENTS(invalid); i++) {
        g_autoptr(GPtrArray) roots = synctrain_parse_folders(invalid[i]);
        g_assert_cmpuint(roots->len, ==, 0);
    }
    g_autoptr(GPtrArray) roots = synctrain_parse_folders("{\"result\":[{\"Path\":\"/a/../b\"}]}");
    g_assert_cmpstr(g_ptr_array_index(roots, 0), ==, "/b");
}

static void test_columns(void)
{
    Synctrain *self = provider();
    GList *columns = nautilus_column_provider_get_columns(NAUTILUS_COLUMN_PROVIDER(self));
    g_assert_cmpuint(g_list_length(columns), ==, 1);
    g_autofree char *attribute = NULL;
    g_object_get(columns->data, "attribute", &attribute, NULL);
    g_assert_cmpstr(attribute, ==, "synctrain_state");
    g_list_free_full(columns, g_object_unref);
    TestFile *file = new_file("file:///mount/a", FALSE);
    const char *states[] = {"remote", "partial", "local", "kept", "pending", "failed", "blocked", "local-only", "unknown", NULL};
    const char *labels[] = {"On the gateway", "Partly here", "Here", "Kept here", "Sending", "Can't be sent", "Can't be sent", "Only here", "", ""};
    GClosure *complete = g_cclosure_new(G_CALLBACK(file_invalidate), NULL, NULL);
    g_closure_ref(complete);
    g_closure_sink(complete);
    NautilusOperationHandle *handle = NULL;
    for (guint i = 0; i < G_N_ELEMENTS(states); i++) {
        cached(file, FALSE, states[i]);
        g_assert_cmpint(nautilus_info_provider_update_file_info(NAUTILUS_INFO_PROVIDER(self),
                       NAUTILUS_FILE_INFO(file), complete, &handle), ==, NAUTILUS_OPERATION_COMPLETE);
        g_assert_cmpstr(file->label, ==, labels[i]);
    }
    g_closure_unref(complete);
    g_object_unref(file);
    g_object_unref(self);
}

static void drain_until(gboolean (*finished)(gpointer), gpointer data)
{
    gint64 deadline = g_get_monotonic_time() + 4 * G_USEC_PER_SEC;
    while (!finished(data)) {
        while (g_main_context_iteration(NULL, FALSE)) {}
        g_assert_cmpint(g_get_monotonic_time(), <, deadline);
        g_usleep(1000);
    }
}
static gboolean invalidated(gpointer data) { return ((TestFile *)data)->invalidated != 0; }
static gboolean not_loading(gpointer data) { return !metadata_for(data)->loading; }
static gboolean discovered(gpointer data) { return !((Synctrain *)data)->discovering; }

static void test_retention(void)
{
    g_autofree char *directory = g_dir_make_tmp("synctrain-native-XXXXXX", NULL);
    g_autofree char *path = g_build_filename(directory, "a b;$(literal).txt", NULL);
    g_assert_true(g_file_set_contents(path, "content", -1, NULL));
    g_autofree char *uri = g_filename_to_uri(path, NULL, NULL);
    TestFile *file = new_file(uri, FALSE);
    Synctrain *self = provider();
    g_ptr_array_add(self->roots->mounts, g_strdup(directory));
    GList *files = g_list_append(NULL, file);
    /* Initially metadata is unknown: only history appears, then a worker refreshes it. */
    assert_menu(self, files, "Synctrain::versions");
    drain_until(not_loading, file);
    for (guint keep = 1; keep <= 2; keep++) {
        file->invalidated = 0;
        GList *items = get_file_items(NAUTILUS_MENU_PROVIDER(self), files);
        g_assert_nonnull(items);
        nautilus_menu_item_activate(items->data);
        /* Destroying the menu must not invalidate the worker's inputs. */
        g_list_free_full(items, g_object_unref);
        drain_until(invalidated, file);
        g_autofree char *value = synctrain_read_attribute(path, SYNCTRAIN_KEEP);
        g_assert_cmpstr(value, ==, keep == 1 ? "1" : "0");
        ensure_metadata(self, NAUTILUS_FILE_INFO(file), path);
        drain_until(not_loading, file);
    }
    g_assert_cmpint(g_remove(path), ==, 0);
    g_assert_cmpint(g_rmdir(directory), ==, 0);
    g_list_free_full(files, g_object_unref);
    g_object_unref(self);
}

static gboolean writes_held(gpointer data)
{
    (void)data;
    g_mutex_lock(&write_mutex);
    gboolean started = write_started;
    g_mutex_unlock(&write_mutex);
    return started;
}

static gboolean retention_finished(gpointer data)
{
    return !((Synctrain *)data)->keeping;
}

static void test_retention_order(void)
{
    g_autofree char *directory = g_dir_make_tmp("synctrain-order-XXXXXX", NULL);
    g_autofree char *first_path = g_build_filename(directory, "first", NULL);
    g_autofree char *second_path = g_build_filename(directory, "second", NULL);
    g_assert_true(g_file_set_contents(first_path, "a", -1, NULL));
    g_assert_true(g_file_set_contents(second_path, "b", -1, NULL));
    g_autofree char *first_uri = g_filename_to_uri(first_path, NULL, NULL);
    g_autofree char *second_uri = g_filename_to_uri(second_path, NULL, NULL);
    TestFile *first = new_file(first_uri, FALSE);
    TestFile *second = new_file(second_uri, FALSE);
    cached(first, FALSE, "remote");
    cached(second, TRUE, "kept");
    Synctrain *self = provider();
    g_ptr_array_add(self->roots->mounts, g_strdup(directory));
    GList *files = g_list_append(g_list_append(NULL, first), second);
    GList *items = get_file_items(NAUTILUS_MENU_PROVIDER(self), files);
    g_assert_cmpuint(g_list_length(items), ==, 2);
    g_mutex_lock(&write_mutex);
    hold_first_write = TRUE;
    write_started = FALSE;
    release_write = FALSE;
    write_count = 0;
    g_mutex_unlock(&write_mutex);
    nautilus_menu_item_activate(items->data);
    drain_until(writes_held, NULL);
    nautilus_menu_item_activate(items->next->data);
    /* Stop is queued while Keep is held inside its first filesystem write. */
    g_assert_cmpuint(g_queue_get_length(&self->keep_queue), ==, 1);
    g_list_free_full(items, g_object_unref);
    g_mutex_lock(&write_mutex);
    release_write = TRUE;
    g_cond_signal(&write_condition);
    g_mutex_unlock(&write_mutex);
    drain_until(retention_finished, self);
    g_mutex_lock(&write_mutex);
    hold_first_write = FALSE;
    g_assert_cmpuint(write_count, ==, 4);
    g_assert_cmpmem(write_values, 4, "1100", 4);
    g_mutex_unlock(&write_mutex);
    g_autofree char *first_value = synctrain_read_attribute(first_path, SYNCTRAIN_KEEP);
    g_autofree char *second_value = synctrain_read_attribute(second_path, SYNCTRAIN_KEEP);
    g_assert_cmpstr(first_value, ==, "0");
    g_assert_cmpstr(second_value, ==, "0");
    g_list_free_full(files, g_object_unref);
    g_object_unref(self);
    g_remove(first_path);
    g_remove(second_path);
    g_rmdir(directory);
}

static void test_metadata_location(void)
{
    g_autofree char *directory = g_dir_make_tmp("synctrain-location-XXXXXX", NULL);
    g_autofree char *old_path = g_build_filename(directory, "old", NULL);
    g_autofree char *new_path = g_build_filename(directory, "new", NULL);
    g_assert_true(g_file_set_contents(old_path, "a", -1, NULL));
    g_assert_true(g_file_set_contents(new_path, "b", -1, NULL));
    g_assert_cmpint(setxattr(old_path, SYNCTRAIN_STATE, "remote", 6, 0), ==, 0);
    g_assert_cmpint(setxattr(new_path, SYNCTRAIN_STATE, "kept", 4, 0), ==, 0);
    g_assert_cmpint(setxattr(new_path, SYNCTRAIN_KEEP, "1", 1, 0), ==, 0);
    g_autofree char *old_uri = g_filename_to_uri(old_path, NULL, NULL);
    g_autofree char *new_uri = g_filename_to_uri(new_path, NULL, NULL);
    TestFile *file = new_file(old_uri, FALSE);
    cached(file, FALSE, "remote");
    Synctrain *self = provider();
    g_ptr_array_add(self->roots->mounts, g_strdup(directory));
    GList *files = g_list_append(NULL, file);
    /* A location change immediately discards the previous location's snapshot. */
    g_free(file->uri);
    file->uri = g_strdup(new_uri);
    assert_menu(self, files, "Synctrain::versions");
    drain_until(not_loading, file);
    assert_menu(self, files, "Synctrain::stop-keeping,Synctrain::versions");
    g_assert_cmpstr(metadata_for(NAUTILUS_FILE_INFO(file))->state, ==, "kept");
    /* Move again after a request starts, before its callback can run. */
    g_free(file->uri);
    file->uri = g_strdup(old_uri);
    ensure_metadata(self, NAUTILUS_FILE_INFO(file), old_path);
    g_free(file->uri);
    file->uri = g_strdup(new_uri);
    drain_until(not_loading, file);
    Metadata *metadata = metadata_for(NAUTILUS_FILE_INFO(file));
    g_assert_cmpstr(metadata->path, ==, new_path);
    g_assert_cmpint(metadata->at, ==, 0);
    g_assert_null(metadata->state);
    ensure_metadata(self, NAUTILUS_FILE_INFO(file), new_path);
    drain_until(not_loading, file);
    g_assert_cmpstr(metadata->state, ==, "kept");
    g_list_free_full(files, g_object_unref);
    g_object_unref(self);
    g_remove(old_path);
    g_remove(new_path);
    g_rmdir(directory);
}

static void test_metadata_invalidation(void)
{
    g_autofree char *directory = g_dir_make_tmp("synctrain-invalidation-XXXXXX", NULL);
    g_autofree char *path = g_build_filename(directory, "file", NULL);
    g_assert_true(g_file_set_contents(path, "content", -1, NULL));
    g_assert_cmpint(setxattr(path, SYNCTRAIN_STATE, "remote", 6, 0), ==, 0);
    g_autofree char *uri = g_filename_to_uri(path, NULL, NULL);
    TestFile *file = new_file(uri, FALSE);
    Synctrain *self = provider();
    g_ptr_array_add(self->roots->mounts, g_strdup(directory));
    update_file_info(NAUTILUS_INFO_PROVIDER(self), NAUTILUS_FILE_INFO(file), NULL, NULL);
    drain_until(not_loading, file);
    Metadata *metadata = metadata_for(NAUTILUS_FILE_INFO(file));
    /* Publishing the completed request must not launch another worker. */
    g_assert_true(metadata->publication_pending);
    update_file_info(NAUTILUS_INFO_PROVIDER(self), NAUTILUS_FILE_INFO(file), NULL, NULL);
    g_assert_cmpstr(file->label, ==, "On the gateway");
    g_assert_false(metadata->loading);
    g_assert_false(metadata->publication_pending);
    /* An external change inside the five-second menu TTL must still be read. */
    g_assert_cmpint(setxattr(path, SYNCTRAIN_STATE, "kept", 4, 0), ==, 0);
    update_file_info(NAUTILUS_INFO_PROVIDER(self), NAUTILUS_FILE_INFO(file), NULL, NULL);
    g_assert_true(metadata->loading);
    drain_until(not_loading, file);
    update_file_info(NAUTILUS_INFO_PROVIDER(self), NAUTILUS_FILE_INFO(file), NULL, NULL);
    g_assert_cmpstr(file->label, ==, "Kept here");
    g_assert_false(metadata->loading);
    /* A real invalidation during a read must discard that in-flight snapshot. */
    update_file_info(NAUTILUS_INFO_PROVIDER(self), NAUTILUS_FILE_INFO(file), NULL, NULL);
    update_file_info(NAUTILUS_INFO_PROVIDER(self), NAUTILUS_FILE_INFO(file), NULL, NULL);
    drain_until(not_loading, file);
    g_assert_cmpint(metadata->at, ==, 0);
    g_assert_false(metadata->publication_pending);
    update_file_info(NAUTILUS_INFO_PROVIDER(self), NAUTILUS_FILE_INFO(file), NULL, NULL);
    drain_until(not_loading, file);
    update_file_info(NAUTILUS_INFO_PROVIDER(self), NAUTILUS_FILE_INFO(file), NULL, NULL);
    g_assert_false(metadata->loading);
    g_object_unref(file);
    g_object_unref(self);
    g_remove(path);
    g_rmdir(directory);
}

typedef struct {
    int listener;
    const char *response;
    gboolean stall;
    gboolean optional;
} Server;
static gpointer serve(gpointer data)
{
    Server *server = data;
    struct pollfd descriptor = {.fd = server->listener, .events = POLLIN};
    int ready = poll(&descriptor, 1, 500);
    if (server->optional && ready == 0)
        return NULL;
    g_assert_cmpint(ready, ==, 1);
    int client = accept(server->listener, NULL, NULL);
    g_assert_cmpint(client, >=, 0);
    char request[128] = {0};
    g_assert_cmpint(read(client, request, sizeof(request) - 1), >, 0);
    g_assert_cmpstr(request, ==, "{\"method\":\"engine.folders\"}\n");
    if (server->stall) {
        /* Wait for the client's deadline to close its side; no real sleep. */
        g_assert_cmpint(read(client, request, sizeof(request)), ==, 0);
    } else {
        size_t offset = 0, length = strlen(server->response);
        while (offset < length) {
            ssize_t count = send(client, server->response + offset, length - offset, MSG_NOSIGNAL);
            if (count < 0 && errno == EINTR)
                continue;
            if (count <= 0)
                break; /* The capped client closes before consuming an oversized response. */
            offset += count;
        }
    }
    close(client);
    return NULL;
}

static char *fixture_dir;
static char *gateway_script;
static void set_helper(const char *script)
{
    g_assert_true(g_file_set_contents(gateway_script, script, -1, NULL));
    g_assert_cmpint(g_chmod(gateway_script, 0700), ==, 0);
}
static void test_discovery(void)
{
    g_autofree char *endpoint = g_build_filename(fixture_dir, "admin.sock", NULL);
    g_autofree char *script = g_strdup_printf("#!/bin/sh\nprintf '{\"socket\":\"%s\"}\\n'\n", endpoint);
    set_helper(script);
    g_autofree char *padding = g_strnfill(SYNCTRAIN_RESPONSE_LIMIT, 'x');
    g_autofree char *oversized = g_strdup_printf(
        "{\"padding\":\"%s\",\"result\":[{\"Path\":\"/sync\"}]}\n", padding);
    g_autofree char *oversized_helper = g_strdup_printf(
        "#!/bin/sh\nprintf '{\"socket\":\"%s\",\"padding\":\"'\n"
        "head -c 1048576 /dev/zero | tr '\\000' x\nprintf '\"}\\n'\n", endpoint);
    for (guint scenario = 0; scenario < 5; scenario++) {
        set_helper(scenario == 4 ? oversized_helper : script);
        gboolean stall = scenario == 1;
        Server server = {.listener = socket(AF_UNIX, SOCK_STREAM, 0),
                         .response = scenario == 2 ? oversized : scenario == 3 ? "{bad}\n" :
                                     "{\"result\":[{\"Path\":\"/sync\"}]}\n",
                         .stall = stall, .optional = scenario == 4};
        struct sockaddr_un address = {.sun_family = AF_UNIX};
        g_strlcpy(address.sun_path, endpoint, sizeof(address.sun_path));
        g_assert_cmpint(bind(server.listener, (struct sockaddr *)&address, sizeof(address)), ==, 0);
        g_assert_cmpint(listen(server.listener, 1), ==, 0);
        GThread *thread = g_thread_new("admin-fixture", serve, &server);
        Synctrain *self = provider();
        self->discovered_at = 0;
        TestFile *file = new_file("file:///sync/a", FALSE);
        gint64 start = g_get_monotonic_time();
        ensure_roots(self, NAUTILUS_FILE_INFO(file));
        g_assert_cmpint(g_get_monotonic_time() - start, <, 100 * 1000);
        drain_until(discovered, self);
        g_assert_cmpuint(self->roots->folders->len, ==, scenario == 0 ? 1 : 0);
        g_assert_cmpuint(file->invalidated, ==, 1);
        g_assert_cmpint(g_get_monotonic_time() - start, <, G_USEC_PER_SEC);
        g_thread_join(thread);
        close(server.listener);
        g_unlink(endpoint);
        g_object_unref(file);
        g_object_unref(self);
    }
    set_helper("#!/bin/sh\nprintf 'not JSON'\n");
    SynctrainRoots *roots = synctrain_discover();
    g_assert_cmpuint(roots->folders->len, ==, 0);
    synctrain_roots_free(roots);
    set_helper("#!/bin/sh\nexec sleep 30\n");
    gint64 start = g_get_monotonic_time();
    roots = synctrain_discover();
    g_assert_cmpint(g_get_monotonic_time() - start, <, 3 * G_USEC_PER_SEC);
    synctrain_roots_free(roots);
    g_assert_cmpint(g_remove(gateway_script), ==, 0);
    roots = synctrain_discover();
    g_assert_cmpuint(roots->folders->len, ==, 0);
    synctrain_roots_free(roots);
}

static gboolean log_exists(gpointer data) { return g_file_test(data, G_FILE_TEST_EXISTS); }
static void test_versions(void)
{
    g_autofree char *binary = g_build_filename(fixture_dir, "synctrain", NULL);
    g_autofree char *log = g_build_filename(fixture_dir, "arguments", NULL);
    g_autofree char *script = g_strdup_printf("#!/bin/sh\nset -e\nprintf '%%s\\n' \"$@\" > '%s.tmp'\nmv '%s.tmp' '%s'\n", log, log, log);
    g_assert_true(g_file_set_contents(binary, script, -1, NULL));
    g_assert_cmpint(g_chmod(binary, 0700), ==, 0);
    const char *path = "/sync/a b;$(literal).txt";
    NautilusMenuItem *item = versions_item(path);
    nautilus_menu_item_activate(item);
    g_object_unref(item);
    drain_until(log_exists, log);
    g_autofree char *arguments = NULL;
    g_assert_true(g_file_get_contents(log, &arguments, NULL, NULL));
    g_assert_cmpstr(arguments, ==, "versions\n/sync/a b;$(literal).txt\n");
    g_remove(log);
    g_remove(binary);
}

static void load_module(const char *path)
{
    GModule *library = g_module_open(path, G_MODULE_BIND_LOCAL);
    g_assert_nonnull(library);
    void (*initialize)(GTypeModule *) = NULL;
    void (*list_types)(const GType **, int *) = NULL;
    void (*shutdown)(void) = NULL;
    g_assert_true(g_module_symbol(library, "nautilus_module_initialize", (gpointer *)&initialize));
    g_assert_true(g_module_symbol(library, "nautilus_module_list_types", (gpointer *)&list_types));
    g_assert_true(g_module_symbol(library, "nautilus_module_shutdown", (gpointer *)&shutdown));
    GTypeModule *module = g_object_new(test_module_get_type(), NULL);
    g_assert_true(g_type_module_use(module));
    initialize(module);
    const GType *types = NULL;
    int count = 0;
    list_types(&types, &count);
    g_assert_cmpint(count, ==, 1);
    GObject *object = g_object_new(types[0], NULL);
    g_assert_true(NAUTILUS_IS_MENU_PROVIDER(object));
    g_assert_true(NAUTILUS_IS_INFO_PROVIDER(object));
    g_assert_true(NAUTILUS_IS_COLUMN_PROVIDER(object));
    GList *columns = nautilus_column_provider_get_columns(NAUTILUS_COLUMN_PROVIDER(object));
    g_assert_cmpuint(g_list_length(columns), ==, 1);
    g_list_free_full(columns, g_object_unref);
    g_object_unref(object);
    shutdown();
    /* Registered dynamic types outlive this harness; keep their module resident. */
    g_module_make_resident(library);
    g_module_close(library);
}

int main(int argc, char **argv)
{
    if (argc == 3 && strcmp(argv[1], "--load-module") == 0) {
        load_module(argv[2]);
        return 0;
    }
    g_assert_cmpint(argc, ==, 2);
    g_autoptr(GSubprocess) loader = g_subprocess_new(G_SUBPROCESS_FLAGS_NONE, NULL, argv[0], "--load-module", argv[1], NULL);
    g_assert_nonnull(loader);
    g_assert_true(g_subprocess_wait_check(loader, NULL, NULL));
    /* Establish PATH before GLib workers start; keep the real environment isolated. */
    fixture_dir = g_dir_make_tmp("synctrain-native-tools-XXXXXX", NULL);
    gateway_script = g_build_filename(fixture_dir, "synctrain-gateway", NULL);
    g_autofree char *path = g_strconcat(fixture_dir, ":", g_getenv("PATH"), NULL);
    g_setenv("PATH", path, TRUE);
    GTypeModule *module = g_object_new(test_module_get_type(), NULL);
    g_assert_true(g_type_module_use(module));
    nautilus_module_initialize(module);
    argc = 1;
    g_test_init(&argc, &argv, NULL);
    g_test_add_func("/nautilus/paths", test_paths);
    g_test_add_func("/nautilus/json", test_json);
    g_test_add_func("/nautilus/menus", test_menus);
    g_test_add_func("/nautilus/synchronized", test_synchronized_menus);
    g_test_add_func("/nautilus/columns", test_columns);
    g_test_add_func("/nautilus/retention", test_retention);
    g_test_add_func("/nautilus/retention-order", test_retention_order);
    g_test_add_func("/nautilus/metadata-location", test_metadata_location);
    g_test_add_func("/nautilus/metadata-invalidation", test_metadata_invalidation);
    g_test_add_func("/nautilus/versions", test_versions);
    g_test_add_func("/nautilus/discovery", test_discovery);
    int result = g_test_run();
    while (g_main_context_iteration(NULL, FALSE)) {}
    g_remove(gateway_script);
    g_rmdir(fixture_dir);
    g_free(gateway_script);
    g_free(fixture_dir);
    return result;
}

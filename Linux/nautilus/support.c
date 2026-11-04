/* Copyright (C) 2026 Gordon. SPDX-License-Identifier: MPL-2.0 */
#include "support.h"

#include <errno.h>
#include <fcntl.h>
#include <gio/gunixinputstream.h>
#include <json-glib/json-glib.h>
#include <poll.h>
#include <stddef.h>
#include <string.h>
#include <sys/socket.h>
#include <sys/un.h>
#include <sys/xattr.h>
#include <unistd.h>

char *synctrain_local_path(const char *uri)
{
    g_autofree char *host = NULL;
    g_autofree char *path = g_filename_from_uri(uri, &host, NULL);
    if (!path || (host && *host && g_ascii_strcasecmp(host, "localhost") != 0))
        return NULL;
    return g_canonicalize_filename(path, NULL);
}

gboolean synctrain_under(const char *path, GPtrArray *roots)
{
    if (!path || !roots)
        return FALSE;
    for (guint i = 0; i < roots->len; i++) {
        const char *root = g_ptr_array_index(roots, i);
        size_t len = strlen(root);
        if (g_str_has_prefix(path, root) &&
            (strcmp(root, "/") == 0 ? path[1] != '\0' : path[len] == '/'))
            return TRUE;
    }
    return FALSE;
}

GPtrArray *synctrain_parse_mounts(const char *text)
{
    GPtrArray *paths = g_ptr_array_new_with_free_func(g_free);
    g_auto(GStrv) lines = g_strsplit(text, "\n", -1);
    for (guint i = 0; lines[i]; i++) {
        g_auto(GStrv) fields = g_strsplit_set(lines[i], " \t", -1);
        if (g_strv_length(fields) < 3 || strcmp(fields[2], "fuse.synctrain") != 0)
            continue;
        GString *path = g_string_new(NULL);
        for (const char *p = fields[1]; *p; p++) {
            if (*p == '\\' && strlen(p) >= 4 && p[1] >= '0' && p[1] <= '7' &&
                p[2] >= '0' && p[2] <= '7' && p[3] >= '0' && p[3] <= '7') {
                g_string_append_c(path, (p[1] - '0') * 64 + (p[2] - '0') * 8 + p[3] - '0');
                p += 3;
            } else {
                g_string_append_c(path, *p);
            }
        }
        g_ptr_array_add(paths, g_string_free(path, FALSE));
    }
    return paths;
}

static JsonObject *parse_object(JsonParser *parser, const char *text)
{
    if (!json_parser_load_from_data(parser, text, -1, NULL))
        return NULL;
    JsonNode *node = json_parser_get_root(parser);
    return JSON_NODE_HOLDS_OBJECT(node) ? json_node_get_object(node) : NULL;
}

static const char *string_member(JsonObject *object, const char *key)
{
    JsonNode *node = json_object_get_member(object, key);
    return node && JSON_NODE_HOLDS_VALUE(node) && json_node_get_value_type(node) == G_TYPE_STRING
        ? json_node_get_string(node) : NULL;
}

GPtrArray *synctrain_parse_folders(const char *text)
{
    GPtrArray *paths = g_ptr_array_new_with_free_func(g_free);
    g_autoptr(JsonParser) parser = json_parser_new();
    JsonObject *object = parse_object(parser, text);
    JsonNode *node = object ? json_object_get_member(object, "result") : NULL;
    if (!node || !JSON_NODE_HOLDS_ARRAY(node))
        return paths;
    JsonArray *array = json_node_get_array(node);
    for (guint i = 0; i < json_array_get_length(array); i++) {
        node = json_array_get_element(array, i);
        if (!JSON_NODE_HOLDS_OBJECT(node))
            continue;
        const char *path = string_member(json_node_get_object(node), "Path");
        if (path && g_path_is_absolute(path))
            g_ptr_array_add(paths, g_canonicalize_filename(path, NULL));
    }
    return paths;
}

/* All waits share an absolute deadline, including partial/trickled responses. */
static gboolean wait_fd(int fd, short events, gint64 deadline)
{
    struct pollfd descriptor = { .fd = fd, .events = events };
    for (;;) {
        gint64 remaining = deadline - g_get_monotonic_time();
        if (remaining <= 0)
            return FALSE;
        int result = poll(&descriptor, 1, (int)((remaining + 999) / 1000));
        if (result > 0)
            return (descriptor.revents & (events | POLLHUP)) != 0;
        if (result == 0 || errno != EINTR)
            return FALSE;
    }
}

static char *read_response(int fd, gint64 deadline, gboolean line)
{
    g_autoptr(GString) text = g_string_new(NULL);
    while (text->len < SYNCTRAIN_RESPONSE_LIMIT) {
        char buffer[4096];
        if (!wait_fd(fd, POLLIN, deadline))
            return NULL;
        ssize_t count = read(fd, buffer, MIN(sizeof(buffer), SYNCTRAIN_RESPONSE_LIMIT - text->len));
        if (count < 0) {
            if (errno == EINTR || errno == EAGAIN)
                continue;
            return NULL;
        }
        if (count == 0)
            return line ? NULL : g_string_free(g_steal_pointer(&text), FALSE);
        if (memchr(buffer, '\0', count))
            return NULL;
        g_string_append_len(text, buffer, count);
        char *newline = line ? strchr(text->str, '\n') : NULL;
        if (newline) {
            *newline = '\0';
            return g_string_free(g_steal_pointer(&text), FALSE);
        }
    }
    return NULL;
}

static char *home_socket(void)
{
    g_autoptr(GSubprocess) process = g_subprocess_new(
        G_SUBPROCESS_FLAGS_STDOUT_PIPE | G_SUBPROCESS_FLAGS_STDERR_SILENCE, NULL,
        "synctrain-gateway", "home", "--json", NULL);
    if (!process)
        return NULL;
    int fd = g_unix_input_stream_get_fd(G_UNIX_INPUT_STREAM(g_subprocess_get_stdout_pipe(process)));
    g_autofree char *response = read_response(fd, g_get_monotonic_time() + 2 * G_USEC_PER_SEC, FALSE);
    /* Reap asynchronously even if a helper closes stdout before exiting. */
    g_subprocess_force_exit(process);
    g_subprocess_wait_async(process, NULL, NULL, NULL);
    g_autoptr(JsonParser) parser = json_parser_new();
    JsonObject *object = response ? parse_object(parser, response) : NULL;
    return object ? g_strdup(string_member(object, "socket")) : NULL;
}

static char *folder_response(const char *endpoint)
{
    struct sockaddr_un address = { .sun_family = AF_UNIX };
    if (!endpoint || !g_path_is_absolute(endpoint) || strlen(endpoint) >= sizeof(address.sun_path))
        return NULL;
    g_strlcpy(address.sun_path, endpoint, sizeof(address.sun_path));
    int fd = socket(AF_UNIX, SOCK_STREAM | SOCK_NONBLOCK | SOCK_CLOEXEC, 0);
    if (fd < 0)
        return NULL;
    char *response = NULL;
    gint64 deadline = g_get_monotonic_time() + 200 * 1000;
    int connected = connect(fd, (struct sockaddr *)&address, sizeof(address));
    if (connected < 0 && errno != EINPROGRESS && errno != EAGAIN)
        goto done;
    if (!wait_fd(fd, POLLOUT, deadline))
        goto done;
    int error = 0;
    socklen_t size = sizeof(error);
    if (getsockopt(fd, SOL_SOCKET, SO_ERROR, &error, &size) != 0 || error)
        goto done;
    const char request[] = "{\"method\":\"engine.folders\"}\n";
    size_t sent = 0;
    while (sent < sizeof(request) - 1) {
        if (!wait_fd(fd, POLLOUT, deadline))
            goto done;
        ssize_t count = send(fd, request + sent, sizeof(request) - 1 - sent, MSG_NOSIGNAL);
        if (count < 0 && (errno == EINTR || errno == EAGAIN))
            continue;
        if (count <= 0)
            goto done;
        sent += count;
    }
    response = read_response(fd, deadline, TRUE);
done:
    close(fd);
    return response;
}

SynctrainRoots *synctrain_discover(void)
{
    SynctrainRoots *roots = g_new0(SynctrainRoots, 1);
    g_autofree char *mounts = NULL;
    g_file_get_contents("/proc/self/mounts", &mounts, NULL, NULL);
    roots->mounts = synctrain_parse_mounts(mounts ? mounts : "");
    g_autofree char *endpoint = home_socket();
    g_autofree char *response = folder_response(endpoint);
    roots->folders = synctrain_parse_folders(response ? response : "{}");
    return roots;
}

void synctrain_roots_free(SynctrainRoots *roots)
{
    g_ptr_array_unref(roots->mounts);
    g_ptr_array_unref(roots->folders);
    g_free(roots);
}

const char *synctrain_state_label(const char *state)
{
    static const char *const states[][2] = {
        {"remote", "On the gateway"}, {"partial", "Partly here"}, {"local", "Here"},
        {"kept", "Kept here"}, {"pending", "Sending"}, {"failed", "Can't be sent"},
        {"blocked", "Can't be sent"}, {"local-only", "Only here"},
    };
    for (guint i = 0; i < G_N_ELEMENTS(states); i++)
        if (g_strcmp0(state, states[i][0]) == 0)
            return states[i][1];
    return "";
}

char *synctrain_read_attribute(const char *path, const char *name)
{
    char buffer[128];
    ssize_t count = getxattr(path, name, buffer, sizeof(buffer) - 1);
    return count >= 0 ? g_strndup(buffer, count) : NULL;
}

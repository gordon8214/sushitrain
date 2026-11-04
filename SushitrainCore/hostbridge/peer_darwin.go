// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
//go:build darwin && cgo

package hostbridge

/*
#cgo LDFLAGS: -framework Security -framework CoreFoundation
#include <Security/Security.h>
#include <CoreFoundation/CoreFoundation.h>
#include <sys/socket.h>
#include <sys/un.h>
#include <unistd.h>
#include <bsm/audit.h>

static int authorize_mcp_peer(int fd) {
    uid_t uid; gid_t gid;
    if (getpeereid(fd, &uid, &gid) != 0 || uid != getuid()) return 0;
    audit_token_t token; socklen_t length = sizeof(token);
    if (getsockopt(fd, SOL_LOCAL, LOCAL_PEERTOKEN, &token, &length) != 0 || length != sizeof(token)) return 0;
    CFDataRef data = CFDataCreate(NULL, (const UInt8 *)&token, sizeof(token));
    const void *keys[] = { kSecGuestAttributeAudit };
    const void *values[] = { data };
    CFDictionaryRef attributes = CFDictionaryCreate(NULL, keys, values, 1, &kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);
    SecCodeRef peer = NULL, own = NULL;
    CFDictionaryRef info = NULL;
    SecRequirementRef requirement = NULL;
    int allowed = 0;
    if (SecCodeCopyGuestWithAttributes(NULL, attributes, kSecCSDefaultFlags, &peer) == errSecSuccess &&
        SecCodeCopySelf(kSecCSDefaultFlags, &own) == errSecSuccess &&
        SecCodeCopySigningInformation(own, kSecCSSigningInformation, &info) == errSecSuccess) {
        CFStringRef team = CFDictionaryGetValue(info, kSecCodeInfoTeamIdentifier);
        if (team) {
            CFStringRef rule = CFStringCreateWithFormat(NULL, NULL, CFSTR("anchor apple generic and identifier \"gordonbyrnes.Synctrain.MCP\" and certificate leaf[subject.OU] = \"%@\""), team);
            if (SecRequirementCreateWithString(rule, kSecCSDefaultFlags, &requirement) == errSecSuccess)
                allowed = SecCodeCheckValidity(peer, kSecCSDefaultFlags, requirement) == errSecSuccess;
            CFRelease(rule);
        }
    }
    if (requirement) CFRelease(requirement);
    if (info) CFRelease(info);
    if (own) CFRelease(own);
    if (peer) CFRelease(peer);
    CFRelease(attributes); CFRelease(data);
    return allowed;
}
*/
import "C"
import (
	"errors"
	"net"
	"syscall"
)

func authorizePeer(conn net.Conn) error {
	peer, ok := conn.(syscall.Conn)
	if !ok {
		return errors.New("cannot authenticate MCP peer")
	}
	raw, err := peer.SyscallConn()
	if err != nil {
		return err
	}
	allowed := false
	err = raw.Control(func(fd uintptr) { allowed = C.authorize_mcp_peer(C.int(fd)) != 0 })
	if err != nil {
		return err
	}
	if !allowed {
		return errors.New("MCP requires the signed Synctrain helper from this team")
	}
	return nil
}

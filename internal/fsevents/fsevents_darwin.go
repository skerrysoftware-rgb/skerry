// Package fsevents watches a directory tree with macOS FSEvents.
//
// virtio-fs doesn't carry inotify events from the host into the guest, so
// file watchers in a VM (dev servers, test runners) never see edits made
// on the Mac. skerry watches the checkout on the host and has the agent
// re-stamp each changed file in the guest, which raises the event there.
package fsevents

/*
#cgo LDFLAGS: -framework CoreServices
#include <CoreServices/CoreServices.h>
#include <dispatch/dispatch.h>
#include <stdlib.h>
static void noop(void *ctx) {}

extern void goFSEvents(uintptr_t handle, size_t n, char **paths, FSEventStreamEventFlags *flags);

static void callback(ConstFSEventStreamRef stream, void *info, size_t n, void *paths,
                     const FSEventStreamEventFlags flags[], const FSEventStreamEventId ids[]) {
	goFSEvents((uintptr_t)info, n, (char **)paths, (FSEventStreamEventFlags *)flags);
}

static FSEventStreamRef start(uintptr_t handle, const char *dir, double latency, dispatch_queue_t *qout) {
	CFStringRef path = CFStringCreateWithCString(NULL, dir, kCFStringEncodingUTF8);
	if (path == NULL) return NULL;
	CFArrayRef paths = CFArrayCreate(NULL, (const void **)&path, 1, &kCFTypeArrayCallBacks);
	CFRelease(path);
	FSEventStreamContext ctx = {0, (void *)handle, NULL, NULL, NULL};
	FSEventStreamRef s = FSEventStreamCreate(NULL, callback, &ctx, paths, kFSEventStreamEventIdSinceNow,
		latency, kFSEventStreamCreateFlagFileEvents | kFSEventStreamCreateFlagNoDefer);
	CFRelease(paths);
	if (s == NULL) return NULL;
	dispatch_queue_t q = dispatch_queue_create("skerry.fsevents", DISPATCH_QUEUE_SERIAL);
	FSEventStreamSetDispatchQueue(s, q);
	if (!FSEventStreamStart(s)) {
		FSEventStreamInvalidate(s);
		FSEventStreamRelease(s);
		dispatch_release(q);
		return NULL;
	}
	*qout = q;
	return s;
}

static void stop(FSEventStreamRef s, dispatch_queue_t q) {
	FSEventStreamStop(s);
	FSEventStreamInvalidate(s);
	dispatch_sync_f(q, NULL, noop); // wait out a callback in progress or queued
	FSEventStreamRelease(s);
	dispatch_release(q);
}
*/
import "C"

import (
	"errors"
	"runtime/cgo"
	"sync"
	"unsafe"
)

// Event flags that mean a file's contents or existence changed. A change
// to metadata alone (which the guest's re-stamping causes) is not one.
const contentFlags = C.kFSEventStreamEventFlagItemCreated | C.kFSEventStreamEventFlagItemRemoved |
	C.kFSEventStreamEventFlagItemRenamed | C.kFSEventStreamEventFlagItemModified

// Watcher reports changed paths under a directory.
type Watcher struct {
	stream C.FSEventStreamRef
	handle cgo.Handle
	queue  C.dispatch_queue_t
	once   sync.Once
}

// Watch starts watching dir; changed paths (absolute) go to fn, called on
// one goroutine at a time, about latency seconds after the change.
func Watch(dir string, latency float64, fn func(paths []string)) (*Watcher, error) {
	w := &Watcher{}
	w.handle = cgo.NewHandle(fn)
	cdir := C.CString(dir)
	defer C.free(unsafe.Pointer(cdir))
	w.stream = C.start(C.uintptr_t(w.handle), cdir, C.double(latency), &w.queue)
	if w.stream == nil {
		w.handle.Delete()
		return nil, errors.New("FSEvents refused to watch " + dir)
	}
	return w, nil
}

// Close stops watching.
func (w *Watcher) Close() {
	w.once.Do(func() {
		C.stop(w.stream, w.queue)
		w.handle.Delete()
	})
}

//export goFSEvents
func goFSEvents(handle C.uintptr_t, n C.size_t, paths **C.char, flags *C.FSEventStreamEventFlags) {
	fn := cgo.Handle(handle).Value().(func([]string))
	ps := unsafe.Slice(paths, int(n))
	fs := unsafe.Slice(flags, int(n))
	var changed []string
	for i := range ps {
		if fs[i]&contentFlags != 0 {
			changed = append(changed, C.GoString(ps[i]))
		}
	}
	if len(changed) > 0 {
		fn(changed)
	}
}

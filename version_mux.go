package main

import (
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"
)

const versionExpiry = 5 * time.Minute

// A versionMux handles routing requests to the various versions we have
// available. It handles two specific problems that crop up while routing
// requests.
//
// First, it has the concept of "prepared" versions - versions that can serve
// local requests, but for which the cluster doesn't forward requests to by
// default. To make any version upgrade monotonic, we need to be able to (for a
// very short time) handle requests for version N-1 to outside clients, while
// responding to requests for version N if they are proxied from other nodes who
// have upgraded before us - and vice versa for clients who upgrade after us.
//
// Second, it has a scheme for making sure that all requests are finished to
// a version before closing it out. This is implemented with a timer and a
// reference count. The timer starts when we mark a version "ready to delete"
// and is reset every time a request comes in, so that a version still serving
// frequent requests will never be closed. Even once we remove a version from
// the mux, in-flight requests might have a pointer to the version, so we need
// to increment a reference count to the version when we pass one out and
// decrement it after the request is done.
type versionMux struct {
	versions       map[string]versionReferenceCount
	currentVersion versionReferenceCount
	lock           sync.RWMutex
}

type versionReferenceCount struct {
	*version
	count      *sync.WaitGroup
	closeTimer *time.Timer
}

func newVersionMux() *versionMux {
	return &versionMux{versions: make(map[string]versionReferenceCount)}
}

// ServeHTTP implements http.Handler.
func (mux *versionMux) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	proxyVersion := r.URL.Query().Get("proxy")
	var vs *version

	if proxyVersion != "" {
		vs = mux.getVersion(proxyVersion)
		if vs == nil {
			log.Println("Got proxied request for unavailable version:", proxyVersion)
			vs = mux.getCurrent()
		}
	} else {
		vs = mux.getCurrent()
	}

	if vs == nil {
		panic("no version prepared")
	}

	vs.ServeHTTP(w, r)
	mux.release(vs)
}

// getCurrent returns the current version and increments the reference count
// for it. It returns nil if there is no prepared version.
func (mux *versionMux) getCurrent() *version {
	mux.lock.RLock()
	defer mux.lock.RUnlock()

	vs := mux.currentVersion
	if vs.version != nil {
		vs.count.Add(1)
		if vs.closeTimer != nil {
			vs.closeTimer.Reset(versionExpiry)
		}
	}

	return vs.version
}

// getVersion returns the version that matches the given name, and increments
// the reference count for it. It returns nil if there is no version matching
// that name.
func (mux *versionMux) getVersion(name string) *version {
	mux.lock.RLock()
	defer mux.lock.RUnlock()

	vs := mux.versions[name]
	if vs.version != nil {
		vs.count.Add(1)
		if vs.closeTimer != nil {
			vs.closeTimer.Reset(versionExpiry)
		}
	}

	return vs.version
}

// release signifies that a request is done with a version, decrementing the
// reference count.
func (mux *versionMux) release(version *version) {
	if version == nil {
		return
	}

	mux.lock.RLock()
	defer mux.lock.RUnlock()

	if vs, ok := mux.versions[version.name]; ok {
		vs.count.Done()
	}
}

// prepare puts a new version in the wings, allowing requests to be routed to it
// but not setting it as the default. If the passed version is already prepared,
// this method will panic.
func (mux *versionMux) prepare(version *version) {
	mux.lock.Lock()
	defer mux.lock.Unlock()

	if _, ok := mux.versions[version.name]; ok {
		panic(fmt.Sprintf("version already prepared: %s", version.name))
	}

	mux.versions[version.name] = versionReferenceCount{
		version: version,
		count:   new(sync.WaitGroup),
	}
}

// upgrade switches the given version to the current default. If the given
// version hasn't been prepared, or if it currently has a different version
// by the same name, this method will panic.
func (mux *versionMux) upgrade(version *version) {
	mux.lock.Lock()
	defer mux.lock.Unlock()

	vs := mux.mustGet(version)
	mux.currentVersion = vs
}

// remove starts a timer that will remove a version from the mux. Any time a
// proxied request comes in for the version, we reset the timer, so it must
// be completely unused for the full period of time before it will get removed
// from the mux (if shouldWait is false, this step is skipped). Once we remove
// it from the mux, we make extra sure that nothing is using it by waiting for
// the reference count to drop to zero.
func (mux *versionMux) remove(version *version, shouldWait bool) {
	if version == nil {
		return
	}

	// Set the timer, then wait for it. Any request from here on will reset the
	// timer.
	if shouldWait {
		mux.lock.Lock()

		vs := mux.mustGet(version)
		vs.closeTimer = time.NewTimer(versionExpiry)

		mux.lock.Unlock()

		// Wait for the timer, which is reset on every request.
		<-vs.closeTimer.C
	}

	mux.lock.Lock()
	vs := mux.mustGet(version)
	delete(mux.versions, vs.version.name)
	mux.lock.Unlock()

	// Wait for the reference count to drop to zero.
	vs.count.Wait()
}

func (mux *versionMux) mustGet(version *version) versionReferenceCount {
	vs, ok := mux.versions[version.name]
	if !ok {
		panic(fmt.Sprintf("version doesn't exist: %s", version.name))
	} else if vs.version != version {
		panic(fmt.Sprintf("somehow got another reference to the same version: %s", version.name))
	}

	return vs
}

package bot

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A face that was cut once is answered without Mojang from then on: after a restart from
// disk, and when the cache has run out the old one at once while the new one is fetched.
func TestFaceIsKeptOnDiskAndAnsweredAtOnce(t *testing.T) {
	mj := fakeMojang(t)
	dir := t.TempDir()
	mk := func() *heads {
		h := newHeads()
		h.dir = dir
		h.profileAPI, h.sessionAPI, h.textures = mj.URL+"/users/profiles/minecraft/", mj.URL+"/session/", mj.URL+"/texture/"
		return h
	}
	ctx := context.Background()

	first := mk()
	face, found := first.faceNow(ctx, "Alice")
	if !found || bytes.Equal(face, placeholderFace()) {
		t.Fatal("the first request should wait for Mojang and get the real face")
	}
	onDisk, err := os.ReadFile(filepath.Join(dir, "alice.png"))
	if err != nil || !bytes.Equal(onDisk, face) {
		t.Fatalf("the face should be on disk: %v", err)
	}

	// after a restart Mojang is not reachable: the face still comes, and at once
	mj.Close()
	second := mk()
	got, found := second.faceNow(ctx, "alice")
	if !found || !bytes.Equal(got, face) {
		t.Fatal("after a restart the face should come from disk")
	}

	// the cache has run out: the old face at once, not the placeholder
	first.mu.Lock()
	first.entries["alice"].at = time.Now().Add(-2 * headTTL)
	first.mu.Unlock()
	got, found = first.faceNow(ctx, "Alice")
	if !found || !bytes.Equal(got, face) {
		t.Fatal("an old face should be served while the new one is fetched")
	}

	// a name never seen, with Mojang away: the placeholder, and nothing is kept
	got, _ = second.faceNow(ctx, "Bobby")
	if !bytes.Equal(got, placeholderFace()) {
		t.Fatal("an unknown name without Mojang should get the plain face")
	}
	if _, err := os.Stat(filepath.Join(dir, "bobby.png")); err == nil {
		t.Fatal("the plain face must not be kept as somebody's face")
	}
	if got, _ := second.faceNow(ctx, "../x"); !bytes.Equal(got, placeholderFace()) {
		t.Fatal("a name that is not a Minecraft name never reaches the disk")
	}
}

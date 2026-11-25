package gateway

// A request's inline images, kept apart from its text for the views that
// show the request again: the Routing card and the Sessions page
// (matrix0487 on Discord). Sending images to DeepSeek by file id (#5407)
// let a conversation go past the 64 MiB DeepSeek takes inline, and its
// bodies past what those views keep: a body over promptBodyMax lost the
// card's text and pictures, and one over conversationBodyMax its turn.
// Almost all of such a body is base64, so each view keeps it with every
// picture swapped for a short ref, and the card reads the picture back
// from keptImages, in memory only and bounded, as the bodies are.

import (
	"bytes"
	"container/list"
	"crypto/sha256"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// imageRef stands in a lean body for a picture's base64, followed by
	// its id in keptImages; never sent anywhere, and not base64, so a
	// picture's checks never take it for one
	imageRef = "magpie-image:"
	// leanMin is the shortest string swapped: a smaller picture costs
	// less than its ref's bookkeeping
	leanMin = 1 << 10
	// keptImagesMax is what the kept pictures take at most together, the
	// least recently used going first
	keptImagesMax = 128 << 20
	// keptImagesTTL is how long an unviewed picture stays in keptImages
	keptImagesTTL = 6 * time.Hour
)

type keptImage struct {
	id   int64
	sum  [32]byte
	b64  string
	when time.Time
}

type keptImageStore struct {
	sync.Mutex
	bySum map[[32]byte]*list.Element
	byID  map[int64]*list.Element
	lru   *list.List // front: the newest
	size  int
	cap   int
	ttl   time.Duration
	now   func() time.Time
	sweep expirySweep
	next  int64
}

func newKeptImageStore(cap int, ttl time.Duration) *keptImageStore {
	return &keptImageStore{
		bySum: map[[32]byte]*list.Element{},
		byID:  map[int64]*list.Element{},
		lru:   list.New(),
		cap:   cap,
		ttl:   ttl,
		now:   time.Now,
	}
}

var keptImages = newKeptImageStore(keptImagesMax, keptImagesTTL)

// keepImage keeps a picture's base64, giving its id; the same picture
// keeps one id.
func keepImage(b64 []byte) int64 {
	return keptImages.put(b64)
}

func (k *keptImageStore) put(b64 []byte) int64 {
	sum := sha256.Sum256(b64)
	k.Lock()
	defer k.Unlock()
	now := k.now()
	if e, ok := k.bySum[sum]; ok {
		e.Value.(*keptImage).when = now
		k.lru.MoveToFront(e)
		return e.Value.(*keptImage).id
	}
	k.next++
	im := &keptImage{id: k.next, sum: sum, b64: string(b64), when: now}
	e := k.lru.PushFront(im)
	k.bySum[sum], k.byID[im.id] = e, e
	k.size += len(im.b64)
	for k.size > k.cap && k.lru.Len() > 1 {
		k.dropLocked(k.lru.Back())
	}
	if delay, more := k.pruneExpiredLocked(now); more {
		k.sweep.arm(delay, k.sweepExpired)
	}
	return im.id
}

// keptImageOf is the base64 of the picture a ref names; false when it
// has gone.
func keptImageOf(ref string) (string, bool) {
	id, err := strconv.ParseInt(ref, 10, 64)
	if err != nil {
		return "", false
	}
	return keptImages.get(id)
}

func (k *keptImageStore) get(id int64) (string, bool) {
	k.Lock()
	defer k.Unlock()
	e, ok := k.byID[id]
	if !ok {
		return "", false
	}
	now := k.now()
	im := e.Value.(*keptImage)
	if k.ttl > 0 && now.Sub(im.when) > k.ttl {
		k.dropLocked(e)
		return "", false
	}
	im.when = now
	k.lru.MoveToFront(e)
	return im.b64, true
}

func (k *keptImageStore) dropLocked(e *list.Element) {
	old := k.lru.Remove(e).(*keptImage)
	delete(k.bySum, old.sum)
	delete(k.byID, old.id)
	k.size -= len(old.b64)
	if k.lru.Len() == 0 {
		k.sweep.stop()
	}
}

func (k *keptImageStore) pruneExpiredLocked(now time.Time) (time.Duration, bool) {
	if k.ttl <= 0 {
		return 0, false
	}
	for e := k.lru.Back(); e != nil; e = k.lru.Back() {
		age := now.Sub(e.Value.(*keptImage).when)
		if age <= k.ttl {
			return k.ttl - age, true
		}
		k.dropLocked(e)
	}
	return 0, false
}

func (k *keptImageStore) sweepExpired(gen uint64) {
	k.Lock()
	defer k.Unlock()
	if !k.sweep.fire(gen) {
		return
	}
	if delay, more := k.pruneExpiredLocked(k.now()); more {
		k.sweep.arm(delay, k.sweepExpired)
	}
}

// imageMagic is how a PNG, JPEG, GIF or WebP's base64 starts: an
// Anthropic or Gemini source's data, which carries no data: head.
var imageMagic = []string{"iVBORw0KGg", "/9j/", "R0lGOD", "UklGR"}

// imagePayload is where a JSON string's picture base64 starts: after a
// data:image/…;base64, head, or at 0 for bare base64 that starts as a
// picture does. False for anything else.
func imagePayload(s []byte) (int, bool) {
	at := 0
	if bytes.HasPrefix(s, []byte("data:")) {
		head, _, ok := bytes.Cut(s, []byte(","))
		if !ok {
			return 0, false
		}
		h := strings.ToLower(string(head))
		if !strings.HasPrefix(h, "data:image/") || !strings.HasSuffix(h, ";base64") {
			return 0, false
		}
		at = len(head) + 1
	} else {
		magic := false
		for _, m := range imageMagic {
			magic = magic || bytes.HasPrefix(s, []byte(m))
		}
		if !magic {
			return 0, false
		}
	}
	if len(s)-at < leanMin {
		return 0, false
	}
	for _, c := range s[at:] {
		if !('A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '+' || c == '/' || c == '=') {
			return 0, false
		}
	}
	return at, true
}

// leanImages is a JSON body with each inline picture's base64 swapped for
// a ref: kept in keptImages for the card to draw again when keep, a bare
// ref otherwise (the Sessions page shows a picture as [image]). The body
// itself when it has none. Only JSON strings without escapes are read, so
// the body's shape is never changed.
func leanImages(body []byte, keep bool) []byte {
	var out []byte
	last := 0
	for i := 0; i < len(body); i++ {
		if body[i] != '"' {
			continue
		}
		j, escaped := i+1, false
		for ; j < len(body) && body[j] != '"'; j++ {
			if body[j] == '\\' {
				escaped = true
				j++
			}
		}
		if j >= len(body) {
			break
		}
		if s := body[i+1 : j]; !escaped && len(s) >= leanMin {
			if at, ok := imagePayload(s); ok {
				if out == nil {
					out = make([]byte, 0, len(body)/8)
				}
				ref := imageRef
				if keep {
					ref += strconv.FormatInt(keepImage(s[at:]), 10)
				}
				out = append(out, body[last:i+1+at]...)
				out = append(out, ref...)
				last = j
			}
		}
		i = j
	}
	if out == nil {
		return body
	}
	return append(out, body[last:]...)
}

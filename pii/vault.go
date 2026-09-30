package pii

import (
	"container/list"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"strings"
	"sync"
	"time"
)

// vault maps originals to stable surrogates per session. surrogates derive
// from hmac(key, category|normalized original) so they stay identical across
// requests and restarts; collisions re-derive with a counter.

type vaultEntry struct {
	original  string
	surrogate string
	label     Label
	expires   time.Time
	elem      *list.Element
}

type Vault struct {
	mu      sync.Mutex
	key     []byte
	ttl     time.Duration
	maxSize int
	mode    string
	byOrig  map[string]*vaultEntry
	bySurr  map[string]*vaultEntry
	lru     *list.List
	counts  map[string]int
}

func NewVault(key []byte, ttl time.Duration, maxSize int) *Vault {
	return NewVaultMode(key, ttl, maxSize, "realistic")
}

func NewVaultMode(key []byte, ttl time.Duration, maxSize int, mode string) *Vault {
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	if maxSize <= 0 {
		maxSize = 4096
	}
	if mode == "" {
		mode = "realistic"
	}
	k := append([]byte(nil), key...)
	return &Vault{
		key: k, ttl: ttl, maxSize: maxSize, mode: mode,
		byOrig: map[string]*vaultEntry{},
		bySurr: map[string]*vaultEntry{},
		lru:    list.New(),
		counts: map[string]int{},
	}
}

func normOrig(label Label, s string) string {
	return string(label) + "|" + strings.ToLower(strings.Join(strings.Fields(s), " "))
}

func (v *Vault) derive(label Label, original string, salt uint32) string {
	m := hmac.New(sha256.New, v.key)
	m.Write([]byte(normOrig(label, original)))
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], salt)
	m.Write(b[:])
	sum := m.Sum(nil)
	switch v.mode {
	case "variable":
		return variableFor(label, original, sum, v)
	case "label":
		return labelFor(label, sum, v)
	default:
		return fakeFor(label, original, sum)
	}
}

func (v *Vault) For(label Label, original string) string {
	v.mu.Lock()
	defer v.mu.Unlock()
	k := normOrig(label, original)
	if e, ok := v.byOrig[k]; ok {
		if time.Now().Before(e.expires) {
			v.lru.MoveToFront(e.elem)
			return e.surrogate
		}
		v.remove(e)
	}
	var surr string
	for salt := uint32(0); ; salt++ {
		surr = v.derive(label, original, salt)
		if _, taken := v.bySurr[surr]; !taken {
			break
		}
	}
	e := &vaultEntry{original: original, surrogate: surr, label: label, expires: time.Now().Add(v.ttl)}
	e.elem = v.lru.PushFront(k)
	v.byOrig[k] = e
	v.bySurr[surr] = e
	v.counts[string(label)]++
	for v.lru.Len() > v.maxSize {
		back := v.lru.Back()
		if back == nil {
			break
		}
		if e2, ok := v.byOrig[back.Value.(string)]; ok {
			v.remove(e2)
		} else {
			v.lru.Remove(back)
		}
	}
	return surr
}

func (v *Vault) remove(e *vaultEntry) {
	delete(v.byOrig, normOrig(e.label, e.original))
	delete(v.bySurr, e.surrogate)
	if e.elem != nil {
		v.lru.Remove(e.elem)
	}
}

func (v *Vault) Lookup(surrogate string) (string, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	e, ok := v.bySurr[surrogate]
	if !ok || time.Now().After(e.expires) {
		return "", false
	}
	return e.original, true
}

func (v *Vault) Surrogates() []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	out := make([]string, 0, len(v.bySurr))
	for s := range v.bySurr {
		out = append(out, s)
	}
	return out
}

func (v *Vault) MaxSurrogateLen() int {
	n := 0
	for _, s := range v.Surrogates() {
		if len(s) > n {
			n = len(s)
		}
	}
	return n
}

// global session vault registry: id -> vault. sessions expire with ttl.
var (
	vaultsMu sync.Mutex
	vaults   = map[string]*Vault{}
)

func SessionVault(id string, key []byte, ttl time.Duration, maxSize int) *Vault {
	return SessionVaultMode(id, key, ttl, maxSize, "realistic")
}

func SessionVaultMode(id string, key []byte, ttl time.Duration, maxSize int, mode string) *Vault {
	vaultsMu.Lock()
	defer vaultsMu.Unlock()
	if v, ok := vaults[id]; ok {
		return v
	}
	v := NewVaultMode(key, ttl, maxSize, mode)
	vaults[id] = v
	return v
}

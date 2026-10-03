package pii

import (
	"container/list"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
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
	// bound the salt loop: small labels (url has 5 fakes, phone ~7M) can
	// exhaust their pool, and an unbounded loop then spins forever at 100%
	// cpu, hanging the request. after maxSurrogateSalts tries, uniquify
	// mints a free surrogate deterministically.
	var surr string
	for salt := uint32(0); salt < maxSurrogateSalts; salt++ {
		surr = v.derive(label, original, salt)
		if _, taken := v.bySurr[surr]; !taken {
			break
		}
	}
	if _, taken := v.bySurr[surr]; taken {
		surr = v.uniquify(label, original)
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

// SeedFixed registers a fixed surrogate -> original pair (entity
// `replacement:` values). fixed pairs never expire with the vault ttl
// and are returned by Lookup/Surrogates like derived ones.
func (v *Vault) SeedFixed(surrogate, original string, label Label) {
	if surrogate == "" || original == "" {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if _, taken := v.bySurr[surrogate]; taken {
		return
	}
	e := &vaultEntry{original: original, surrogate: surrogate, label: label, expires: time.Now().Add(100 * 365 * 24 * time.Hour)}
	v.byOrig[normOrig(label, original)] = e
	v.bySurr[surrogate] = e
}

// maxSurrogateSalts caps the hmac salt-retry loop in Vault.For.
const maxSurrogateSalts = 64

// uniquify mints a free surrogate when derive's fake pool is exhausted:
// the base fake plus a "~n" index. deterministic for a given (label,
// original) and vault state, and the index space is unbounded so a free
// candidate always exists.
func (v *Vault) uniquify(label Label, original string) string {
	base := v.derive(label, original, 0)
	for n := 2; ; n++ {
		cand := base + "~" + fmt.Sprint(n)
		if _, taken := v.bySurr[cand]; !taken {
			return cand
		}
	}
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
	vaultsMu  sync.Mutex
	vaults    = map[string]*vaultEntry2{}
	vaultsLRU = list.New()
)

// maxVaults mirrors maxGuards: the session vault registry is keyed by
// the same attacker-influenced session id.
const maxVaults = 512

type vaultEntry2 struct {
	vault *Vault
	elem  *list.Element
}

func SessionVault(id string, key []byte, ttl time.Duration, maxSize int) *Vault {
	return SessionVaultMode(id, key, ttl, maxSize, "realistic")
}

func SessionVaultMode(id string, key []byte, ttl time.Duration, maxSize int, mode string) *Vault {
	if len(id) > 128 {
		id = id[:128]
	}
	vaultsMu.Lock()
	defer vaultsMu.Unlock()
	if e, ok := vaults[id]; ok {
		vaultsLRU.MoveToFront(e.elem)
		return e.vault
	}
	v := NewVaultMode(key, ttl, maxSize, mode)
	e := &vaultEntry2{vault: v}
	e.elem = vaultsLRU.PushFront(id)
	vaults[id] = e
	for vaultsLRU.Len() > maxVaults {
		back := vaultsLRU.Back()
		if back == nil {
			break
		}
		delete(vaults, back.Value.(string))
		vaultsLRU.Remove(back)
	}
	return v
}

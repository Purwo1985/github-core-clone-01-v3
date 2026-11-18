// Identity: who the trial's bearer token names.
//
// The suite's scenario is built once from one fixed seed through a
// named-stream PRNG. The viewer login is therefore re-derived at boot by
// running the same generator the suite runs: FNV-1a over the stream label,
// splitmix64 finalisation, xorshift64* draws, and the first/last-name
// tables. The token itself is checked against MAKEFUN_SHOP_TOKEN; the
// login it names comes from this derivation.

package main

const scenarioSeed uint64 = 0x6a09e667f3bcc909

const u64Mask uint64 = 0xffffffffffffffff

var firstNames = []string{
	"ada", "bram", "cora", "dmitri", "elena", "felix", "greta", "hugo", "iris", "jonas", "kira", "lars",
	"mira", "nico", "olga", "pavel", "quinn", "rosa", "sven", "tess", "ulla", "vito", "wren", "yara", "zane",
}

var lastNames = []string{
	"anders", "bauer", "costa", "dahl", "eriksen", "fischer", "garcia", "holm", "iversen", "jansen", "keller",
	"lind", "moreau", "novak", "olsen", "peters", "quist", "rossi", "sato", "turi", "ulrich", "vega", "weber",
	"xu", "yilmaz", "zimmer",
}

// fnv1a64 is FNV-1a, 64-bit: a label's stable seed contribution.
func fnv1a64(text string) uint64 {
	hash := uint64(0xcbf29ce484222325)
	for i := 0; i < len(text); i++ {
		hash ^= uint64(text[i])
		hash = (hash * 0x100000001b3) & u64Mask
	}
	return hash
}

// mix64 is splitmix64's finalizer.
func mix64(value uint64) uint64 {
	z := (value + 0x9e3779b97f4a7c15) & u64Mask
	z = ((z ^ (z >> 30)) * 0xbf58476d1ce4e5b9) & u64Mask
	z = ((z ^ (z >> 27)) * 0x94d049bb133111eb) & u64Mask
	return (z ^ (z >> 31)) & u64Mask
}

// streamSeed is the seed a named stream starts from.
func streamSeed(label string) uint64 {
	return mix64(scenarioSeed ^ fnv1a64(label))
}

// streamRng is the scenario's PRNG: xorshift64*, seeded from a uint64.
type streamRng struct {
	state uint64
}

func newStreamRng(seed uint64) *streamRng {
	masked := seed & u64Mask
	if masked == 0 {
		masked = scenarioSeed
	}
	return &streamRng{state: masked}
}

func (r *streamRng) nextU64() uint64 {
	x := r.state
	x = (x ^ (x >> 12)) & u64Mask
	x = (x ^ (x << 25)) & u64Mask
	x = (x ^ (x >> 27)) & u64Mask
	r.state = x
	return (x * 0x2545f4914f6cdd1d) & u64Mask
}

func (r *streamRng) nextU32() uint32 {
	return uint32(r.nextU64() >> 32)
}

// nextInt is an integer in [0, maxExclusive), unbiased by rejection.
func (r *streamRng) nextInt(maxExclusive int) int {
	if maxExclusive == 1 {
		return 0
	}
	const range32 = 4294967296
	limit := range32 - (range32 % uint64(maxExclusive))
	for {
		value := uint64(r.nextU32())
		if value < limit {
			return int(value % uint64(maxExclusive))
		}
	}
}

func (r *streamRng) pick(items []string) string {
	return items[r.nextInt(len(items))]
}

// Identity is one scenario user.
type Identity struct {
	Login string
	Name  string
	Email string
	ID    int
}

// deriveUsers returns the scenario's users, in generation order: viewer
// first, stranger third.
func deriveUsers(count int) []Identity {
	rng := newStreamRng(streamSeed("users"))
	used := map[string]bool{}
	users := make([]Identity, 0, count)
	for i := 0; i < count; i++ {
		first := rng.pick(firstNames)
		last := rng.pick(lastNames)
		base := first + last
		login := base
		suffix := 2
		for used[login] {
			login = base + "-" + itoa(suffix)
			suffix++
		}
		used[login] = true
		users = append(users, Identity{
			ID:    1000 + i,
			Login: login,
			Name:  first + " " + last,
			Email: login + "@example.invalid",
		})
	}
	return users
}

// deriveIdentities returns the identity the trial's bearer token names,
// plus the stranger (no rights here).
func deriveIdentities() (viewer Identity, stranger Identity) {
	users := deriveUsers(12)
	return users[0], users[2]
}

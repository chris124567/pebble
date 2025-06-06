// Copyright 2021 The LevelDB-Go and Pebble Authors. All rights reserved. Use
// of this source code is governed by a BSD-style license that can be found in
// the LICENSE file.

// Package testkeys provides facilities for generating and comparing
// human-readable test keys for use in tests and benchmarks. This package
// provides a single Comparer implementation that compares all keys generated
// by this package.
//
// Keys generated by this package may optionally have a 'suffix' encoding an
// MVCC timestamp. This suffix is of the form "@<integer>". Comparisons on the
// suffix are performed using integer value, not the byte representation.
package testkeys

import (
	"bytes"
	"cmp"
	"fmt"
	"math"
	"math/rand/v2"
	"regexp"
	"strconv"
	"strings"

	"github.com/cockroachdb/errors"
	"github.com/chris124567/pebble/internal/base"
)

const alpha = "abcdefghijklmnopqrstuvwxyz"

const suffixDelim = '@'

var inverseAlphabet = make(map[byte]int64, len(alpha))

var prefixRE = regexp.MustCompile("[" + alpha + "]+")

// ignoreTimestampSuffix is a suffix that is ignored when comparing keys, but
// not when comparing suffixes. It simulates the CRDB synthetic bit situation
// (see https://github.com/cockroachdb/cockroach/issues/130533).
var ignoreTimestampSuffix = []byte("_synthetic")

func init() {
	for i := range alpha {
		inverseAlphabet[alpha[i]] = int64(i)
	}
}

// MaxSuffixLen is the maximum length of a suffix generated by this package.
var MaxSuffixLen = 1 + len(fmt.Sprintf("%d", int64(math.MaxInt64)))

// Comparer is the comparer for test keys generated by this package.
var Comparer = &base.Comparer{
	ComparePointSuffixes: compareSuffixes,
	CompareRangeSuffixes: compareSuffixes,
	Compare:              compare,
	Equal:                func(a, b []byte) bool { return compare(a, b) == 0 },
	AbbreviatedKey: func(k []byte) uint64 {
		return base.DefaultComparer.AbbreviatedKey(k[:split(k)])
	},
	FormatKey: base.DefaultFormatter,
	Separator: func(dst, a, b []byte) []byte {
		ai := split(a)
		if ai == len(a) {
			return append(dst, a...)
		}
		bi := split(b)
		if bi == len(b) {
			return append(dst, a...)
		}

		// If the keys are the same just return a.
		if bytes.Equal(a[:ai], b[:bi]) {
			return append(dst, a...)
		}
		n := len(dst)
		dst = base.DefaultComparer.Separator(dst, a[:ai], b[:bi])
		// Did it pick a separator different than a[:ai] -- if not we can't do better than a.
		buf := dst[n:]
		if bytes.Equal(a[:ai], buf) {
			return append(dst[:n], a...)
		}
		// The separator is > a[:ai], so return it
		return dst
	},
	Successor: func(dst, a []byte) []byte {
		ai := split(a)
		if ai == len(a) {
			return append(dst, a...)
		}
		n := len(dst)
		dst = base.DefaultComparer.Successor(dst, a[:ai])
		// Did it pick a successor different than a[:ai] -- if not we can't do better than a.
		buf := dst[n:]
		if bytes.Equal(a[:ai], buf) {
			return append(dst[:n], a...)
		}
		// The successor is > a[:ai], so return it.
		return dst
	},
	ImmediateSuccessor: func(dst, a []byte) []byte {
		// TODO(jackson): Consider changing this Comparer to only support
		// representable prefix keys containing characters a-z.
		ai := split(a)
		if ai != len(a) {
			panic("pebble: ImmediateSuccessor invoked with a non-prefix key")
		}
		return append(append(dst, a...), 0x00)
	},
	Split: split,
	ValidateKey: func(k []byte) error {
		// Ensure that if the key has a suffix, it's a valid integer
		// (potentially modulo a faux synthetic bit suffix).
		k = bytes.TrimSuffix(k, ignoreTimestampSuffix)
		i := split(k)
		if i == len(k) {
			return nil
		}
		if _, err := parseUintBytes(k[i+1:], 10, 64); err != nil {
			return errors.Wrapf(err, "invalid key %q", k)
		}
		return nil
	},
	Name: "pebble.internal.testkeys",
}

// The comparator is similar to the one in Cockroach; when the prefixes are
// equal:
//   - a key without a suffix is smaller than one with a suffix;
//   - when both keys have a suffix, the key with the larger (decoded) suffix
//     value is smaller.
func compare(a, b []byte) int {
	ai, bi := split(a), split(b)
	if v := bytes.Compare(a[:ai], b[:bi]); v != 0 {
		return v
	}
	return compareTimestamps(a[ai:], b[bi:])
}

func split(a []byte) int {
	i := bytes.LastIndexByte(a, suffixDelim)
	if i >= 0 {
		return i
	}
	return len(a)
}

func compareTimestamps(a, b []byte) int {
	a = bytes.TrimSuffix(a, ignoreTimestampSuffix)
	b = bytes.TrimSuffix(b, ignoreTimestampSuffix)
	if len(a) == 0 || len(b) == 0 {
		// The empty suffix sorts first.
		return cmp.Compare(len(a), len(b))
	}
	if a[0] != suffixDelim || b[0] != suffixDelim {
		panic(fmt.Sprintf("invalid suffixes %q %q", a, b))
	}
	ai, err := parseUintBytes(a[1:], 10, 64)
	if err != nil {
		panic(fmt.Sprintf("invalid test mvcc timestamp %q", a))
	}
	bi, err := parseUintBytes(b[1:], 10, 64)
	if err != nil {
		panic(fmt.Sprintf("invalid test mvcc timestamp %q", b))
	}
	return cmp.Compare(bi, ai)
}

func compareSuffixes(a, b []byte) int {
	cmp := compareTimestamps(a, b)
	if cmp == 0 {
		aHasIgnorableSuffix := bytes.HasSuffix(a, ignoreTimestampSuffix)
		bHasIgnorableSuffix := bytes.HasSuffix(b, ignoreTimestampSuffix)
		if aHasIgnorableSuffix && !bHasIgnorableSuffix {
			return 1
		}
		if !aHasIgnorableSuffix && bHasIgnorableSuffix {
			return -1
		}
	}
	return cmp
}

// Keyspace describes a finite keyspace of unsuffixed test keys.
type Keyspace interface {
	// Count returns the number of keys that exist within this keyspace.
	Count() int64

	// MaxLen returns the maximum length, in bytes, of a key within this
	// keyspace. This is only guaranteed to return an upper bound.
	MaxLen() int

	// Slice returns the sub-keyspace from index i, inclusive, to index j,
	// exclusive. The receiver is unmodified.
	Slice(i, j int64) Keyspace

	// EveryN returns a key space that includes 1 key for every N keys in the
	// original keyspace. The receiver is unmodified.
	EveryN(n int64) Keyspace

	// key writes the i-th key to the buffer and returns the length.
	key(buf []byte, i int64) int
}

// Divvy divides the provided keyspace into N equal portions, containing
// disjoint keys evenly distributed across the keyspace.
func Divvy(ks Keyspace, n int64) []Keyspace {
	ret := make([]Keyspace, n)
	for i := int64(0); i < n; i++ {
		ret[i] = ks.Slice(i, ks.Count()).EveryN(n)
	}
	return ret
}

// Alpha constructs a keyspace consisting of all keys containing characters a-z,
// with at most `maxLength` characters.
func Alpha(maxLength int) Keyspace {
	return alphabet{
		alphabet:  []byte(alpha),
		maxLength: maxLength,
		increment: 1,
	}
}

// KeyAt returns the i-th key within the keyspace with a suffix encoding the
// timestamp t.
func KeyAt(k Keyspace, i int64, t int64) []byte {
	b := make([]byte, k.MaxLen()+MaxSuffixLen)
	return b[:WriteKeyAt(b, k, i, t)]
}

// WriteKeyAt writes the i-th key within the keyspace to the buffer dst, with a
// suffix encoding the timestamp t suffix. It returns the number of bytes
// written.
func WriteKeyAt(dst []byte, k Keyspace, i int64, t int64) int {
	n := WriteKey(dst, k, i)
	n += WriteSuffix(dst[n:], t)
	return n
}

// Suffix returns the test keys suffix representation of timestamp t.
func Suffix(t int64) []byte {
	b := make([]byte, MaxSuffixLen)
	return b[:WriteSuffix(b, t)]
}

// SuffixLen returns the exact length of the given suffix when encoded.
func SuffixLen(t int64) int {
	// Begin at 1 for the '@' delimiter, 1 for a single digit.
	n := 2
	t /= 10
	for t > 0 {
		t /= 10
		n++
	}
	return n
}

// ParseSuffix returns the integer representation of the encoded suffix.
func ParseSuffix(s []byte) (int64, error) {
	s = bytes.TrimSuffix(s, ignoreTimestampSuffix)
	return strconv.ParseInt(strings.TrimPrefix(string(s), string(suffixDelim)), 10, 64)
}

// WriteSuffix writes the test keys suffix representation of timestamp t to dst,
// returning the number of bytes written.
func WriteSuffix(dst []byte, t int64) int {
	dst[0] = suffixDelim
	n := 1
	n += len(strconv.AppendInt(dst[n:n], t, 10))
	return n
}

// Key returns the i-th unsuffixed key within the keyspace.
func Key(k Keyspace, i int64) []byte {
	b := make([]byte, k.MaxLen())
	return b[:k.key(b, i)]
}

// WriteKey writes the i-th unsuffixed key within the keyspace to the buffer dst. It
// returns the number of bytes written.
func WriteKey(dst []byte, k Keyspace, i int64) int {
	return k.key(dst, i)
}

type alphabet struct {
	alphabet  []byte
	maxLength int
	headSkip  int64
	tailSkip  int64
	increment int64
}

func (a alphabet) Count() int64 {
	// Calculate the total number of keys, ignoring the increment.
	total := keyCount(len(a.alphabet), a.maxLength) - a.headSkip - a.tailSkip

	// The increment dictates that we take every N keys, where N = a.increment.
	// Consider a total containing the 5 keys:
	//   a  b  c  d  e
	//   ^     ^     ^
	// If the increment is 2, this keyspace includes 'a', 'c' and 'e'. After
	// dividing by the increment, there may be remainder. If there is, there's
	// one additional key in the alphabet.
	count := total / a.increment
	if total%a.increment > 0 {
		count++
	}
	return count
}

func (a alphabet) MaxLen() int {
	return a.maxLength
}

func (a alphabet) Slice(i, j int64) Keyspace {
	s := a
	s.headSkip += i
	s.tailSkip += a.Count() - j
	return s
}

func (a alphabet) EveryN(n int64) Keyspace {
	s := a
	s.increment *= n
	return s
}

func keyCount(n, l int) int64 {
	// The number of representable keys in the keyspace is a function of the
	// length of the alphabet n and the max key length l:
	//   n + n^2 + ... + n^l
	x := int64(1)
	res := int64(0)
	for i := 1; i <= l; i++ {
		if x >= math.MaxInt64/int64(n) {
			panic("overflow")
		}
		x *= int64(n)
		res += x
		if res < 0 {
			panic("overflow")
		}
	}
	return res
}

func (a alphabet) key(buf []byte, idx int64) int {
	// This function generates keys of length 1..maxKeyLength, pulling
	// characters from the alphabet. The idx determines which key to generate,
	// generating the i-th lexicographically next key.
	//
	// The index to use is advanced by `headSkip`, allowing a keyspace to encode
	// a subregion of the keyspace.
	//
	// Eg, alphabet = `ab`, maxKeyLength = 3:
	//
	//           aaa aab     aba abb         baa bab     bba bbb
	//       aa          ab              ba          bb
	//   a                           b
	//   0   1   2   3   4   5   6   7   8   9   10  11  12  13
	//
	return generateAlphabetKey(buf, a.alphabet, (idx*a.increment)+a.headSkip,
		keyCount(len(a.alphabet), a.maxLength))
}

func generateAlphabetKey(buf, alphabet []byte, i, keyCount int64) int {
	if keyCount == 0 || i > keyCount || i < 0 {
		return 0
	}

	// Of the keyCount keys in the generative keyspace, how many are there
	// starting with a particular character?
	keysPerCharacter := keyCount / int64(len(alphabet))

	// Find the character that the key at index i starts with and set it.
	characterIdx := i / keysPerCharacter
	buf[0] = alphabet[characterIdx]

	// Consider characterIdx = 0, pointing to 'a'.
	//
	//           aaa aab     aba abb         baa bab     bba bbb
	//       aa          ab              ba          bb
	//   a                           b
	//   0   1   2   3   4   5   6   7   8   9   10  11  12  13
	//  \_________________________/
	//    |keysPerCharacter| keys
	//
	// In our recursive call, we reduce the problem to:
	//
	//           aaa aab     aba abb
	//       aa          ab
	//       0   1   2   3   4   5
	//     \________________________/
	//    |keysPerCharacter-1| keys
	//
	// In the subproblem, there are keysPerCharacter-1 keys (eliminating the
	// just 'a' key, plus any keys beginning with any other character).
	//
	// The index i is also offset, reduced by the count of keys beginning with
	// characters earlier in the alphabet (keysPerCharacter*characterIdx) and
	// the key consisting of just the 'a' (-1).
	i = i - keysPerCharacter*characterIdx - 1
	return 1 + generateAlphabetKey(buf[1:], alphabet, i, keysPerCharacter-1)
}

// computeAlphabetKeyIndex computes the inverse of generateAlphabetKey,
// returning the index of a particular key, given the provided alphabet and max
// length of a key.
//
// len(key) must be ≥ 1.
func computeAlphabetKeyIndex(key []byte, alphabet map[byte]int64, n int) int64 {
	i, ok := alphabet[key[0]]
	if !ok {
		panic(fmt.Sprintf("unrecognized alphabet character %v", key[0]))
	}
	// How many keys exist that start with the preceding i characters? Each of
	// the i characters themselves are a key, plus the count of all the keys
	// with one less character for each.
	ret := i + i*keyCount(len(alphabet), n-1)
	if len(key) > 1 {
		ret += 1 + computeAlphabetKeyIndex(key[1:], alphabet, n-1)
	}
	return ret
}

// RandomPrefixInRange returns a random prefix in the range [a, b), where a and
// b are prefixes.
func RandomPrefixInRange(a, b []byte, rng *rand.Rand) []byte {
	assertValidPrefix(a)
	assertValidPrefix(b)
	assertLess(a, b)
	commonPrefix := 0
	for commonPrefix < len(a)-1 && commonPrefix < len(b)-1 && a[commonPrefix] == b[commonPrefix] {
		commonPrefix++
	}

	// We will generate a piece of a key from the Alpha(maxLength) keyspace. Note
	// that maxLength cannot be higher than ~13 or we will encounter overflows.
	maxLength := 4 + rng.IntN(8)

	// Skip any common prefix (but leave at least one character in each key).
	skipPrefix := 0
	for skipPrefix+1 < min(len(a), len(b)) && a[skipPrefix] == b[skipPrefix] {
		skipPrefix++
	}
	aPiece := a[skipPrefix:]
	bPiece := b[skipPrefix:]
	if len(aPiece) > maxLength {
		// The trimmed prefix is smaller than a; we must be careful below to not
		// return a key smaller than a.
		aPiece = aPiece[:maxLength]
	}
	if len(bPiece) > maxLength {
		// The trimmed prefix is smaller than b, so we will still respect the bound.
		bPiece = bPiece[:maxLength]
	}
	assertLess(aPiece, bPiece)
	apIdx := computeAlphabetKeyIndex(aPiece, inverseAlphabet, maxLength)
	bpIdx := computeAlphabetKeyIndex(bPiece, inverseAlphabet, maxLength)
	if bpIdx <= apIdx {
		panic("unreachable")
	}
	generatedIdx := apIdx + rng.Int64N(bpIdx-apIdx)
	if generatedIdx == apIdx {
		// Return key a. We handle this separately in case we trimmed aPiece above.
		return append([]byte(nil), a...)
	}
	dst := make([]byte, skipPrefix+maxLength)
	copy(dst, a[:skipPrefix])
	pieceLen := WriteKey(dst[skipPrefix:], Alpha(maxLength), generatedIdx)
	dst = dst[:skipPrefix+pieceLen]
	assertLE(a, dst)
	assertLess(dst, b)
	return dst
}

func assertValidPrefix(p []byte) {
	if !prefixRE.Match(p) {
		panic(fmt.Sprintf("invalid prefix %q", p))
	}
}

func assertLess(a, b []byte) {
	if Comparer.Compare(a, b) >= 0 {
		panic(fmt.Sprintf("invalid key ordering: %q >= %q", a, b))
	}
}

func assertLE(a, b []byte) {
	if Comparer.Compare(a, b) > 0 {
		panic(fmt.Sprintf("invalid key ordering: %q > %q", a, b))
	}
}

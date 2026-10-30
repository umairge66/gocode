package releasesig

import (
	"encoding/binary"
	"math/bits"
)

// A minimal, unkeyed BLAKE2b (RFC 7693) used only for minisign's prehashed
// signatures ("ED": Ed25519 over BLAKE2b-512(message)) and the secret-key
// checksum (BLAKE2b-256). Self-contained so the module needs no
// golang.org/x/crypto dependency.

const blake2bBlockSize = 128

var blake2bIV = [8]uint64{
	0x6a09e667f3bcc908, 0xbb67ae8584caa73b, 0x3c6ef372fe94f82b, 0xa54ff53a5f1d36f1,
	0x510e527fade682d1, 0x9b05688c2b3e6c1f, 0x1f83d9abfb41bd6b, 0x5be0cd19137e2179,
}

var blake2bSigma = [12][16]byte{
	{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15},
	{14, 10, 4, 8, 9, 15, 13, 6, 1, 12, 0, 2, 11, 7, 5, 3},
	{11, 8, 12, 0, 5, 2, 15, 13, 10, 14, 3, 6, 7, 1, 9, 4},
	{7, 9, 3, 1, 13, 12, 11, 14, 2, 6, 5, 10, 4, 0, 15, 8},
	{9, 0, 5, 7, 2, 4, 10, 15, 14, 1, 11, 12, 6, 8, 3, 13},
	{2, 12, 6, 10, 0, 11, 8, 3, 4, 13, 7, 5, 15, 14, 1, 9},
	{12, 5, 1, 15, 14, 13, 4, 10, 0, 7, 6, 3, 9, 2, 8, 11},
	{13, 11, 7, 14, 12, 1, 3, 9, 5, 0, 15, 4, 8, 6, 2, 10},
	{6, 15, 14, 9, 11, 3, 0, 8, 12, 2, 13, 7, 1, 4, 10, 5},
	{10, 2, 8, 4, 7, 6, 1, 5, 15, 11, 9, 14, 3, 12, 13, 0},
	{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15},
	{14, 10, 4, 8, 9, 15, 13, 6, 1, 12, 0, 2, 11, 7, 5, 3},
}

type blake2b struct {
	h      [8]uint64
	t      [2]uint64
	buf    [blake2bBlockSize]byte
	n      int
	outLen int
}

func newBlake2b(outLen int) *blake2b {
	if outLen < 1 || outLen > 64 {
		panic("releasesig: invalid BLAKE2b output length")
	}
	d := &blake2b{outLen: outLen, h: blake2bIV}
	d.h[0] ^= 0x01010000 ^ uint64(outLen)
	return d
}

func (d *blake2b) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		// Only compress a full buffer once more input follows: the final
		// block must be compressed with the last-block flag in Sum.
		if d.n == blake2bBlockSize {
			d.incr(blake2bBlockSize)
			d.compress(&d.buf, false)
			d.n = 0
		}
		c := copy(d.buf[d.n:], p)
		d.n += c
		p = p[c:]
	}
	return n, nil
}

func (d *blake2b) incr(n uint64) {
	d.t[0] += n
	if d.t[0] < n {
		d.t[1]++
	}
}

// Sum returns the digest; d must not be used afterwards.
func (d *blake2b) Sum() []byte {
	d.incr(uint64(d.n))
	for i := d.n; i < blake2bBlockSize; i++ {
		d.buf[i] = 0
	}
	d.compress(&d.buf, true)
	var out [64]byte
	for i, v := range d.h {
		binary.LittleEndian.PutUint64(out[i*8:], v)
	}
	return out[:d.outLen]
}

func (d *blake2b) compress(block *[blake2bBlockSize]byte, last bool) {
	var m [16]uint64
	for i := range m {
		m[i] = binary.LittleEndian.Uint64(block[i*8:])
	}
	var v [16]uint64
	copy(v[:8], d.h[:])
	copy(v[8:], blake2bIV[:])
	v[12] ^= d.t[0]
	v[13] ^= d.t[1]
	if last {
		v[14] = ^v[14]
	}
	g := func(a, b, c, e int, x, y uint64) {
		v[a] = v[a] + v[b] + x
		v[e] = bits.RotateLeft64(v[e]^v[a], -32)
		v[c] += v[e]
		v[b] = bits.RotateLeft64(v[b]^v[c], -24)
		v[a] = v[a] + v[b] + y
		v[e] = bits.RotateLeft64(v[e]^v[a], -16)
		v[c] += v[e]
		v[b] = bits.RotateLeft64(v[b]^v[c], -63)
	}
	for r := 0; r < 12; r++ {
		s := &blake2bSigma[r]
		g(0, 4, 8, 12, m[s[0]], m[s[1]])
		g(1, 5, 9, 13, m[s[2]], m[s[3]])
		g(2, 6, 10, 14, m[s[4]], m[s[5]])
		g(3, 7, 11, 15, m[s[6]], m[s[7]])
		g(0, 5, 10, 15, m[s[8]], m[s[9]])
		g(1, 6, 11, 12, m[s[10]], m[s[11]])
		g(2, 7, 8, 13, m[s[12]], m[s[13]])
		g(3, 4, 9, 14, m[s[14]], m[s[15]])
	}
	for i := range d.h {
		d.h[i] ^= v[i] ^ v[i+8]
	}
}

func blake2bSum(outLen int, parts ...[]byte) []byte {
	d := newBlake2b(outLen)
	for _, p := range parts {
		_, _ = d.Write(p)
	}
	return d.Sum()
}

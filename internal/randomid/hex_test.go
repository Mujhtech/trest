package randomid

import (
	"encoding/hex"
	"math/rand"
	"testing"
)

func assertHexBlock(t testing.TB, raw [256]byte) {
	t.Helper()
	var got, want [512]byte
	encodeHexBlock(&got, &raw)
	hex.Encode(want[:], raw[:])
	if got != want {
		t.Fatalf("hex mismatch for %x: got %s; want %s", raw, got, want)
	}
}

func TestHexBlockMatchesStandardLibrary(t *testing.T) {
	var raw [256]byte
	for value := range 256 {
		for index := range raw {
			raw[index] = byte(value)
		}
		assertHexBlock(t, raw)
	}
	for start := range 256 {
		for index := range raw {
			raw[index] = byte(start + index)
		}
		assertHexBlock(t, raw)
	}
	rng := rand.New(rand.NewSource(1))
	for range 4096 {
		if _, err := rng.Read(raw[:]); err != nil {
			t.Fatal(err)
		}
		assertHexBlock(t, raw)
	}
}

func FuzzHexBlockMatchesStandardLibrary(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0, 1, 9, 10, 15, 16, 0x7f, 0x80, 0x9f, 0xa0, 0xff})
	seed := make([]byte, 256)
	for index := range seed {
		seed[index] = byte(index)
	}
	f.Add(seed)
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 4096 {
			t.Skip()
		}
		var raw [256]byte
		copy(raw[:], data)
		assertHexBlock(t, raw)
	})
}

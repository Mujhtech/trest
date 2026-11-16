package randomid

import (
	"crypto/rand"
	"encoding/hex"
	"runtime"
	"testing"
)

func directID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

func BenchmarkID(b *testing.B) {
	for _, variant := range []struct {
		name string
		new  func() func() (string, error)
	}{
		{name: "direct", new: func() func() (string, error) { return directID }},
		{name: "batched", new: func() func() (string, error) { return New().ID }},
	} {
		b.Run(variant.name, func(b *testing.B) {
			generate := variant.new()
			b.ReportAllocs()
			b.ResetTimer()
			var last string
			for i := 0; i < b.N; i++ {
				id, err := generate()
				if err != nil {
					b.Fatal(err)
				}
				last = id
			}
			b.StopTimer()
			runtime.KeepAlive(last)
		})
	}
}

func BenchmarkIDParallel(b *testing.B) {
	for _, variant := range []struct {
		name string
		new  func() func() (string, error)
	}{
		{name: "direct", new: func() func() (string, error) { return directID }},
		{name: "batched", new: func() func() (string, error) { return New().ID }},
	} {
		b.Run(variant.name, func(b *testing.B) {
			generate := variant.new()
			b.ReportAllocs()
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				var last string
				for pb.Next() {
					id, err := generate()
					if err != nil {
						b.Error(err)
						return
					}
					last = id
				}
				runtime.KeepAlive(last)
			})
		})
	}
}

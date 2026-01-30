// Package vector implements vector search for SQLite via scalar functions
// registered with zombiezen.com/go/sqlite. It is implemented in pure Go
// and requires no CGo.
package vector

import (
	"encoding/binary"
	"fmt"
	"math"
)

// Float32ToBlob converts a []float32 to a little-endian byte slice suitable
// for storage as a SQLite blob.
func Float32ToBlob(v []float32) []byte {
	b := make([]byte, len(v)*4)
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(f))
	}
	return b
}

// BlobToFloat32 converts a little-endian byte slice back to []float32.
// Returns an error if len(b) is not a multiple of 4.
func BlobToFloat32(b []byte) ([]float32, error) {
	if len(b)%4 != 0 {
		return nil, fmt.Errorf("blob length %d is not a multiple of 4", len(b))
	}
	v := make([]float32, len(b)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return v, nil
}

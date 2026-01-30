// Package vector implements vector search for SQLite via scalar functions
// registered with zombiezen.com/go/sqlite. It is implemented in pure Go
// and requires no CGo.
package vector

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"

	"zombiezen.com/go/sqlite"
)

type config struct {
	dim          int
	quantMin     float32
	quantMax     float32
	quantEnabled bool
}

// Option configures vector function registration.
type Option func(*config)

// WithQuantRange enables quantization and sets the global min/max range
// for scalar int8 mapping.
func WithQuantRange(min, max float32) Option {
	return func(c *config) {
		c.quantMin = min
		c.quantMax = max
		c.quantEnabled = true
	}
}

// Register registers all SQL functions on the given connection for vectors
// of dimension dim. Returns an error if dim < 1.
func Register(conn *sqlite.Conn, dim int, opts ...Option) error {
	if dim < 1 {
		return fmt.Errorf("vector: dimension must be >= 1, got %d", dim)
	}
	cfg := &config{dim: dim}
	for _, o := range opts {
		o(cfg)
	}

	err := conn.CreateFunction("vector_encode", &sqlite.FunctionImpl{
		NArgs:         1,
		Deterministic: true,
		Scalar: func(ctx sqlite.Context, args []sqlite.Value) (sqlite.Value, error) {
			if args[0].Type() == sqlite.TypeNull {
				return sqlite.Value{}, nil
			}
			text := args[0].Text()
			var nums []float64
			if err := json.Unmarshal([]byte(text), &nums); err != nil {
				return sqlite.Value{}, fmt.Errorf("vector_encode: invalid JSON: %v", err)
			}
			if len(nums) != cfg.dim {
				return sqlite.Value{}, fmt.Errorf("vector_encode: expected dimension %d, got %d", cfg.dim, len(nums))
			}
			floats := make([]float32, len(nums))
			for i, n := range nums {
				floats[i] = float32(n)
			}
			return sqlite.BlobValue(Float32ToBlob(floats)), nil
		},
	})
	if err != nil {
		return err
	}

	err = conn.CreateFunction("vector_distance", &sqlite.FunctionImpl{
		NArgs:         2,
		Deterministic: true,
		Scalar: func(ctx sqlite.Context, args []sqlite.Value) (sqlite.Value, error) {
			return sqlite.Value{}, fmt.Errorf("vector_distance: not implemented")
		},
	})
	if err != nil {
		return err
	}

	err = conn.CreateFunction("vector_quantize", &sqlite.FunctionImpl{
		NArgs:         1,
		Deterministic: true,
		Scalar: func(ctx sqlite.Context, args []sqlite.Value) (sqlite.Value, error) {
			return sqlite.Value{}, fmt.Errorf("vector_quantize: not implemented")
		},
	})
	if err != nil {
		return err
	}

	err = conn.CreateFunction("vector_distance_q", &sqlite.FunctionImpl{
		NArgs:         2,
		Deterministic: true,
		Scalar: func(ctx sqlite.Context, args []sqlite.Value) (sqlite.Value, error) {
			return sqlite.Value{}, fmt.Errorf("vector_distance_q: not implemented")
		},
	})
	if err != nil {
		return err
	}

	return nil
}

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

func l2Squared(a, b []float32) float64 {
	var sum float64
	for i := range a {
		d := float64(a[i]) - float64(b[i])
		sum += d * d
	}
	return sum
}

func isQuantizedBlob(b []byte) bool {
	return len(b) >= 2 && b[0] == 0x00 && b[1] == 0x01
}

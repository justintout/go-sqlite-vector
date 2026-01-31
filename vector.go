// Package vector implements vector search for SQLite via scalar functions
// registered with zombiezen.com/go/sqlite. It is implemented in pure Go
// and requires no CGo.
package vector

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"

	"zombiezen.com/go/sqlite"
)

// Embedder produces vector embeddings from text.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

// Chunker splits text into chunks for embedding.
type Chunker interface {
	Chunk(text string) ([]string, error)
}

type config struct {
	dim          int
	quantMin     float32
	quantMax     float32
	quantEnabled bool
	embedder     Embedder
	chunker      Chunker
}

// Option configures vector function registration.
type Option func(*config)

// WithChunker enables the vector_chunk table-valued function using the given Chunker.
func WithChunker(c Chunker) Option {
	return func(cfg *config) {
		cfg.chunker = c
	}
}

// WithEmbedder enables the vector_embed SQL function using the given Embedder.
func WithEmbedder(e Embedder) Option {
	return func(c *config) {
		c.embedder = e
	}
}

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
			if args[0].Type() == sqlite.TypeNull || args[1].Type() == sqlite.TypeNull {
				return sqlite.Value{}, nil
			}
			blobA := args[0].Blob()
			blobB := args[1].Blob()
			if isQuantizedBlob(blobA) || isQuantizedBlob(blobB) {
				return sqlite.Value{}, fmt.Errorf("vector_distance: input is quantized, use vector_distance_q")
			}
			expected := cfg.dim * 4
			if len(blobA) != expected {
				return sqlite.Value{}, fmt.Errorf("vector_distance: expected %d bytes (dim=%d), got %d", expected, cfg.dim, len(blobA))
			}
			if len(blobB) != expected {
				return sqlite.Value{}, fmt.Errorf("vector_distance: expected %d bytes (dim=%d), got %d", expected, cfg.dim, len(blobB))
			}
			a, _ := BlobToFloat32(blobA)
			b, _ := BlobToFloat32(blobB)
			return sqlite.FloatValue(l2Squared(a, b)), nil
		},
	})
	if err != nil {
		return err
	}

	err = conn.CreateFunction("vector_quantize", &sqlite.FunctionImpl{
		NArgs:         1,
		Deterministic: true,
		Scalar: func(ctx sqlite.Context, args []sqlite.Value) (sqlite.Value, error) {
			if args[0].Type() == sqlite.TypeNull {
				return sqlite.Value{}, nil
			}
			if !cfg.quantEnabled {
				return sqlite.Value{}, fmt.Errorf("vector_quantize: quantization not configured, call Register with WithQuantRange")
			}
			blob := args[0].Blob()
			expected := cfg.dim * 4
			if len(blob) != expected {
				return sqlite.Value{}, fmt.Errorf("vector_quantize: expected %d bytes (dim=%d), got %d", expected, cfg.dim, len(blob))
			}
			floats, _ := BlobToFloat32(blob)
			return sqlite.BlobValue(quantize(floats, cfg.quantMin, cfg.quantMax)), nil
		},
	})
	if err != nil {
		return err
	}

	err = conn.CreateFunction("vector_distance_q", &sqlite.FunctionImpl{
		NArgs:         2,
		Deterministic: true,
		Scalar: func(ctx sqlite.Context, args []sqlite.Value) (sqlite.Value, error) {
			if args[0].Type() == sqlite.TypeNull || args[1].Type() == sqlite.TypeNull {
				return sqlite.Value{}, nil
			}
			if !cfg.quantEnabled {
				return sqlite.Value{}, fmt.Errorf("vector_distance_q: quantization not configured, call Register with WithQuantRange")
			}
			blobA := args[0].Blob()
			blobB := args[1].Blob()
			if !isQuantizedBlob(blobA) {
				return sqlite.Value{}, fmt.Errorf("vector_distance_q: input a is not quantized (missing magic bytes)")
			}
			if !isQuantizedBlob(blobB) {
				return sqlite.Value{}, fmt.Errorf("vector_distance_q: input b is not quantized (missing magic bytes)")
			}
			expected := 2 + cfg.dim
			if len(blobA) != expected {
				return sqlite.Value{}, fmt.Errorf("vector_distance_q: expected %d bytes (dim=%d), got %d", expected, cfg.dim, len(blobA))
			}
			if len(blobB) != expected {
				return sqlite.Value{}, fmt.Errorf("vector_distance_q: expected %d bytes (dim=%d), got %d", expected, cfg.dim, len(blobB))
			}
			a, _ := dequantize(blobA, cfg.quantMin, cfg.quantMax)
			b, _ := dequantize(blobB, cfg.quantMin, cfg.quantMax)
			return sqlite.FloatValue(l2Squared(a, b)), nil
		},
	})
	if err != nil {
		return err
	}

	err = conn.CreateFunction("vector_embed", &sqlite.FunctionImpl{
		NArgs:         1,
		Deterministic: false,
		Scalar: func(ctx sqlite.Context, args []sqlite.Value) (sqlite.Value, error) {
			if args[0].Type() == sqlite.TypeNull {
				return sqlite.Value{}, nil
			}
			if cfg.embedder == nil {
				return sqlite.Value{}, fmt.Errorf("vector_embed: no embedder configured, call Register with WithEmbedder")
			}
			text := args[0].Text()
			floats, err := cfg.embedder.Embed(context.Background(), text)
			if err != nil {
				return sqlite.Value{}, fmt.Errorf("vector_embed: %w", err)
			}
			if len(floats) != cfg.dim {
				return sqlite.Value{}, fmt.Errorf("vector_embed: embedder returned dimension %d, expected %d", len(floats), cfg.dim)
			}
			return sqlite.BlobValue(Float32ToBlob(floats)), nil
		},
	})
	if err != nil {
		return err
	}

	err = conn.SetModule("vector_chunk", &sqlite.Module{
		Connect: func(c *sqlite.Conn, opts *sqlite.VTableConnectOptions) (sqlite.VTable, *sqlite.VTableConfig, error) {
			return &chunkVTable{chunker: cfg.chunker}, &sqlite.VTableConfig{
				Declaration: "CREATE TABLE x(value TEXT, chunk_index INTEGER, text TEXT HIDDEN)",
			}, nil
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

func quantize(v []float32, min, max float32) []byte {
	b := make([]byte, 2+len(v))
	b[0] = 0x00
	b[1] = 0x01
	r := max - min
	for i, f := range v {
		normalized := (f - min) / r * 255
		q := math.Round(float64(normalized)) - 128
		if q < -128 {
			q = -128
		} else if q > 127 {
			q = 127
		}
		b[2+i] = byte(int8(q))
	}
	return b
}

func dequantize(b []byte, min, max float32) ([]float32, error) {
	if len(b) < 2 || b[0] != 0x00 || b[1] != 0x01 {
		return nil, fmt.Errorf("dequantize: missing quantized format magic bytes")
	}
	data := b[2:]
	r := float64(max - min)
	v := make([]float32, len(data))
	for i, raw := range data {
		q := int8(raw)
		v[i] = float32((float64(q)+128)/255*r + float64(min))
	}
	return v, nil
}

const chunkColValue = 0
const chunkColIndex = 1
const chunkColText = 2

type chunkVTable struct {
	chunker Chunker
}

func (vt *chunkVTable) BestIndex(inputs *sqlite.IndexInputs) (*sqlite.IndexOutputs, error) {
	outputs := &sqlite.IndexOutputs{
		EstimatedCost: 1e12,
		EstimatedRows: 1e6,
	}
	for i, c := range inputs.Constraints {
		if c.Column == chunkColText && c.Op == sqlite.IndexConstraintEq && c.Usable {
			usage := make([]sqlite.IndexConstraintUsage, len(inputs.Constraints))
			usage[i] = sqlite.IndexConstraintUsage{
				ArgvIndex: 1,
				Omit:      true,
			}
			outputs.ConstraintUsage = usage
			outputs.EstimatedCost = 1
			outputs.EstimatedRows = 10
			outputs.ID = sqlite.IndexID{Num: 1}
			break
		}
	}
	return outputs, nil
}

func (vt *chunkVTable) Open() (sqlite.VTableCursor, error) {
	return &chunkCursor{vtab: vt}, nil
}

func (vt *chunkVTable) Disconnect() error { return nil }
func (vt *chunkVTable) Destroy() error    { return nil }

type chunkCursor struct {
	vtab   *chunkVTable
	chunks []string
	pos    int
}

func (cur *chunkCursor) Filter(id sqlite.IndexID, argv []sqlite.Value) error {
	if cur.vtab.chunker == nil {
		return fmt.Errorf("vector_chunk: no chunker configured, call Register with WithChunker")
	}
	cur.chunks = nil
	cur.pos = 0
	if len(argv) == 0 || argv[0].Type() == sqlite.TypeNull {
		return nil
	}
	text := argv[0].Text()
	chunks, err := cur.vtab.chunker.Chunk(text)
	if err != nil {
		return fmt.Errorf("vector_chunk: %w", err)
	}
	cur.chunks = chunks
	return nil
}

func (cur *chunkCursor) Next() error {
	cur.pos++
	return nil
}

func (cur *chunkCursor) Column(i int, noChange bool) (sqlite.Value, error) {
	switch i {
	case chunkColValue:
		return sqlite.TextValue(cur.chunks[cur.pos]), nil
	case chunkColIndex:
		return sqlite.IntegerValue(int64(cur.pos)), nil
	default:
		return sqlite.Value{}, nil
	}
}

func (cur *chunkCursor) RowID() (int64, error) {
	return int64(cur.pos), nil
}

func (cur *chunkCursor) EOF() bool {
	return cur.pos >= len(cur.chunks)
}

func (cur *chunkCursor) Close() error {
	return nil
}

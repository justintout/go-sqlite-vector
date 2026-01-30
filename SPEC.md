# go-sqlite-vector

`go-sqlite-vector` implements vector search for SQLite via scalar functions registered with [zombiezen.com/go/sqlite](https://pkg.go.dev/zombiezen.com/go/sqlite). It is implemented in pure Go and requires no CGo.

Users store embeddings as little-endian float32 blobs and perform vector search by combining `vector_distance` with `ORDER BY ... LIMIT k` (nearest neighbor scan). Optional scalar int8 quantization reduces storage. Only squared L2 distance is supported.

It acts in place of external extensions like [sqlite-vec](https://github.com/asg017/sqlite-vec) and [sqlite-vector](https://github.com/sqliteai/sqlite-vector).

## Project

- **Module path**: `github.com/justintout/go-sqlite-vector`
- **Package name**: `vector`
- **Go version**: 1.25+
- **License**: BSD-3-Clause
- **Dependencies**: `zombiezen.com/go/sqlite` only. All other functionality uses the standard library (`encoding/json`, `encoding/binary`, `math`).
- **Package structure**: single package at the repository root. All implementation details are unexported.

## Go Public API

### Register

```go
func Register(conn *sqlite.Conn, dim int, opts ...Option) error
```

Registers all SQL functions on the given connection for vectors of dimension `dim`. All four SQL functions are always registered. Calling `Register` again on the same connection overwrites the previous registration.

Returns an error if `dim < 1`.

### Options

```go
type Option func(*config)
```

```go
func WithQuantRange(min, max float32) Option
```

Enables quantization and sets the global min/max range for scalar int8 mapping. All calls to `vector_quantize` and `vector_distance_q` on this connection use this range. If `WithQuantRange` is not provided, `vector_quantize` and `vector_distance_q` return SQL errors when called.

### Blob Helpers

```go
func Float32ToBlob(v []float32) []byte
```

Converts a `[]float32` to a little-endian byte slice suitable for storage as a SQLite blob. Length of the returned slice is `len(v) * 4`.

```go
func BlobToFloat32(b []byte) ([]float32, error)
```

Converts a little-endian byte slice back to `[]float32`. Returns an error if `len(b)` is not a multiple of 4.

## SQL Functions

All four functions are registered on every `Register` call. NULL input to any function produces NULL output (standard SQL NULL propagation).

### vector_encode

```sql
vector_encode(json_text TEXT) -> BLOB
```

Parses a JSON array of numbers into a little-endian float32 blob.

- **Input**: JSON text containing an array of numbers, e.g. `'[0.1, 0.2, 0.3]'`.
- **Output**: float32 blob (raw little-endian bytes, length = dim * 4).
- **Errors**: returns a SQL error if the JSON array length does not match the registered dimension. Returns a SQL error if the input is not valid JSON or not an array of numbers.

### vector_distance

```sql
vector_distance(a BLOB, b BLOB) -> REAL
```

Computes squared L2 (Euclidean) distance between two float32 blobs: `sum((a[i] - b[i])^2)`. No square root is applied. Squared L2 preserves nearest-neighbor ordering and is cheaper to compute.

- **Input**: two float32 blobs.
- **Output**: `REAL` (float64).
- **Errors**: returns a SQL error if either blob's byte length does not equal `dim * 4`. Returns a SQL error if either blob has the quantized format magic bytes (0x00, 0x01 prefix) — use `vector_distance_q` for quantized blobs.

### vector_quantize

```sql
vector_quantize(vec BLOB) -> BLOB
```

Converts a float32 blob to a scalar int8 quantized blob using the globally configured min/max range.

- **Input**: float32 blob.
- **Output**: quantized int8 blob (format described below).
- **Mapping**: linear mapping from `[min, max]` to `[-128, 127]`. Values outside the configured range are clamped silently to the int8 boundaries.
- **Errors**: returns a SQL error if quantization was not configured (i.e. `WithQuantRange` was not passed to `Register`). Returns a SQL error if the input blob's byte length does not equal `dim * 4`.

### vector_distance_q

```sql
vector_distance_q(a BLOB, b BLOB) -> REAL
```

Computes squared L2 distance between two quantized int8 blobs. Both blobs are dequantized back to float32 using the configured min/max range before computing the distance.

- **Input**: two quantized int8 blobs.
- **Output**: `REAL` (float64).
- **Errors**: returns a SQL error if quantization was not configured. Returns a SQL error if either blob does not have the quantized format magic bytes (0x00, 0x01 prefix). Returns a SQL error if either blob's data length does not equal `dim` (total blob length = 2 + dim).

## Blob Formats

### Float32 Blob

Raw little-endian IEEE 754 float32 values concatenated. No header or magic bytes.

```
[float32 LE] [float32 LE] ... [float32 LE]
 ──dim values──
```

Total byte length: `dim * 4`.

### Quantized Int8 Blob

Two-byte header followed by int8 values.

```
[0x00] [0x01] [int8] [int8] ... [int8]
 fmt    ver    ──dim values──
```

- Byte 0: `0x00` — format identifier.
- Byte 1: `0x01` — version number.
- Bytes 2..dim+2: signed int8 values, one per dimension.

Total byte length: `2 + dim`.

### Format Discrimination

Given a blob and a known dimension `dim`:
- Float32 blob: byte length == `dim * 4`.
- Quantized blob: byte length == `2 + dim` AND first two bytes are `0x00, 0x01`.

`vector_distance` and `vector_distance_q` validate the format of their inputs and return errors on mismatch.

## Quantization

Scalar int8 quantization maps each float32 value to a signed int8 using a global linear mapping:

```
quantize(v) = clamp(round((v - min) / (max - min) * 255 - 128), -128, 127)
dequantize(q) = (q + 128) / 255 * (max - min) + min
```

- **Range**: configured via `WithQuantRange(min, max float32)`.
- **Not enabled by default**: calling `vector_quantize` or `vector_distance_q` without `WithQuantRange` returns a SQL error.
- **Clamping**: float32 values outside `[min, max]` are clamped silently — no error, no warning.

## Error Handling

All errors are returned as SQL errors that abort the executing statement.

| Condition | Error message format |
|---|---|
| Dimension mismatch | `"vector_encode: expected dimension %d, got %d"` |
| Blob size mismatch | `"vector_distance: expected %d bytes (dim=%d), got %d"` |
| Wrong blob format | `"vector_distance: input is quantized, use vector_distance_q"` |
| Quantization not configured | `"vector_quantize: quantization not configured, call Register with WithQuantRange"` |
| Invalid JSON | `"vector_encode: invalid JSON: %v"` |

NULL inputs produce NULL output without error.

## Usage Example

```sql
-- Create table
CREATE TABLE documents (
    id INTEGER PRIMARY KEY,
    content TEXT,
    embedding BLOB
);

-- Insert with vector_encode
INSERT INTO documents (content, embedding)
VALUES ('hello world', vector_encode('[0.1, 0.2, 0.3]'));

-- k-nearest neighbor search (k=5)
SELECT id, content, vector_distance(embedding, vector_encode('[0.15, 0.25, 0.35]')) AS dist
FROM documents
ORDER BY dist
LIMIT 5;

-- With quantization (if configured with WithQuantRange)
CREATE TABLE documents_q (
    id INTEGER PRIMARY KEY,
    content TEXT,
    embedding_q BLOB
);

INSERT INTO documents_q (content, embedding_q)
VALUES ('hello world', vector_quantize(vector_encode('[0.1, 0.2, 0.3]')));

SELECT id, content, vector_distance_q(embedding_q, vector_quantize(vector_encode('[0.15, 0.25, 0.35]'))) AS dist
FROM documents_q
ORDER BY dist
LIMIT 5;
```

```go
package main

import (
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"

	"github.com/justintout/go-sqlite-vector"
)

func main() {
	conn, _ := sqlite.OpenConn(":memory:")
	defer conn.Close()

	// Register with dimension 3, no quantization
	vector.Register(conn, 3)

	// Register with dimension 768 and quantization
	vector.Register(conn, 768, vector.WithQuantRange(-1.0, 1.0))

	// Use Float32ToBlob for programmatic blob creation
	emb := vector.Float32ToBlob([]float32{0.1, 0.2, 0.3})
	// emb can be bound as a blob parameter in SQL statements
	_ = emb
}
```

## Testing

Table-driven integration tests executed against real SQLite connections. Each test case defines a SQL setup, a query, and the expected result. Test categories:

- **vector_encode**: valid JSON arrays, dimension mismatch, invalid JSON, NULL input.
- **vector_distance**: correct distance computation, dimension mismatch, quantized blob rejection, NULL input.
- **vector_quantize**: correct quantization output, out-of-range clamping, not-configured error, NULL input.
- **vector_distance_q**: correct distance after dequantization, format validation, not-configured error, NULL input.
- **Round-trip**: encode -> store -> retrieve -> distance pipeline.
- **Float32ToBlob / BlobToFloat32**: Go-level encode/decode correctness.

## Benchmarks

Go benchmarks (`testing.B`) for the core operations at dimensions 384, 768, and 1536:

- `BenchmarkL2Distance_{384,768,1536}`
- `BenchmarkQuantize_{384,768,1536}`
- `BenchmarkDequantize_{384,768,1536}`
- `BenchmarkVectorEncode_{384,768,1536}`

## Design Notes

- **Extensibility**: the package is designed so that a virtual table module (via `sqlite.SetModule` / `SetModule`) could be added in the future without breaking the existing scalar function API.
- **No CGo**: all vector math is implemented in pure Go using `encoding/binary` and `math` from the standard library.
- **Nearest neighbor scan**: search is a brute-force scan over all rows. This is appropriate for SQLite-scale datasets (thousands to low millions of vectors). For larger datasets, users should consider a dedicated vector database.

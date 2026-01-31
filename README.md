# go-sqlite-vector

Pure Go vector search for SQLite. No CGo required.

Register scalar SQL functions on a [zombiezen.com/go/sqlite](https://pkg.go.dev/zombiezen.com/go/sqlite) connection to store embeddings as blobs and run k-nearest neighbor queries with `ORDER BY vector_distance(...) LIMIT k`.

```go
conn, _ := sqlite.OpenConn(":memory:")
defer conn.Close()

vector.Register(conn, 3)
```

```sql
CREATE TABLE documents (
    id INTEGER PRIMARY KEY,
    content TEXT,
    embedding BLOB
);

INSERT INTO documents (content, embedding)
VALUES ('hello world', vector_encode('[0.1, 0.2, 0.3]'));

-- k-nearest neighbor search
SELECT id, content
FROM documents
ORDER BY vector_distance(embedding, vector_encode('[0.15, 0.25, 0.35]'))
LIMIT 5;
```

## Install

```
go get github.com/justintout/go-sqlite-vector
```

Requires Go 1.23+. The only dependency is `zombiezen.com/go/sqlite`.

## SQL Functions

All functions are registered by calling `vector.Register`. NULL inputs produce NULL outputs.

| Function | Signature | Description |
|---|---|---|
| `vector_encode` | `(json TEXT) -> BLOB` | Parse a JSON number array into a float32 blob |
| `vector_distance` | `(a BLOB, b BLOB) -> REAL` | Squared L2 distance between two float32 blobs |
| `vector_quantize` | `(vec BLOB) -> BLOB` | Float32 blob to scalar int8 quantized blob |
| `vector_distance_q` | `(a BLOB, b BLOB) -> REAL` | Squared L2 distance between two quantized blobs |
| `vector_embed` | `(text TEXT) -> BLOB` | Embed text into a float32 blob using a configured `Embedder` |

Squared L2 is used instead of Euclidean distance because it preserves nearest-neighbor ordering and avoids the square root.

## Go API

```go
// Register all SQL functions for the given dimension.
func Register(conn *sqlite.Conn, dim int, opts ...Option) error

// Enable int8 quantization with a global min/max range.
func WithQuantRange(min, max float32) Option

// Enable the vector_embed SQL function with a custom embedder.
func WithEmbedder(e Embedder) Option

// Embedder produces vector embeddings from text.
type Embedder interface {
    Embed(ctx context.Context, text string) ([]float32, error)
}

// Convert between []float32 and little-endian blobs for parameter binding.
func Float32ToBlob(v []float32) []byte
func BlobToFloat32(b []byte) ([]float32, error)
```

### Parameter binding

Use `Float32ToBlob` to bind embeddings as query parameters instead of going through JSON:

```go
emb := vector.Float32ToBlob(queryVector)
sqlitex.ExecuteTransient(conn,
    "SELECT id FROM documents ORDER BY vector_distance(embedding, ?1) LIMIT 10",
    &sqlitex.ExecOptions{Args: []any{emb}},
)
```

## Quantization

Optional scalar int8 quantization reduces storage from `dim * 4` bytes to `2 + dim` bytes per vector. Enable it by passing `WithQuantRange` to `Register`:

```go
vector.Register(conn, 768, vector.WithQuantRange(-1.0, 1.0))
```

```sql
INSERT INTO docs (embedding_q)
VALUES (vector_quantize(vector_encode('[0.1, 0.2, ...]')));

SELECT id FROM docs
ORDER BY vector_distance_q(embedding_q, vector_quantize(vector_encode('[0.15, ...]')))
LIMIT 10;
```

Values outside the configured range are clamped silently. Calling `vector_quantize` or `vector_distance_q` without configuring a range returns a SQL error.

## Embedding

Optional `vector_embed` function converts text to embeddings inside SQL. Provide an `Embedder` implementation via `WithEmbedder`:

```go
type myEmbedder struct{}

func (m *myEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
    // Call your embedding API (OpenAI, Ollama, etc.)
    return callEmbeddingAPI(ctx, text)
}

vector.Register(conn, 768, vector.WithEmbedder(&myEmbedder{}))
```

```sql
SELECT id, content
FROM documents
ORDER BY vector_distance(embedding, vector_embed('search query'))
LIMIT 5;
```

Calling `vector_embed` without configuring an embedder returns a SQL error.

## Design

- **Brute-force scan**: search is a linear scan over all rows, appropriate for SQLite-scale datasets (thousands to low millions of vectors).
- **Pure Go**: all vector math uses `encoding/binary` and `math` from the standard library.
- **Single package**: everything lives in package `vector` at the module root. All internals are unexported.

## Benchmarks

```
go test -bench=. -benchmem ./...
```

Results on Apple M3 Max:

```
BenchmarkL2Distance/dim=384     10429109    115.1 ns/op     0 B/op    0 allocs/op
BenchmarkL2Distance/dim=768      5440725    221.4 ns/op     0 B/op    0 allocs/op
BenchmarkL2Distance/dim=1536     2768809    434.3 ns/op     0 B/op    0 allocs/op
BenchmarkQuantize/dim=384        2386230    504.5 ns/op   416 B/op    1 allocs/op
BenchmarkQuantize/dim=768        1000000   1001   ns/op   896 B/op    1 allocs/op
BenchmarkQuantize/dim=1536        606144   1974   ns/op  1792 B/op    1 allocs/op
BenchmarkDequantize/dim=384      2781045    439.3 ns/op  1536 B/op    1 allocs/op
BenchmarkDequantize/dim=768      1607391    739.0 ns/op  3072 B/op    1 allocs/op
BenchmarkDequantize/dim=1536      788222   1473   ns/op  6144 B/op    1 allocs/op
```

## License

BSD-3-Clause

# go-sqlite-vector

[![test](https://github.com/justintout/go-sqlite-vector/actions/workflows/test.yml/badge.svg)](https://github.com/justintout/go-sqlite-vector/actions/workflows/test.yml)

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
| `vector_chunk` | `(text TEXT) -> (value TEXT, chunk_index INTEGER)` | Table-valued: split text into chunk rows using a configured `Chunker` |

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

// Enable the vector_chunk table-valued function with a custom chunker.
func WithChunker(c Chunker) Option

// Chunker splits text into chunks for embedding.
type Chunker interface {
    Chunk(text string) ([]string, error)
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

## Vector Index

The `vector_index` virtual table is a faster alternative to `ORDER BY vector_distance(...)` for large tables. It stores vectors in chunks of 1024 and scans them in Go across `GOMAXPROCS` goroutines, instead of calling a SQL function for every row.

```sql
CREATE VIRTUAL TABLE docs_vec USING vector_index();     -- float32
CREATE VIRTUAL TABLE docs_q USING vector_index(int8);   -- quantized, requires WithQuantRange

INSERT INTO docs_vec (rowid, embedding) VALUES (1, vector_encode('[0.1, 0.2, 0.3]'));

-- k nearest neighbors
SELECT rowid, distance
FROM docs_vec
WHERE embedding MATCH vector_encode('[0.15, 0.25, 0.35]') AND k = 5;

-- LIMIT also works when the query reads only docs_vec
SELECT rowid, distance
FROM docs_vec
WHERE embedding MATCH vector_encode('[0.15, 0.25, 0.35]')
ORDER BY distance
LIMIT 5;

-- joins need k, because SQLite does not pass LIMIT through a join
SELECT d.content, v.distance
FROM docs_vec v
JOIN documents d ON d.id = v.rowid
WHERE v.embedding MATCH vector_encode('[0.15, 0.25, 0.35]') AND v.k = 5
ORDER BY v.distance;
```

`embedding` is written and matched as a float32 blob. int8 tables quantize on write and on search using the `WithQuantRange` range, and return `vector_distance_q` distances. `UPDATE` and `DELETE` work as on a normal table.

The table keeps its data in ordinary shadow tables (`docs_vec_info`, `docs_vec_chunks`, `docs_vec_data`, `docs_vec_rowids`), so writes follow SQLite transactions. Reopening a database with a different dimension or quantization range than the table was created with returns an error.

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

## Chunking

Optional `vector_chunk` table-valued function splits text into rows for per-chunk embedding. Provide a `Chunker` implementation via `WithChunker`:

```go
type myChunker struct{}

func (m *myChunker) Chunk(text string) ([]string, error) {
    // Split text by paragraph, token count, etc.
    return splitByTokens(text, 512), nil
}

vector.Register(conn, 768,
    vector.WithEmbedder(&myEmbedder{}),
    vector.WithChunker(&myChunker{}),
)
```

```sql
-- Chunk a document and embed each chunk in one query
INSERT INTO doc_chunks (doc_id, chunk_idx, chunk_text, embedding)
SELECT :doc_id, chunk_index, value, vector_embed(value)
FROM vector_chunk(:text);
```

Each row returned by `vector_chunk` has a `value` (the chunk text) and a `chunk_index` (0-based position). Calling `vector_chunk` without configuring a chunker returns a SQL error.

## Design

- **Brute-force scan**: search is a linear scan over all rows, through the scalar functions or `vector_index`, appropriate for SQLite-scale datasets (thousands to low millions of vectors).
- **Pure Go**: all vector math uses `encoding/binary` and `math` from the standard library.
- **Single package**: everything lives in package `vector` at the module root. All internals are unexported.

## Benchmarks

```
go test -bench=. -benchmem ./...
```

Results on Apple M3 Max. Distance benchmarks operate on encoded blobs, as the SQL functions do:

```
BenchmarkL2Distance/dim=384            4908330     241.5 ns/op     0 B/op    0 allocs/op
BenchmarkL2Distance/dim=768            2496775     483.3 ns/op     0 B/op    0 allocs/op
BenchmarkL2Distance/dim=1536           1246688     965.9 ns/op     0 B/op    0 allocs/op
BenchmarkQuantize/dim=384              2521759     477.5 ns/op   416 B/op    1 allocs/op
BenchmarkQuantize/dim=768              1269049     944.5 ns/op   896 B/op    1 allocs/op
BenchmarkQuantize/dim=1536              630952    1889   ns/op  1792 B/op    1 allocs/op
BenchmarkL2DistanceQuantized/dim=384  11420806     104.5 ns/op     0 B/op    0 allocs/op
BenchmarkL2DistanceQuantized/dim=768   5961984     201.5 ns/op     0 B/op    0 allocs/op
BenchmarkL2DistanceQuantized/dim=1536  3041773     398.2 ns/op     0 B/op    0 allocs/op
```

### SIFT1M

[SIFT1M](http://corpus-texmex.irisa.fr/) is a standard nearest-neighbor benchmark: 1,000,000 base vectors and 10,000 query vectors, 128 dimensions, L2 distance, with published ground-truth neighbors. The first 100 queries run with k=100 against an on-disk database with default SQLite settings. Every implementation searches on one thread except `vector_index`, which uses all 14 cores of the test machine.

```
curl -O ftp://ftp.irisa.fr/local/texmex/corpus/sift.tar.gz
mkdir -p .tmp && tar xzf sift.tar.gz -C .tmp
go test -tags sift -run TestSIFT1M -timeout 0 -v -sift.dir .tmp/sift
uv run bench/sift_compare.py .tmp/sift 100
```

Results on Apple M3 Max, macOS 15.7, Go 1.24.12. Build time is the time to insert all vectors (HNSW builds use all cores).

| Implementation | Search | Build | p50 | QPS | Recall@10 | Recall@100 |
|---|---|---|---|---|---|---|
| go-sqlite-vector `vector_distance` | exact scan | 2.5s | 363 ms | 2.7 | 0.999 | 1.000 |
| go-sqlite-vector `vector_distance_q` (int8) | quantized scan | +3.3s | 336 ms | 2.9 | 0.983 | 0.988 |
| go-sqlite-vector `vector_index` | exact chunk scan | 9.8s | 139 ms | 6.9 | 0.999 | 1.000 |
| go-sqlite-vector `vector_index(int8)` | quantized chunk scan | 6.5s | 46 ms | 20.9 | 0.983 | 0.988 |
| sqlite-vec 0.1.9 `vec_distance_l2` | exact scan | 1.4s | 306 ms | 3.3 | 0.999 | 1.000 |
| sqlite-vec 0.1.9 `vec0` | exact scan | 4.4s | 143 ms | 6.9 | 0.999 | 1.000 |
| FAISS 1.15 `IndexFlatL2` | exact, in memory | 0.0s | 7.9 ms | 125 | 0.999 | 1.000 |
| FAISS 1.15 HNSW M=16 ef=512 | approximate | 28.7s | 0.91 ms | 1,121 | 0.999 | 0.999 |
| hnswlib 0.8 M=16 ef=512 | approximate | 46.8s | 1.22 ms | 852 | 0.999 | 0.998 |

Exact recall@10 is 0.999 rather than 1.000 because the ground truth contains tied distances. The int8 column stores 130 bytes per vector instead of 512.

Scans read the whole table through SQLite's pager. Memory-mapping the database file avoids a `pread` system call per page and speeds up scans of large tables. Set the size to at least the database file size:

```sql
PRAGMA mmap_size = 4294967296; -- 4 GiB
```

With this setting, SIFT1M p50 latency drops to 243 ms for `vector_distance`, 282 ms for `vector_distance_q`, 64 ms for `vector_index`, and 24 ms for `vector_index(int8)`. The comparison table above uses default settings for every SQLite implementation.

## License

BSD-3-Clause

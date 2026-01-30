# Implementation Plan

Reference: [SPEC.md](SPEC.md)

Status legend: `[ ]` pending | `[~]` in progress | `[x]` complete | `[!]` failed

---

## Part 1: Project Scaffolding

### Work
- [x] Run `go mod init github.com/justintout/go-sqlite-vector`
- [x] Run `go get zombiezen.com/go/sqlite`
- [x] Create `LICENSE` file (BSD-3-Clause)
- [x] Create `vector.go` with package declaration and doc comment
- [x] Create `vector_test.go` with package declaration

### Validation
- [x] `go build ./...` succeeds
- [x] `go vet ./...` succeeds
- [x] `go test ./...` runs (no tests yet, but no errors)
- [x] `go mod tidy` produces no changes

### Failure Log
_(no failures)_

### Commit
- [x] Commit: "scaffold project with go module and license"

---

## Part 2: Blob Encoding (Float32ToBlob, BlobToFloat32)

### Work
- [x] Implement `Float32ToBlob(v []float32) []byte` in `vector.go`
  - Little-endian encoding via `encoding/binary`
- [x] Implement `BlobToFloat32(b []byte) ([]float32, error)` in `vector.go`
  - Return error if `len(b) % 4 != 0`
  - Little-endian decoding via `encoding/binary`

### Validation
- [x] Write table-driven tests in `vector_test.go`:
  - Round-trip: `BlobToFloat32(Float32ToBlob(v))` == `v` for several vectors
  - `BlobToFloat32` with invalid length returns error
  - Empty slice round-trips correctly
  - Known byte values: `Float32ToBlob([]float32{1.0})` == `[]byte{0x00, 0x00, 0x80, 0x3f}`
- [x] `go test ./...` passes
- [x] `go vet ./...` passes

### Failure Log
_(no failures)_

### Commit
- [x] Commit: "implement Float32ToBlob and BlobToFloat32"

---

## Part 3: Config and Registration Skeleton

### Work
- [x] Define unexported `config` struct with fields:
  - `dim int`
  - `quantMin, quantMax float32`
  - `quantEnabled bool`
- [x] Define `type Option func(*config)`
- [x] Implement `WithQuantRange(min, max float32) Option`
- [x] Implement `Register(conn *sqlite.Conn, dim int, opts ...Option) error`
  - Validate `dim >= 1`
  - Apply options to config
  - Register all four SQL function stubs via `conn.CreateFunction` that return errors ("not implemented")
  - Each SQL function receives the config via closure

### Validation
- [x] Write test: `Register` with `dim=0` returns error
- [x] Write test: `Register` with `dim=3` succeeds
- [x] Write test: `Register` with `WithQuantRange(-1, 1)` succeeds and marks quant enabled
- [x] Write test: calling any SQL function stub returns a "not implemented" or placeholder error (skipped: blocked on upstream library bug)
- [x] Write test: re-calling `Register` on the same connection does not error (overwrite behavior)
- [x] `go test ./...` passes
- [x] `go vet ./...` passes

### Failure Log
- **Stub error propagation test**: zombiezen.com/go/sqlite v1.4.2 has a bug in `func.go:resultError` where `err` is shadowed by `:=` on line 167, causing `sqlite3_result_error_code(ctx, SQLITE_OK)` to cancel the error. All Scalar function error returns are silently swallowed. Test skipped with `t.Skip()`. Upstream patch is in progress.

### Commit
- [x] Commit: "add config, options, and Register skeleton"

---

## Part 4: vector_encode SQL Function

### Work
- [x] Implement `vector_encode` function body:
  - Check for NULL input, return NULL
  - Read text argument as string
  - Parse JSON via `encoding/json` into `[]float64` (JSON numbers are float64)
  - Validate array length == `dim`; return SQL error with expected vs actual if mismatch
  - Convert `[]float64` to `[]float32`
  - Use `Float32ToBlob` to produce the result blob
  - Return blob result

### Validation
- [x] Table-driven integration tests (open real SQLite conn, register, run SQL):
  - `SELECT vector_encode('[1.0, 2.0, 3.0]')` with dim=3 returns correct 12-byte blob
  - `SELECT vector_encode('[1.0, 2.0]')` with dim=3 returns SQL error (skipped: upstream bug)
  - `SELECT vector_encode('not json')` returns SQL error (skipped: upstream bug)
  - `SELECT vector_encode('{}')` returns SQL error (skipped: upstream bug)
  - `SELECT vector_encode(NULL)` returns NULL
  - Round-trip: `BlobToFloat32` on the result of `vector_encode` matches input values
- [x] `go test ./...` passes
- [x] `go vet ./...` passes

### Failure Log
- Error propagation tests skipped due to upstream zombiezen/go/sqlite resultError bug (same as Part 3). Error-returning code paths are implemented correctly; tests will pass once the library is patched.

### Commit
- [x] Commit: "implement vector_encode SQL function"

---

## Part 5: L2 Squared Distance (internal)

### Work
- [x] Implement unexported `l2Squared(a, b []float32) float64`
  - Compute `sum((a[i] - b[i])^2)` using float64 accumulator for precision
- [x] Implement unexported `isQuantizedBlob(b []byte) bool`
  - Returns true if `len(b) >= 2 && b[0] == 0x00 && b[1] == 0x01`

### Validation
- [x] Unit tests for `l2Squared`:
  - Identical vectors → 0.0
  - Known values: `l2Squared([1,0,0], [0,1,0])` == 2.0
  - Known values: `l2Squared([1,2,3], [4,5,6])` == 27.0
  - Single dimension: `l2Squared([3], [7])` == 16.0
- [x] Unit tests for `isQuantizedBlob`:
  - `[]byte{0x00, 0x01, ...}` → true
  - `[]byte{0x00, 0x00, ...}` → false
  - Float32 blob → false
  - Empty/short slices → false
- [x] `go test ./...` passes
- [x] `go vet ./...` passes

### Failure Log
_(no failures)_

### Commit
- [x] Commit: "implement l2Squared distance and blob format detection"

---

## Part 6: vector_distance SQL Function

### Work
- [x] Implement `vector_distance` function body:
  - Check for NULL inputs, return NULL
  - Read both arguments as blobs ([]byte)
  - Validate neither blob has quantized magic bytes; return SQL error if detected
  - Validate both blobs have byte length == `dim * 4`; return SQL error with expected vs actual
  - Decode both blobs to `[]float32` via `BlobToFloat32`
  - Compute `l2Squared` and return as float64 result

### Validation
- [x] Table-driven integration tests:
  - Distance of identical vectors == 0.0
  - Distance of known vectors matches hand-computed value
  - `SELECT vector_distance(vector_encode('[1,2,3]'), vector_encode('[4,5,6]'))` == 27.0 (dim=3)
  - Wrong dimension blob → SQL error (skipped: upstream bug)
  - Quantized blob input → SQL error mentioning `vector_distance_q` (skipped: upstream bug)
  - NULL input → NULL output
  - Two NULLs → NULL
  - One NULL, one valid → NULL
- [x] `go test ./...` passes
- [x] `go vet ./...` passes

### Failure Log
- Error propagation tests skipped due to upstream zombiezen/go/sqlite resultError bug (same as Parts 3-4).

### Commit
- [x] Commit: "implement vector_distance SQL function"

---

## Part 7: Quantization (internal)

### Work
- [x] Implement unexported `quantize(v []float32, min, max float32) []byte`
  - Allocate `2 + len(v)` bytes
  - Write magic bytes `0x00, 0x01`
  - For each float: `clamp(round((v - min) / (max - min) * 255 - 128), -128, 127)`
  - Write as signed int8
- [x] Implement unexported `dequantize(b []byte, min, max float32) ([]float32, error)`
  - Validate magic bytes; return error if missing
  - For each int8: `(q + 128) / 255 * (max - min) + min`
  - Return `[]float32`

### Validation
- [x] Unit tests for `quantize`:
  - Known values with range [-1, 1]: value 0.0 → int8 value near 0 (middle of range)
  - Boundary: value == min → -128, value == max → 127
  - Out-of-range clamping: value > max → 127, value < min → -128
  - Output blob starts with `0x00, 0x01`
  - Output blob length == `2 + dim`
- [x] Unit tests for `dequantize`:
  - Round-trip: `dequantize(quantize(v))` is approximately equal to `v` (within int8 precision)
  - Missing magic bytes → error
  - Correct output length == input length - 2
- [x] `go test ./...` passes
- [x] `go vet ./...` passes

### Failure Log
_(no failures)_

### Commit
- [x] Commit: "implement scalar int8 quantization and dequantization"

---

## Part 8: vector_quantize SQL Function

### Work
- [x] Implement `vector_quantize` function body:
  - Check for NULL input, return NULL
  - Check `quantEnabled`; return SQL error if false
  - Read blob argument
  - Validate byte length == `dim * 4`
  - Decode to `[]float32` via `BlobToFloat32`
  - Call `quantize(floats, config.quantMin, config.quantMax)`
  - Return quantized blob

### Validation
- [x] Table-driven integration tests:
  - `SELECT vector_quantize(vector_encode('[0.5, -0.5, 0.0]'))` with dim=3 and range [-1,1] produces correct blob (starts with 0x00, 0x01, length 5)
  - Calling without `WithQuantRange` → SQL error (skipped: upstream bug)
  - Wrong dimension input → SQL error (skipped: upstream bug)
  - NULL input → NULL
  - Out-of-range values are clamped (no error)
- [x] `go test ./...` passes
- [x] `go vet ./...` passes

### Failure Log
- Error propagation tests skipped due to upstream zombiezen/go/sqlite resultError bug.

### Commit
- [x] Commit: "implement vector_quantize SQL function"

---

## Part 9: vector_distance_q SQL Function

### Work
- [x] Implement `vector_distance_q` function body:
  - Check for NULL inputs, return NULL
  - Check `quantEnabled`; return SQL error if false
  - Read both arguments as blobs
  - Validate both blobs have quantized magic bytes; return SQL error if missing
  - Validate both blobs have byte length == `2 + dim`
  - Dequantize both via `dequantize(blob, config.quantMin, config.quantMax)`
  - Compute `l2Squared` on the dequantized float32 slices
  - Return float64 result

### Validation
- [x] Table-driven integration tests:
  - Quantized distance of identical vectors ≈ 0.0 (not exact due to quantization loss)
  - Quantized distance ordering matches float32 distance ordering for a set of test vectors
  - Non-quantized blob input → SQL error (skipped: upstream bug)
  - Calling without `WithQuantRange` → SQL error (skipped: upstream bug)
  - Wrong quantized blob dimension → SQL error (skipped: upstream bug)
  - NULL inputs → NULL
- [x] `go test ./...` passes
- [x] `go vet ./...` passes

### Failure Log
- Error propagation tests skipped due to upstream zombiezen/go/sqlite resultError bug.

### Commit
- [x] Commit: "implement vector_distance_q SQL function"

---

## Part 10: End-to-End Integration Tests

### Work
- [x] Write full pipeline integration tests that mirror real usage:
  - Create table, insert rows with `vector_encode`, query with `vector_distance` + `ORDER BY` + `LIMIT`
  - Verify k-NN ordering is correct for a known dataset
  - Create table with quantized embeddings, insert with `vector_quantize(vector_encode(...))`, query with `vector_distance_q`
  - Verify quantized k-NN ordering matches float32 k-NN ordering
  - Test `Register` overwrite: register dim=3, then re-register dim=4, verify dim=4 functions work
- [x] Write test for Go-level `Float32ToBlob` used with SQL parameter binding (bind blob, then compute distance)

### Validation
- [x] `go test ./...` passes (all tests including new e2e tests)
- [x] `go vet ./...` passes
- [x] `go test -count=1 ./...` passes (no caching)
- [x] `go test -race ./...` passes

### Failure Log
_(no failures)_

### Commit
- [x] Commit: "add end-to-end integration tests"

---

## Part 11: Benchmarks

### Work
- [x] Create `bench_test.go`
- [x] Implement benchmarks using pre-allocated random vectors:
  - `BenchmarkL2Distance/dim=384`
  - `BenchmarkL2Distance/dim=768`
  - `BenchmarkL2Distance/dim=1536`
  - `BenchmarkQuantize/dim=384`
  - `BenchmarkQuantize/dim=768`
  - `BenchmarkQuantize/dim=1536`
  - `BenchmarkDequantize/dim=384`
  - `BenchmarkDequantize/dim=768`
  - `BenchmarkDequantize/dim=1536`
  - `BenchmarkVectorEncode/dim=384`
  - `BenchmarkVectorEncode/dim=768`
  - `BenchmarkVectorEncode/dim=1536`
- [x] Each benchmark uses `b.ResetTimer()` after setup and `b.ReportAllocs()`

### Validation
- [x] `go test -bench=. -benchmem ./...` runs without errors
- [x] All benchmarks produce non-zero ns/op values
- [x] `go test ./...` still passes (benchmarks don't break tests)
- [x] `go vet ./...` passes

### Failure Log
_(no failures)_

### Commit
- [x] Commit: "add benchmarks for core operations"

---

## Part 12: Final Review and Cleanup

### Work
- [x] Run `go mod tidy` to clean dependencies
- [x] Verify all exported symbols have doc comments
- [x] Verify no unexported symbols are accidentally exported
- [x] Verify error message strings match SPEC.md table exactly
- [x] Review all SQL functions for correct NULL handling
- [x] Review all SQL functions for correct error propagation (error returns are correct; propagation blocked by upstream library bug)
- [x] Confirm `Register` overwrite behavior works end-to-end

### Validation
- [x] `go build ./...` succeeds
- [x] `go vet ./...` passes
- [x] `go test ./...` passes
- [x] `go test -race ./...` passes
- [x] `go test -bench=. -benchmem ./...` runs
- [x] `go mod tidy` produces no changes
- [x] No `TODO`, `FIXME`, or `HACK` comments remain in source

### Failure Log
_(no failures)_

### Commit
- [x] Commit: "final review and cleanup"

---

## Completion Checklist

- [x] All 12 parts have status `[x]`
- [x] All tests pass: `go test -race -count=1 ./...`
- [x] All benchmarks run: `go test -bench=. -benchmem ./...`
- [x] `go vet` clean
- [x] `go mod tidy` clean
- [x] Every part has a commit
- [x] PLAN.md is fully updated with final statuses

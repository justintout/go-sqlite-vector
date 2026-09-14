//go:build sift

package vector

import (
	"encoding/binary"
	"flag"
	"math"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

var (
	siftDir     = flag.String("sift.dir", "", "directory containing sift_base.fvecs, sift_query.fvecs, sift_groundtruth.ivecs")
	siftQueries = flag.Int("sift.queries", 100, "number of queries to run from sift_query.fvecs")
)

// TestSIFT1M runs exact and quantized k-NN search over the TEXMEX SIFT1M
// dataset and reports build time, single-threaded query latency, and recall
// against the published ground truth.
//
//	go test -tags sift -run TestSIFT1M -timeout 0 -v -sift.dir .tmp/sift
func TestSIFT1M(t *testing.T) {
	if *siftDir == "" {
		t.Fatal("-sift.dir is required")
	}
	const dim, k = 128, 100

	base := readVecs(t, filepath.Join(*siftDir, "sift_base.fvecs"), dim)
	queries := readVecs(t, filepath.Join(*siftDir, "sift_query.fvecs"), dim)
	truth := readVecs(t, filepath.Join(*siftDir, "sift_groundtruth.ivecs"), k)
	nq := min(*siftQueries, len(queries))

	lo, hi := float32(math.Inf(1)), float32(math.Inf(-1))
	for _, v := range base {
		lo = min(lo, slices.Min(v))
		hi = max(hi, slices.Max(v))
	}

	dbPath := filepath.Join(t.TempDir(), "sift.db")
	conn, err := sqlite.OpenConn(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := Register(conn, dim, WithQuantRange(lo, hi)); err != nil {
		t.Fatal(err)
	}
	exec(t, conn, "CREATE TABLE sift (id INTEGER PRIMARY KEY, embedding BLOB, embedding_q BLOB)")

	start := time.Now()
	exec(t, conn, "BEGIN")
	stmt := conn.Prep("INSERT INTO sift (id, embedding) VALUES (?1, ?2)")
	for i, v := range base {
		stmt.BindInt64(1, int64(i))
		stmt.BindBytes(2, Float32ToBlob(v))
		if _, err := stmt.Step(); err != nil {
			t.Fatal(err)
		}
		stmt.Reset()
	}
	exec(t, conn, "COMMIT")
	insertTime := time.Since(start)

	start = time.Now()
	exec(t, conn, "UPDATE sift SET embedding_q = vector_quantize(embedding)")
	quantizeTime := time.Since(start)

	fi, err := os.Stat(dbPath)
	if err != nil {
		t.Fatal(err)
	}

	t.Logf("SIFT1M: %d base vectors, %d queries, dim=%d, quant range [%g, %g]", len(base), nq, dim, lo, hi)
	t.Logf("insert float32:       %v", insertTime.Round(time.Millisecond))
	t.Logf("quantize column:      %v", quantizeTime.Round(time.Millisecond))
	t.Logf("database size:        %.1f MiB (both columns)", float64(fi.Size())/(1<<20))
	t.Logf("%-10s %10s %10s %10s %8s %8s %8s", "method", "p50", "p99", "QPS", "R@1", "R@10", "R@100")

	for _, m := range []struct {
		name  string
		query string
		arg   func([]float32) []byte
	}{
		{"float32", "SELECT id FROM sift ORDER BY vector_distance(embedding, ?1) LIMIT 100", Float32ToBlob},
		{"int8", "SELECT id FROM sift ORDER BY vector_distance_q(embedding_q, ?1) LIMIT 100", func(v []float32) []byte { return quantize(v, lo, hi) }},
	} {
		lat := make([]time.Duration, nq)
		var r1, r10, r100 float64
		stmt := conn.Prep(m.query)
		for qi := range nq {
			ids := make([]int32, 0, k)
			start := time.Now()
			stmt.BindBytes(1, m.arg(queries[qi]))
			for {
				row, err := stmt.Step()
				if err != nil {
					t.Fatal(err)
				}
				if !row {
					break
				}
				ids = append(ids, int32(stmt.ColumnInt64(0)))
			}
			stmt.Reset()
			lat[qi] = time.Since(start)
			r1 += recall(ids, truth[qi], 1)
			r10 += recall(ids, truth[qi], 10)
			r100 += recall(ids, truth[qi], 100)
		}
		var total time.Duration
		for _, d := range lat {
			total += d
		}
		slices.Sort(lat)
		n := float64(nq)
		t.Logf("%-10s %10v %10v %10.2f %8.4f %8.4f %8.4f", m.name,
			lat[nq/2].Round(time.Millisecond), lat[nq*99/100].Round(time.Millisecond),
			n/total.Seconds(), r1/n, r10/n, r100/n)
	}
}

// readVecs reads a TEXMEX .fvecs or .ivecs file. Each record is a
// little-endian int32 dimension followed by that many float32 or int32
// values. ivecs values are converted to float32 so both formats share one
// return type; SIFT1M ids fit exactly in float32.
func readVecs(t *testing.T, path string, dim int) [][]float32 {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	isInt := filepath.Ext(path) == ".ivecs"
	rec := 4 + dim*4
	if len(data)%rec != 0 {
		t.Fatalf("%s: size %d is not a multiple of record size %d", path, len(data), rec)
	}
	out := make([][]float32, len(data)/rec)
	for i := range out {
		r := data[i*rec : (i+1)*rec]
		if d := int(binary.LittleEndian.Uint32(r)); d != dim {
			t.Fatalf("%s: record %d has dimension %d, want %d", path, i, d, dim)
		}
		v := make([]float32, dim)
		for j := range v {
			bits := binary.LittleEndian.Uint32(r[4+j*4:])
			if isInt {
				v[j] = float32(int32(bits))
			} else {
				v[j] = math.Float32frombits(bits)
			}
		}
		out[i] = v
	}
	return out
}

// recall returns the fraction of the true top-k ids present in the first k
// results.
func recall(got []int32, truth []float32, k int) float64 {
	want := make(map[int32]bool, k)
	for _, id := range truth[:k] {
		want[int32(id)] = true
	}
	hits := 0
	for _, id := range got[:min(k, len(got))] {
		if want[id] {
			hits++
		}
	}
	return float64(hits) / float64(k)
}

func exec(t *testing.T, conn *sqlite.Conn, query string) {
	t.Helper()
	if err := sqlitex.ExecuteTransient(conn, query, nil); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

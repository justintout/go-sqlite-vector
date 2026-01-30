package vector

import (
	"bytes"
	"io"
	"testing"

	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

func openTestConn(t *testing.T) *sqlite.Conn {
	t.Helper()
	conn, err := sqlite.OpenConn(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func TestFloat32ToBlob(t *testing.T) {
	tests := []struct {
		name  string
		input []float32
		want  []byte
	}{
		{
			name:  "single 1.0",
			input: []float32{1.0},
			want:  []byte{0x00, 0x00, 0x80, 0x3f},
		},
		{
			name:  "empty",
			input: []float32{},
			want:  []byte{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Float32ToBlob(tt.input)
			if !bytes.Equal(got, tt.want) {
				t.Errorf("Float32ToBlob(%v) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestBlobToFloat32(t *testing.T) {
	tests := []struct {
		name    string
		input   []byte
		want    []float32
		wantErr bool
	}{
		{
			name:  "single 1.0",
			input: []byte{0x00, 0x00, 0x80, 0x3f},
			want:  []float32{1.0},
		},
		{
			name:  "empty",
			input: []byte{},
			want:  []float32{},
		},
		{
			name:    "invalid length 3 bytes",
			input:   []byte{0x00, 0x00, 0x80},
			wantErr: true,
		},
		{
			name:    "invalid length 5 bytes",
			input:   []byte{0x00, 0x00, 0x80, 0x3f, 0x01},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := BlobToFloat32(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("BlobToFloat32() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if len(got) != len(tt.want) {
				t.Fatalf("BlobToFloat32() length = %d, want %d", len(got), len(tt.want))
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("BlobToFloat32()[%d] = %v, want %v", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestBlobRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		vec  []float32
	}{
		{name: "3d vector", vec: []float32{0.1, 0.2, 0.3}},
		{name: "negative values", vec: []float32{-1.0, 0.0, 1.0}},
		{name: "large values", vec: []float32{1e10, -1e10, 3.14159}},
		{name: "single element", vec: []float32{42.0}},
		{name: "empty", vec: []float32{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			blob := Float32ToBlob(tt.vec)
			got, err := BlobToFloat32(blob)
			if err != nil {
				t.Fatalf("BlobToFloat32(Float32ToBlob(%v)) error: %v", tt.vec, err)
			}
			if len(got) != len(tt.vec) {
				t.Fatalf("round-trip length = %d, want %d", len(got), len(tt.vec))
			}
			for i := range got {
				if got[i] != tt.vec[i] {
					t.Errorf("round-trip[%d] = %v, want %v", i, got[i], tt.vec[i])
				}
			}
		})
	}
}

func TestRegister(t *testing.T) {
	t.Run("dim 0 returns error", func(t *testing.T) {
		conn := openTestConn(t)
		err := Register(conn, 0)
		if err == nil {
			t.Fatal("expected error for dim=0, got nil")
		}
	})

	t.Run("dim 3 succeeds", func(t *testing.T) {
		conn := openTestConn(t)
		err := Register(conn, 3)
		if err != nil {
			t.Fatalf("Register(dim=3) error: %v", err)
		}
	})

	t.Run("with WithQuantRange succeeds", func(t *testing.T) {
		conn := openTestConn(t)
		err := Register(conn, 3, WithQuantRange(-1, 1))
		if err != nil {
			t.Fatalf("Register with WithQuantRange error: %v", err)
		}
	})

	t.Run("stub functions return error", func(t *testing.T) {
		t.Skip("blocked on zombiezen/go/sqlite fix: resultError shadows err variable, preventing SQL error propagation")
		conn := openTestConn(t)
		if err := Register(conn, 3); err != nil {
			t.Fatal(err)
		}
		err := sqlitex.ExecuteTransient(conn, "SELECT vector_encode('[1,2,3]')", nil)
		if err == nil {
			t.Fatal("expected stub error from vector_encode, got nil")
		}
	})

	t.Run("re-register overwrites without error", func(t *testing.T) {
		conn := openTestConn(t)
		if err := Register(conn, 3); err != nil {
			t.Fatal(err)
		}
		if err := Register(conn, 4); err != nil {
			t.Fatalf("second Register call error: %v", err)
		}
	})
}

func TestVectorEncode(t *testing.T) {
	t.Run("valid 3d vector", func(t *testing.T) {
		conn := openTestConn(t)
		if err := Register(conn, 3); err != nil {
			t.Fatal(err)
		}
		var blob []byte
		err := sqlitex.ExecuteTransient(conn, "SELECT vector_encode('[1.0, 2.0, 3.0]')", &sqlitex.ExecOptions{
			ResultFunc: func(stmt *sqlite.Stmt) error {
				r := stmt.ColumnReader(0)
				b, err := io.ReadAll(r)
				if err != nil {
					return err
				}
				blob = b
				return nil
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(blob) != 12 {
			t.Fatalf("blob length = %d, want 12", len(blob))
		}
		floats, err := BlobToFloat32(blob)
		if err != nil {
			t.Fatal(err)
		}
		want := []float32{1.0, 2.0, 3.0}
		for i := range floats {
			if floats[i] != want[i] {
				t.Errorf("floats[%d] = %v, want %v", i, floats[i], want[i])
			}
		}
	})

	t.Run("dimension mismatch", func(t *testing.T) {
		t.Skip("blocked on zombiezen/go/sqlite fix: resultError shadows err variable")
		conn := openTestConn(t)
		if err := Register(conn, 3); err != nil {
			t.Fatal(err)
		}
		err := sqlitex.ExecuteTransient(conn, "SELECT vector_encode('[1.0, 2.0]')", nil)
		if err == nil {
			t.Fatal("expected error for dimension mismatch, got nil")
		}
	})

	t.Run("invalid JSON", func(t *testing.T) {
		t.Skip("blocked on zombiezen/go/sqlite fix: resultError shadows err variable")
		conn := openTestConn(t)
		if err := Register(conn, 3); err != nil {
			t.Fatal(err)
		}
		err := sqlitex.ExecuteTransient(conn, "SELECT vector_encode('not json')", nil)
		if err == nil {
			t.Fatal("expected error for invalid JSON, got nil")
		}
	})

	t.Run("JSON object not array", func(t *testing.T) {
		t.Skip("blocked on zombiezen/go/sqlite fix: resultError shadows err variable")
		conn := openTestConn(t)
		if err := Register(conn, 3); err != nil {
			t.Fatal(err)
		}
		err := sqlitex.ExecuteTransient(conn, `SELECT vector_encode('{}')`, nil)
		if err == nil {
			t.Fatal("expected error for non-array JSON, got nil")
		}
	})

	t.Run("NULL input returns NULL", func(t *testing.T) {
		conn := openTestConn(t)
		if err := Register(conn, 3); err != nil {
			t.Fatal(err)
		}
		var isNull bool
		err := sqlitex.ExecuteTransient(conn, "SELECT vector_encode(NULL)", &sqlitex.ExecOptions{
			ResultFunc: func(stmt *sqlite.Stmt) error {
				isNull = stmt.ColumnType(0) == sqlite.TypeNull
				return nil
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		if !isNull {
			t.Fatal("expected NULL result for NULL input")
		}
	})

	t.Run("round-trip with BlobToFloat32", func(t *testing.T) {
		conn := openTestConn(t)
		if err := Register(conn, 3); err != nil {
			t.Fatal(err)
		}
		var blob []byte
		err := sqlitex.ExecuteTransient(conn, "SELECT vector_encode('[0.1, 0.2, 0.3]')", &sqlitex.ExecOptions{
			ResultFunc: func(stmt *sqlite.Stmt) error {
				r := stmt.ColumnReader(0)
				b, err := io.ReadAll(r)
				if err != nil {
					return err
				}
				blob = b
				return nil
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		floats, err := BlobToFloat32(blob)
		if err != nil {
			t.Fatal(err)
		}
		want := []float32{0.1, 0.2, 0.3}
		for i := range floats {
			if floats[i] != want[i] {
				t.Errorf("round-trip[%d] = %v, want %v", i, floats[i], want[i])
			}
		}
	})
}

func TestL2Squared(t *testing.T) {
	tests := []struct {
		name string
		a, b []float32
		want float64
	}{
		{
			name: "identical vectors",
			a:    []float32{1, 2, 3},
			b:    []float32{1, 2, 3},
			want: 0.0,
		},
		{
			name: "unit vectors",
			a:    []float32{1, 0, 0},
			b:    []float32{0, 1, 0},
			want: 2.0,
		},
		{
			name: "known values 1-2-3 vs 4-5-6",
			a:    []float32{1, 2, 3},
			b:    []float32{4, 5, 6},
			want: 27.0,
		},
		{
			name: "single dimension",
			a:    []float32{3},
			b:    []float32{7},
			want: 16.0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := l2Squared(tt.a, tt.b)
			if got != tt.want {
				t.Errorf("l2Squared(%v, %v) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

func TestIsQuantizedBlob(t *testing.T) {
	tests := []struct {
		name string
		b    []byte
		want bool
	}{
		{
			name: "quantized blob",
			b:    []byte{0x00, 0x01, 0x7f, 0x80},
			want: true,
		},
		{
			name: "wrong version byte",
			b:    []byte{0x00, 0x00, 0x7f},
			want: false,
		},
		{
			name: "float32 blob",
			b:    Float32ToBlob([]float32{1.0}),
			want: false,
		},
		{
			name: "empty",
			b:    []byte{},
			want: false,
		},
		{
			name: "single byte",
			b:    []byte{0x00},
			want: false,
		},
		{
			name: "just magic bytes",
			b:    []byte{0x00, 0x01},
			want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isQuantizedBlob(tt.b)
			if got != tt.want {
				t.Errorf("isQuantizedBlob(%v) = %v, want %v", tt.b, got, tt.want)
			}
		})
	}
}

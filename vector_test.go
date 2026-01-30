package vector

import (
	"bytes"
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

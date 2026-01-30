package vector

import (
	"encoding/json"
	"math/rand"
	"testing"
)

func randomFloat32s(n int) []float32 {
	v := make([]float32, n)
	for i := range v {
		v[i] = rand.Float32()*2 - 1 // range [-1, 1]
	}
	return v
}

func BenchmarkL2Distance(b *testing.B) {
	for _, dim := range []int{384, 768, 1536} {
		a := randomFloat32s(dim)
		c := randomFloat32s(dim)
		b.Run("dim="+itoa(dim), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				l2Squared(a, c)
			}
		})
	}
}

func BenchmarkQuantize(b *testing.B) {
	for _, dim := range []int{384, 768, 1536} {
		v := randomFloat32s(dim)
		b.Run("dim="+itoa(dim), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				quantize(v, -1.0, 1.0)
			}
		})
	}
}

func BenchmarkDequantize(b *testing.B) {
	for _, dim := range []int{384, 768, 1536} {
		v := randomFloat32s(dim)
		qblob := quantize(v, -1.0, 1.0)
		b.Run("dim="+itoa(dim), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				dequantize(qblob, -1.0, 1.0)
			}
		})
	}
}

func BenchmarkVectorEncode(b *testing.B) {
	for _, dim := range []int{384, 768, 1536} {
		v := randomFloat32s(dim)
		// Build JSON array
		f64 := make([]float64, dim)
		for i, f := range v {
			f64[i] = float64(f)
		}
		jsonBytes, _ := json.Marshal(f64)
		jsonStr := string(jsonBytes)

		b.Run("dim="+itoa(dim), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				var nums []float64
				json.Unmarshal([]byte(jsonStr), &nums)
				floats := make([]float32, len(nums))
				for j, n := range nums {
					floats[j] = float32(n)
				}
				Float32ToBlob(floats)
			}
		})
	}
}

func itoa(n int) string {
	switch n {
	case 384:
		return "384"
	case 768:
		return "768"
	case 1536:
		return "1536"
	default:
		return "?"
	}
}

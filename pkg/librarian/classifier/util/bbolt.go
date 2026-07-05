package util

import (
	"encoding/binary"
	"fmt"
	"math"
	"strconv"
	"strings"

	bolt "go.etcd.io/bbolt"
)

func bucketName(model string, dimensions int) []byte {
	return []byte("vectors:" + model + ":" + strconv.Itoa(dimensions))
}

func encodeVector(v []float64) []byte {
	buf := make([]byte, len(v)*8)
	for i, f := range v {
		binary.LittleEndian.PutUint64(buf[i*8:], math.Float64bits(f))
	}
	return buf
}

func decodeVector(b []byte) []float64 {
	n := len(b) / 8
	v := make([]float64, n)
	for i := range v {
		v[i] = math.Float64frombits(binary.LittleEndian.Uint64(b[i*8:]))
	}
	return v
}

type BboltWriter struct {
	db         *bolt.DB
	model      string
	dimensions int
	bucket     []byte
}

func NewBboltWriter(db *bolt.DB, model string, dimensions int) *BboltWriter {
	return &BboltWriter{
		db:         db,
		model:      model,
		dimensions: dimensions,
		bucket:     bucketName(model, dimensions),
	}
}

func (w *BboltWriter) Model() string   { return w.model }
func (w *BboltWriter) Dimensions() int { return w.dimensions }

func (w *BboltWriter) Write(key string, vector []float64) error {
	if len(vector) != w.dimensions {
		return fmt.Errorf("dimension mismatch: got %d, want %d", len(vector), w.dimensions)
	}
	return w.db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(w.bucket)
		if err != nil {
			return err
		}
		return b.Put([]byte(key), encodeVector(vector))
	})
}

type BboltIterator struct {
	model      string
	dimensions int
	entries    []KeyedVector
	pos        int
}

func NewBboltIterator(db *bolt.DB, model string, dimensions int) *BboltIterator {
	it := &BboltIterator{
		model:      model,
		dimensions: dimensions,
	}
	db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketName(model, dimensions))
		if b == nil {
			return nil
		}
		c := b.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			it.entries = append(it.entries, KeyedVector{
				Key:    string(k),
				Vector: decodeVector(v),
			})
		}
		return nil
	})
	return it
}

func (it *BboltIterator) Model() string   { return it.model }
func (it *BboltIterator) Dimensions() int { return it.dimensions }

func (it *BboltIterator) Read() (*KeyedVector, error) {
	if it.pos >= len(it.entries) {
		return nil, ErrIteratorExhausted
	}
	kv := &it.entries[it.pos]
	it.pos++
	return kv, nil
}

// ParseBucketName extracts model and dimensions from a bucket name.
func ParseBucketName(name string) (model string, dimensions int, ok bool) {
	const prefix = "vectors:"
	if !strings.HasPrefix(name, prefix) {
		return "", 0, false
	}
	rest := name[len(prefix):]
	idx := strings.LastIndex(rest, ":")
	if idx < 0 {
		return "", 0, false
	}
	model = rest[:idx]
	d, err := strconv.Atoi(rest[idx+1:])
	if err != nil {
		return "", 0, false
	}
	return model, d, true
}

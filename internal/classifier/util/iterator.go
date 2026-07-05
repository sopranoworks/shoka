package util

import (
	"errors"
	"fmt"
	"sync"
)

var ErrIteratorExhausted = errors.New("iterator exhausted")

type KeyedVector struct {
	Key    string
	Vector []float64
}

type Iterator interface {
	Model() string
	Dimensions() int
	Read() (*KeyedVector, error)
}

type Writer interface {
	Model() string
	Dimensions() int
	Write(key string, vector []float64) error
}

type MemoryStore struct {
	mu         sync.Mutex
	model      string
	dimensions int
	entries    []KeyedVector
}

func NewMemoryStore(model string, dimensions int) *MemoryStore {
	return &MemoryStore{
		model:      model,
		dimensions: dimensions,
	}
}

func (s *MemoryStore) Model() string    { return s.model }
func (s *MemoryStore) Dimensions() int  { return s.dimensions }

func (s *MemoryStore) Write(key string, vector []float64) error {
	if len(vector) != s.dimensions {
		return fmt.Errorf("dimension mismatch: got %d, want %d", len(vector), s.dimensions)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = append(s.entries, KeyedVector{Key: key, Vector: vector})
	return nil
}

func (s *MemoryStore) Iterator() Iterator {
	s.mu.Lock()
	defer s.mu.Unlock()
	snapshot := make([]KeyedVector, len(s.entries))
	copy(snapshot, s.entries)
	return &memoryIterator{
		model:      s.model,
		dimensions: s.dimensions,
		entries:    snapshot,
	}
}

type memoryIterator struct {
	model      string
	dimensions int
	entries    []KeyedVector
	pos        int
}

func (it *memoryIterator) Model() string    { return it.model }
func (it *memoryIterator) Dimensions() int  { return it.dimensions }

func (it *memoryIterator) Read() (*KeyedVector, error) {
	if it.pos >= len(it.entries) {
		return nil, ErrIteratorExhausted
	}
	kv := &it.entries[it.pos]
	it.pos++
	return kv, nil
}
